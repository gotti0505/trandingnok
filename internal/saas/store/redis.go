package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"quantsaas/internal/saas/config"
)

// RedisClient wraps go-redis with the three operations needed by QuantSaaS.
// Usage: champion gene cache (key: champion:{strategyID}), session tokens.
// Redis must NOT be used as a signal bus — all SaaS→Agent messages go via WebSocket.
type RedisClient struct {
	client *redis.Client
}

// NewRedis creates a RedisClient from config.
func NewRedis(cfg *config.Config) *RedisClient {
	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	return &RedisClient{client: rdb}
}

func (r *RedisClient) Get(ctx context.Context, key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

func (r *RedisClient) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *RedisClient) Del(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}
