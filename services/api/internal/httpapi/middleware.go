package httpapi

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
)

// requestIDPattern 是允许客户端自带的 request id 形状。
//
// 为什么要白名单、而不是"非空就采信"：这个值会被写进响应头与**全部日志**，
// 原样采信等于让调用方决定我们的日志长什么样 —— 可以塞超长串把日志行撑爆，
// 也可以塞入特殊字符伪造日志关联（日志注入）。受限字符集同时保证它能安全地
// 放进响应头（不触发 Go 的头部校验），不会在 Set 时静默失败。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// RequestID 给每个请求分配唯一 id，并写进 context 与响应头，方便串日志。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			// 不合规就**丢弃并重新生成**，而不是"清洗后使用"：
			// 清洗规则一旦有洞，流进日志的就是攻击者可控的值；
			// 而重新生成的代价只是一个 uuid。
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// statusRecorder 记录真实写出的状态码，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// AccessLog 输出结构化访问日志，并把"已绑定 request_id 的 logger"挂到 context 上，
// 后续任何一层用 logging.From(ctx) 取出来的日志都会自动带 request id，不用手工传递。
func AccessLog(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			logger := base.With(slog.String("request_id", logging.RequestID(r.Context())))
			ctx := logging.WithLogger(r.Context(), logger)
			r = r.WithContext(ctx)

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			logger.Info("http.request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

// Recoverer 兜住 panic，避免整个进程被一个请求带走。
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logging.From(r.Context()).Error("请求处理 panic",
					"panic", rec,
					"path", r.URL.Path,
					"request_id", logging.RequestID(r.Context()),
				)
				WriteJSON(w, http.StatusInternalServerError, Problem{
					Type:   "about:blank",
					Title:  "服务内部错误",
					Status: http.StatusInternalServerError,
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
