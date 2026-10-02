package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/httpapi/gen"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// Deps 是 HTTP 层需要的全部依赖，由 cmd 层注入。
type Deps struct {
	Logger       *slog.Logger
	Pool         *pgxpool.Pool
	Redis        *redis.Client
	Issuer       *auth.TokenIssuer
	RefreshStore *auth.RefreshStore
	Wechat       auth.WechatExchanger
	Payment      payment.Provider
	PendingTTL   time.Duration
}

// NewRouter 组装路由与中间件链。
//
// 中间件顺序有讲究：RequestID 必须最先（后面所有日志都要用它），
// Recoverer 要在业务之前的最后一层（这样它下面任何 panic 都能兜住）。
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(AccessLog(d.Logger))
	r.Use(Recoverer)
	r.Use(chimw.RealIP)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(authGate(d.Issuer, d.Payment.Name()))

	// /readyz 不在契约里：它是编排层面的探针，不该因为 API 契约变动而抖动。
	// /healthz 则由契约生成（见 openapi.yaml），这样它的形状有漂移门禁兜底。
	r.Get("/readyz", readyz(d))

	svc := service.New(service.Deps{
		Q:            repo.New(d.Pool),
		Pool:         d.Pool,
		Redis:        d.Redis,
		Issuer:       d.Issuer,
		RefreshStore: d.RefreshStore,
		Wechat:       d.Wechat,
		Payment:      d.Payment,
		Logger:       d.Logger,
		PendingTTL:   d.PendingTTL,
	})

	strict := gen.NewStrictHandlerWithOptions(NewHandler(svc), nil, gen.StrictHTTPServerOptions{
		// 业务错误 → RFC7807。所有状态码映射都收敛在 WriteProblem 一处。
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteProblem(w, r, err)
		},
		// 请求体解析失败属于客户端问题，回 400 而不是 500。
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteProblem(w, r, fmt.Errorf("%w: %v", domain.ErrConflict, err))
		},
	})

	return gen.HandlerFromMux(strict, r)
}

func readyz(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		checks := map[string]string{"pg": "ok", "redis": "ok"}
		healthy := true

		if err := d.Pool.Ping(ctx); err != nil {
			checks["pg"] = err.Error()
			healthy = false
		}
		if err := d.Redis.Ping(ctx).Err(); err != nil {
			checks["redis"] = err.Error()
			healthy = false
		}

		if !healthy {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "unavailable",
				"checks": checks,
			})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
	}
}

// authGate 是**默认拒绝**的认证门禁。
//
// 两个设计决定：
//  1. 不用 chi 的 RoutePattern 做精确匹配 —— chi 的 r.Use 中间件在路由匹配之前执行，
//     那时 RoutePattern 还是空的。所以按路径前缀判定。
//  2. 默认拒绝而非默认放行：新增接口如果忘了登记白名单，它会被保护起来（fail closed），
//     而不是对外裸奔。写漏一个接口最多是 401，不会变成数据泄露。
func authGate(issuer *auth.TokenIssuer, paymentProvider string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicRoute(r.Method, r.URL.Path, paymentProvider) {
				next.ServeHTTP(w, r)
				return
			}

			raw := bearerToken(r)
			if raw == "" {
				WriteProblem(w, r, domain.ErrUnauthorized)
				return
			}
			claims, err := issuer.ParseAccess(raw)
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
		})
	}
}

// isPublicRoute 判定某条路由是否无需登录。
// 新增接口时如果没在这里登记，它会被自动保护起来。
func isPublicRoute(method, path, paymentProvider string) bool {
	switch {
	case path == "/healthz", path == "/readyz":
		return true
	case strings.HasPrefix(path, "/api/v1/auth/"):
		return true
	// 商品浏览对未登录用户开放
	case method == http.MethodGet && (path == "/api/v1/products" || strings.HasPrefix(path, "/api/v1/products/")):
		return true
	// mock 支付回调入口只在 mock 模式下存在；换真实支付后这里直接当作 404，
	// 不能留一个可以凭空把订单改成已支付的公开入口。
	case method == http.MethodPost && path == "/api/v1/pay/mock/notify":
		return paymentProvider == "mock"
	default:
		return false
	}
}

// RequireRole 在 authGate 之后使用，做角色鉴权（商户端接口用）。
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := auth.FromContext(r.Context())
			if !ok {
				WriteProblem(w, r, domain.ErrUnauthorized)
				return
			}
			if !claims.HasRole(role) {
				WriteProblem(w, r, domain.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
