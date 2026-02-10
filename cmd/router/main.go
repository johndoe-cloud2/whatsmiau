// Router: single entrypoint that validates API key (header apikey) and proxies requests to the correct backend using Redis (route:<id> and backends set).
package main

import (
	"context"
	"crypto/tls"
	"net/http"
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
	// Allow unauthenticated health checks (ALB target group hits GET /)
	if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/health") {
		w.WriteHeader(http.StatusOK)
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
		if err != nil || len(urls) == 0 {
			zap.L().Warn("no backends available", zap.Error(err))
			http.Error(w, "No backend available", http.StatusServiceUnavailable)
			return
		}
		backendURL = urls[0]
	}

	target, err := url.Parse(backendURL)
	if err != nil {
		zap.L().Error("invalid backend URL", zap.String("url", backendURL), zap.Error(err))
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
