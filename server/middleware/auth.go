package middleware

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/verbeux-ai/whatsmiau/env"
)

func Auth(ctx echo.Context, next echo.HandlerFunc) error {
	p := ctx.Request().URL.Path
	if p == "/" || p == "/health" {
		return next(ctx)
	}
	if strings.HasPrefix(p, "/media/") {
		return next(ctx)
	}
	if strings.HasPrefix(p, "/swagger/") {
		return next(ctx)
	}

	configuredKey := strings.TrimSpace(env.Env.ApiKey)
	if configuredKey == "" {
		return next(ctx)
	}
	if ctx.Request().Header.Get("apikey") != configuredKey {
		return echo.NewHTTPError(http.StatusUnauthorized)
	}

	return next(ctx)
}

type simplifiedMiddleware func(c echo.Context, next echo.HandlerFunc) error

func Simplify(handler simplifiedMiddleware) func(next echo.HandlerFunc) echo.HandlerFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			return handler(ctx, next)
		}
	}
}
