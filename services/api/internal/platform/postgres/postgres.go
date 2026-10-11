package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultMaxConns 是连接池上限的默认值（历史值，配置可覆盖）。
//
// 它必须与 MaxInflightRequests 一起看：池是入口并发**下游**的一个更小的容量限制，
// 两层之差就是"已进入业务、但还在等连接"的请求数 —— 那部分由 ctx 超时兜底。
const defaultMaxConns = 20

// Option 用来覆盖连接池的默认装配。
type Option func(*pgxpool.Config)

// WithMaxConns 覆盖连接池上限。n ≤ 0 时忽略（沿用默认），
// 因为"0 条连接"只会得到一个永远超时的池，而那是配置错误而非意图。
func WithMaxConns(n int) Option {
	return func(c *pgxpool.Config) {
		if n <= 0 {
			return
		}
		c.MaxConns = int32(n)
		// MinConns 不能大于 MaxConns：pgxpool 会拒绝这种配置直接报错，
		// 而错误信息不会告诉你"是你把 MaxConns 调到了 MinConns 以下"。
		if c.MinConns > c.MaxConns {
			c.MinConns = c.MaxConns
		}
	}
}

// New 建立 PG 连接池并立刻 Ping 一次 —— 连不上就在启动阶段失败，
// 不要等到第一个请求进来才发现。
func New(ctx context.Context, dsn string, opts ...Option) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 DATABASE_URL 失败: %w", err)
	}

	cfg.MaxConns = defaultMaxConns
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	for _, opt := range opts {
		opt(cfg)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 PG 连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PG Ping 失败: %w", err)
	}
	return pool, nil
}
