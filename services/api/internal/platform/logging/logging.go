package logging

import (
	"context"
	"log/slog"
	"os"
)

// New 构造全进程唯一的 slog.Logger。
// dev 用可读的 text handler，prod 用 JSON（便于采集）。
func New(level slog.Level, isDev bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if isDev {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// 请求上下文的 key，中间件写入、业务层读取。
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// WithRequestID 把 request id 塞进 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID 取出 request id（没有则返回空串）。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithLogger 把带 request id 的 logger 塞进 context。
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// From 取出 context 里的 logger；没有则返回默认 logger，绝不返回 nil。
func From(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && v != nil {
		return v
	}
	return slog.Default()
}
