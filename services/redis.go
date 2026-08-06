package services

import (
	"crypto/tls"

	"github.com/verbeux-ai/whatsmiau/env"
	"golang.org/x/net/context"

	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
)

var redisInstance *redis.Client

func Redis() *redis.Client {
	if redisInstance == nil {
		instance, err := NewRedis()
		if err != nil {
			zap.L().Fatal("failed to start redis", zap.Error(err))
		}

		redisInstance = instance
	}

	return redisInstance
}

func NewRedis() (*redis.Client, error) {
	// The default pool is sized from GOMAXPROCS, which on a small Fargate task leaves ~10 connections
	// for up to HANDLER_SEMAPHORE_SIZE concurrent event handlers: bursts then queue on the pool and
	// blow the callers' context deadlines even while Redis itself is idle.
	poolSize := env.Env.RedisPoolSize
	if poolSize <= 0 {
		poolSize = 100
	}

	opt := &redis.Options{
		Addr:     env.Env.RedisURL,
		Password: env.Env.RedisPassword,
		DB:       0,
		PoolSize: poolSize,
	}

	if env.Env.RedisTLS {
		opt.TLSConfig = &tls.Config{}
	}

	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		zap.L().Panic("failed to connect to redis", zap.Error(err), zap.Any("env", env.Env))
	}

	return client, nil
}
