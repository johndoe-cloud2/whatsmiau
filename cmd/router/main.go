// Router: single entrypoint that validates API key (header apikey) and proxies requests to the correct backend using Redis (route:<id> and backends set).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const redisKeyBackends = "backends"
const redisKeyRoutePrefix = "route:"

type config struct {
	Port          string `env:"PORT" envDefault:"8080"`
	RedisURL      string `env:"REDIS_URL" envDefault:"localhost:6379"`
	RedisPassword string `env:"REDIS_PASSWORD"`
	RedisTLS      bool   `env:"REDIS_TLS" envDefault:"false"`
	APIKey        string `env:"API_KEY" envDefault:""`
}

func main() {
	_ = godotenv.Load(".env")
	var cfg config
	if err := env.Parse(&cfg); err != nil {
		panic(err)
	}

	logCfg := zap.NewProductionConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ := logCfg.Build()
	defer logger.Sync()
	zap.ReplaceGlobals(logger)

	opts := &redis.Options{Addr: cfg.RedisURL, Password: cfg.RedisPassword}
	if cfg.RedisTLS {
		opts.TLSConfig = &tls.Config{}
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		zap.L().Fatal("redis ping failed", zap.Error(err))
	}
	defer rdb.Close()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Set per-request in ServeHTTP via closure
		},
		Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 10 * time.Second}
				return d.DialContext(ctx, network, addr)
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			zap.L().Error("proxy to backend failed", zap.String("path", r.URL.Path), zap.String("host", r.Host), zap.Error(err))
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	handler := &routerHandler{
		redis:  rdb,
		proxy:  proxy,
		apiKey: cfg.APIKey,
	}

	addr := ":" + cfg.Port
	zap.L().Info("router listening", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler); err != nil {
		zap.L().Fatal("listen failed", zap.Error(err))
	}
}

const apikeyHeader = "apikey"

type routerHandler struct {
	redis  *redis.Client
	proxy  *httputil.ReverseProxy
	apiKey string
}

func (h *routerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Allow unauthenticated health checks (ALB and GET/HEAD /, /health)
	norm := strings.TrimSuffix(strings.TrimSpace(r.URL.Path), "/")
	if norm == "" {
		norm = "/"
	}
	healthMethod := r.Method == http.MethodGet || r.Method == http.MethodHead
	if healthMethod && (norm == "/" || norm == "/health") {
		w.WriteHeader(http.StatusOK)
		if norm == "/health" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		}
		return
	}
	if key := strings.TrimSpace(h.apiKey); key != "" {
		if r.Header.Get(apikeyHeader) != key {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	path := r.URL.Path
	instanceID := extractInstanceID(path)

	var backendURL string
	if instanceID != "" {
		backendURL, _ = h.redis.Get(ctx, redisKeyRoutePrefix+instanceID).Result()
		if backendURL == "" {
			zap.L().Warn("no route for instance", zap.String("instance", instanceID))
			http.Error(w, "No backend for this instance", http.StatusServiceUnavailable)
			return
		}
	} else {
		urls, err := h.redis.SMembers(ctx, redisKeyBackends).Result()
		if err != nil {
			zap.L().Warn("no backends available", zap.Error(err))
			http.Error(w, "No backend available", http.StatusServiceUnavailable)
			return
		}
		if len(urls) == 0 {
			zap.L().Warn("no backends available", zap.Int("backends_count", 0))
			http.Error(w, "No backend available", http.StatusServiceUnavailable)
			return
		}
		zap.L().Info("proxying to backends", zap.String("path", path), zap.Int("backends_count", len(urls)))
		backendURL = urls[0]
	}

	target, err := url.Parse(backendURL)
	if err != nil {
		zap.L().Error("invalid backend URL", zap.String("url", backendURL), zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// For requests without instance ID we may have multiple backends; try each until one responds (avoids 502 when one backend is down).
	// Use a fresh context per attempt so client timeout/disconnect doesn't cancel all retries ("context canceled").
	if instanceID == "" {
		urls, _ := h.redis.SMembers(ctx, redisKeyBackends).Result()
		var bodyBuf []byte
		if r.Body != nil {
			bodyBuf, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
		// Per-backend timeout long enough for slow ops (e.g. create instance + QR); max tries so we stay under ALB idle.
		const perBackendTimeout = 90 * time.Second
		const maxBackendTries = 2
		for i, u := range urls {
			if i >= maxBackendTries {
				break
			}
			t, err := url.Parse(u)
			if err != nil {
				continue
			}
			tryCtx, tryCancel := context.WithTimeout(context.Background(), perBackendTimeout)
			defer tryCancel()
			reqTry := r.Clone(tryCtx)
			if len(bodyBuf) > 0 {
				reqTry.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			}
			rec := httptest.NewRecorder()
			h.proxy.Director = func(req *http.Request) {
				req.URL.Scheme = t.Scheme
				req.URL.Host = t.Host
				req.URL.Path = path
				req.URL.RawPath = ""
				req.Host = t.Host
			}
			h.proxy.ServeHTTP(rec, reqTry)
			if rec.Code != http.StatusBadGateway && rec.Code != 0 {
				zap.L().Info("backend responded", zap.String("backend", u), zap.Int("code", rec.Code))
				for k, v := range rec.Header() {
					for _, vv := range v {
						w.Header().Add(k, vv)
					}
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
				return
			}
			// Backend unreachable (stale task IP); remove from Redis so we don't keep trying it.
			if err := h.redis.SRem(ctx, redisKeyBackends, u).Err(); err != nil {
				zap.L().Warn("failed to remove dead backend from Redis", zap.String("backend", u), zap.Error(err))
			} else {
				zap.L().Info("removed dead backend from Redis", zap.String("backend", u))
			}
			zap.L().Warn("backend failed, trying next", zap.String("backend", u), zap.Int("code", rec.Code))
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	h.proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = path
		req.URL.RawPath = ""
		req.Host = target.Host
	}

	h.proxy.ServeHTTP(w, r)
}

func extractInstanceID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	if parts[0] != "v1" {
		return ""
	}
	switch parts[1] {
	case "instance":
		if len(parts) >= 3 {
			return parts[2]
		}
		return ""
	case "message", "chat":
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
		return ""
	default:
		return ""
	}
}
