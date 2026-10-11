package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultPoolSize 是业务 Redis 客户端的池大小默认值（历史值，配置可覆盖）。
const defaultPoolSize = 20

// Option 用来覆盖客户端的默认装配。
type Option func(*redis.Options)

// WithPoolSize 覆盖连接池大小。n ≤ 0 时忽略（沿用默认）——
// "0 条连接"只会得到一个永远超时的客户端，那是配置错误而非意图。
func WithPoolSize(n int) Option {
	return func(o *redis.Options) {
		if n > 0 {
			o.PoolSize = n
		}
	}
}

// New 建立 Redis 客户端并 Ping 一次。
func New(ctx context.Context, addr, password string, db int, opts ...Option) (*redis.Client, error) {
	o := &redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     defaultPoolSize,
		MinIdleConns: 2,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	for _, opt := range opts {
		opt(o)
	}
	client := redis.NewClient(o)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("Redis Ping 失败 (addr=%s db=%d): %w", addr, db, err)
	}
	return client, nil
}
