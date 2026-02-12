package main

import (
	"context"
	"log"
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
		if err := redisRepo.RegisterBackend(context.Background(), env.Env.BackendPublicURL); err != nil {
			zap.L().Warn("failed to register backend in Redis", zap.Error(err))
		} else {
			zap.L().Info("registered backend in Redis", zap.String("url", env.Env.BackendPublicURL))
		}
		// On SIGTERM/SIGINT (e.g. ECS stop), unregister so the router does not keep proxying to this task.
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			zap.L().Info("shutdown signal received, unregistering backend from Redis")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
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
