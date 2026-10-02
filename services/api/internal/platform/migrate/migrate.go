package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/tutic/itiao-mall/services/api/db"
)

// Up 把 db/migrations 下的迁移全部前滚。
// 迁移文件嵌在二进制里，所以部署时不需要额外带 SQL 文件。
func Up(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(&gooseLogger{l: logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("设置 goose 方言失败: %w", err)
	}

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("执行迁移失败: %w", err)
	}
	return nil
}

// Version 返回当前已应用的最高迁移版本号。
func Version(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, sqlDB)
}

// gooseLogger 把 goose 的日志接进 slog，避免它直接打到 stderr 破坏 JSON 日志。
type gooseLogger struct{ l *slog.Logger }

func (g *gooseLogger) Printf(format string, v ...any) {
	g.l.Info("goose: " + fmt.Sprintf(format, v...))
}

func (g *gooseLogger) Fatalf(format string, v ...any) {
	g.l.Error("goose: " + fmt.Sprintf(format, v...))
}

// WaitForDB 在容器编排里很实用：DB 可能比 API 先起也可能后起，这里做有界重试。
func WaitForDB(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pool.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("等待数据库就绪超时 (%s): %w", timeout, lastErr)
}
