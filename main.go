package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/verbeux-ai/whatsmiau/env"
	log_connect "github.com/verbeux-ai/whatsmiau/lib/log-connect"
	"github.com/verbeux-ai/whatsmiau/lib/whatsmiau"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"github.com/verbeux-ai/whatsmiau/server/routes"
	"github.com/verbeux-ai/whatsmiau/services"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

// cleanupDeadBackends removes unreachable backend URLs from Redis on startup.
// This prevents the router from trying to proxy to old/dead ECS task IPs.
func cleanupDeadBackends(redisRepo *instances.RedisInstance) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backends, err := redisRepo.GetAllBackends(ctx)
	if err != nil {
		zap.L().Warn("failed to get backends for cleanup", zap.Error(err))
		return
	}
	if len(backends) == 0 {
		return
	}

	zap.L().Info("checking backends health on startup", zap.Int("count", len(backends)))
	client := &http.Client{Timeout: 3 * time.Second}
	removed := 0

	for _, backendURL := range backends {
		// Quick health check: GET / (backend serves /health or / for health)
		resp, err := client.Get(backendURL + "/")
		if err != nil || (resp != nil && resp.StatusCode >= 500) {
			if resp != nil {
				resp.Body.Close()
			}
			// Backend is dead or returning 5xx; remove it
			if err := redisRepo.UnregisterBackend(ctx, backendURL); err != nil {
				zap.L().Warn("failed to remove dead backend during cleanup", zap.String("url", backendURL), zap.Error(err))
			} else {
				zap.L().Info("removed dead backend during cleanup", zap.String("url", backendURL))
				removed++
			}
		} else if resp != nil {
			resp.Body.Close()
		}
	}

	if removed > 0 {
		zap.L().Info("cleaned up dead backends", zap.Int("removed", removed))
	}
}

func main() {
	if err := env.Load(); err != nil {
		panic(err)
	}

	if err := log_connect.StartLogger(); err != nil {
		log.Fatalln(err)
	}

	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	whatsmiau.LoadMiau(ctx, services.SQLStore())

	if env.Env.BackendPublicURL != "" {
		redisRepo := instances.NewRedis(services.Redis())
		
		// Clean up dead backends from Redis on startup (quick health check)
		cleanupDeadBackends(redisRepo)
		
		if err := redisRepo.RegisterBackend(context.Background(), env.Env.BackendPublicURL); err != nil {
			zap.L().Warn("failed to register backend in Redis", zap.Error(err))
		} else {
			zap.L().Info("registered backend in Redis", zap.String("url", env.Env.BackendPublicURL))
		}
		// On SIGTERM/SIGINT (e.g. ECS stop), unregister and delete routes so the router stops proxying to this task.
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			zap.L().Info("shutdown signal received, unregistering backend from Redis")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if n, err := redisRepo.DeleteRoutesForBackend(ctx, env.Env.BackendPublicURL); err != nil {
				zap.L().Warn("failed to delete routes for backend from Redis", zap.Error(err))
			} else {
				zap.L().Info("deleted routes for backend in Redis", zap.String("url", env.Env.BackendPublicURL), zap.Int("count", n))
			}
			if err := redisRepo.UnregisterBackend(ctx, env.Env.BackendPublicURL); err != nil {
				zap.L().Warn("failed to unregister backend from Redis", zap.Error(err))
			} else {
				zap.L().Info("unregistered backend from Redis", zap.String("url", env.Env.BackendPublicURL))
			}
			os.Exit(0)
		}()
	}

	app := echo.New()
	app.Pre(middleware.Recover())
	app.Pre(middleware.RemoveTrailingSlash())
	app.Pre(middleware.CORS())

	// Health endpoint for docker/ECS healthcheck (GET /); no auth required
	app.GET("/", func(c echo.Context) error { return c.String(200, "ok") })

	// Serve local media folder when LOCAL_MEDIA_PATH is set (e.g. test-stack saves images here)
	if env.Env.LocalMediaPath != "" {
		app.Static("/media", env.Env.LocalMediaPath)
	}

	routes.Load(app)

	port := ":" + env.Env.Port
	zap.L().Info("starting server...", zap.String("port", port))

	s := &http2.Server{}
	if err := app.StartH2CServer(port, s); err != nil {
		zap.L().Fatal("failed to start server", zap.Error(err))
	}
}
