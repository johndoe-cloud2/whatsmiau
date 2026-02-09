package main

import (
	"log"
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
	"golang.org/x/net/context"
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
	}

	app := echo.New()
	app.Pre(middleware.Recover())
	app.Pre(middleware.RemoveTrailingSlash())
	app.Pre(middleware.CORS())

	routes.Load(app)

	port := ":" + env.Env.Port
	zap.L().Info("starting server...", zap.String("port", port))

	s := &http2.Server{}
	if err := app.StartH2CServer(port, s); err != nil {
		zap.L().Fatal("failed to start server", zap.Error(err))
	}
}
