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

// Per-backend timeout long enough for slow ops (e.g. create instance + QR); max tries so we stay under ALB idle (180s).
const perBackendTimeout = 90 * time.Second
const maxBackendTries = 2

// During a deploy the old task unregisters from Redis before the new one finishes starting, so for a short
// window there are no backends at all. Waiting turns what used to be an instant 502/503 into a slower success.
const backendWaitBudget = 45 * time.Second
const backendWaitInterval = 1 * time.Second

// Internal marker so we can tell "the proxy could not reach the backend" apart from a real 502 reply. Never sent to clients.
const headerProxyError = "X-Whatsmiau-Router-Proxy-Error"

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

	handler := &routerHandler{
		redis: rdb,
		// Shared transport (connection pool); each attempt builds its own ReverseProxy on top so
		// concurrent requests never share a Director (the old single proxy raced on it).
		transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 10 * time.Second}
				return d.DialContext(ctx, network, addr)
			},
		},
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
	redis     *redis.Client
	transport http.RoundTripper
	apiKey    string
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

	path := r.URL.Path

	// Buffer the body so an attempt against a dead backend can be retried against a live one.
	var bodyBuf []byte
	if r.Body != nil {
		bodyBuf, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	if instanceID := extractInstanceID(path); instanceID != "" {
		h.serveInstance(w, r, instanceID, path, bodyBuf)
		return
	}
	h.serveAny(w, r, path, bodyBuf)
}

// serveInstance proxies a request for a specific instance. If the routed backend is unreachable
// (stale task IP after a deploy), it drops the stale route and retries against a live backend
// instead of failing the request.
func (h *routerHandler) serveInstance(w http.ResponseWriter, r *http.Request, instanceID, path string, body []byte) {
	tried := make(map[string]bool)
	for attempt := 0; attempt < maxBackendTries; attempt++ {
		backendURL := h.resolveBackend(r.Context(), instanceID, tried)
		if backendURL == "" {
			zap.L().Warn("no route for instance and no backends", zap.String("instance", instanceID))
			http.Error(w, "No backend for this instance", http.StatusServiceUnavailable)
			return
		}
		tried[backendURL] = true

		target, err := url.Parse(backendURL)
		if err != nil {
			zap.L().Error("invalid backend URL", zap.String("url", backendURL), zap.Error(err))
			_ = h.redis.Del(r.Context(), redisKeyRoutePrefix+instanceID).Err()
			continue
		}

		zap.L().Debug("routing instance to backend", zap.String("instance", instanceID), zap.String("backend", backendURL))
		rec := h.proxyOnce(target, r, path, body)
		if !isUnreachable(rec) {
			writeRecorded(w, rec)
			return
		}

		// Unreachable: stale task IP. Drop the route (and the backend entry, if listed) so nothing else lands here.
		_ = h.redis.Del(r.Context(), redisKeyRoutePrefix+instanceID).Err()
		_ = h.redis.SRem(r.Context(), redisKeyBackends, backendURL).Err()
		zap.L().Info("deleted stale route after 502, retrying on live backend",
			zap.String("instance", instanceID), zap.String("backend", backendURL))
	}
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

// serveAny proxies a request without an instance ID (create/list); any live backend can take it.
func (h *routerHandler) serveAny(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	urls := h.waitForBackends(r.Context(), nil)
	if len(urls) == 0 {
		zap.L().Warn("no backends available", zap.Int("backends_count", 0))
		http.Error(w, "No backend available", http.StatusServiceUnavailable)
		return
	}
	zap.L().Info("proxying to backends", zap.String("path", path), zap.Int("backends_count", len(urls)))

	for i, u := range urls {
		if i >= maxBackendTries {
			break
		}
		target, err := url.Parse(u)
		if err != nil {
			continue
		}
		rec := h.proxyOnce(target, r, path, body)
		if !isUnreachable(rec) {
			zap.L().Info("backend responded", zap.String("backend", u), zap.Int("code", rec.Code))
			writeRecorded(w, rec)
			return
		}
		// Backend unreachable (stale task IP); remove from Redis so we don't keep trying it.
		if err := h.redis.SRem(r.Context(), redisKeyBackends, u).Err(); err != nil {
			zap.L().Warn("failed to remove dead backend from Redis", zap.String("backend", u), zap.Error(err))
		} else {
			zap.L().Info("removed dead backend from Redis", zap.String("backend", u))
		}
		zap.L().Warn("backend failed, trying next", zap.String("backend", u))
	}
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

// resolveBackend returns the backend for an instance: its route if set (and not already tried),
// otherwise any live backend from the backends set, which it then sticks as the new route so
// subsequent requests (e.g. Connect polls) hit the same backend.
func (h *routerHandler) resolveBackend(ctx context.Context, instanceID string, tried map[string]bool) string {
	if backendURL, _ := h.redis.Get(ctx, redisKeyRoutePrefix+instanceID).Result(); backendURL != "" && !tried[backendURL] {
		return backendURL
	}

	for _, u := range h.waitForBackends(ctx, tried) {
		zap.L().Info("no route for instance, using fallback backend", zap.String("instance", instanceID), zap.String("backend", u))
		_ = h.redis.Set(ctx, redisKeyRoutePrefix+instanceID, u, 0).Err()
		return u
	}
	return ""
}

// waitForBackends polls the backends set until at least one URL outside `tried` appears, the wait
// budget elapses, or the client goes away. Covers the deploy window where no backend is registered yet.
func (h *routerHandler) waitForBackends(ctx context.Context, tried map[string]bool) []string {
	deadline := time.Now().Add(backendWaitBudget)
	waited := false
	for {
		urls, err := h.redis.SMembers(ctx, redisKeyBackends).Result()
		if err == nil {
			candidates := urls[:0]
			for _, u := range urls {
				if !tried[u] {
					candidates = append(candidates, u)
				}
			}
			if len(candidates) > 0 {
				if waited {
					zap.L().Info("backend appeared after waiting", zap.Int("backends_count", len(candidates)))
				}
				return candidates
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		if !waited {
			zap.L().Warn("no backends available, waiting for one to register", zap.Duration("budget", backendWaitBudget))
			waited = true
		}
		time.Sleep(backendWaitInterval)
	}
}

// proxyOnce sends the request to one backend with a fresh context (so client/ALB timeouts don't cancel
// retries) and records the response. Unreachable backends yield a 502 marked with headerProxyError.
func (h *routerHandler) proxyOnce(target *url.URL, r *http.Request, path string, body []byte) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), perBackendTimeout)
	defer cancel()

	req := r.Clone(ctx)
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	rec := httptest.NewRecorder()
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = path
			req.URL.RawPath = ""
			req.Host = target.Host
		},
		Transport: h.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			zap.L().Error("proxy to backend failed", zap.String("path", path), zap.String("host", target.Host), zap.Error(err))
			w.Header().Set(headerProxyError, "1")
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(rec, req)
	return rec
}

// isUnreachable reports whether the recorded response is a proxy error (backend not reachable),
// as opposed to a real reply from the backend.
func isUnreachable(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusBadGateway && rec.Header().Get(headerProxyError) == "1"
}

// writeRecorded copies the recorded backend response to the client, minus internal markers.
func writeRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		if k == headerProxyError {
			continue
		}
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
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
		if len(parts) >= 4 && (parts[2] == "connect" || parts[2] == "connectionState" || parts[2] == "logout" || parts[2] == "delete" || parts[2] == "update") {
			// Evolution: /v1/instance/connect/:id, /v1/instance/connectionState/:id, etc.
			return parts[3]
		}
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
