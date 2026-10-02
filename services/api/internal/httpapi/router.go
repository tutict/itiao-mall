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

	// CORS 需要放在 authGate 之前执行，原因见 cors.go 的注释。
	CORSOrigins    []string
	CORSAllowLocal bool
}

// NewRouter 组装路由与中间件链。
//
// 中间件顺序有讲究：RequestID 必须最先（后面所有日志都要用它），
// Recoverer 要在业务之前的最后一层（这样它下面任何 panic 都能兜住），
// CORS 必须在 authGate 之前（预检请求不带 Authorization，见 cors.go）。
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(AccessLog(d.Logger))
	r.Use(Recoverer)
	r.Use(chimw.RealIP)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(CORS(CORSConfig{AllowedOrigins: d.CORSOrigins, AllowLocalhost: d.CORSAllowLocal}))
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

// authGate 是**默认拒绝**的认证与鉴权门禁。
//
// 三个设计决定：
//  1. 不用 chi 的 RoutePattern 做精确匹配 —— chi 的 r.Use 中间件在路由匹配之前执行，
//     那时 RoutePattern 还是空的。所以按路径前缀判定。
//  2. 默认拒绝而非默认放行：新增接口如果忘了登记策略，它会被保护起来（fail closed），
//     而不是对外裸奔。写漏一个接口最多是 401，不会变成数据泄露。
//  3. 认证与鉴权合在同一层：先解 token，再查策略表判角色。拆成两层也能做，但那样
//     「哪段路径要认证」和「哪段路径要什么角色」这两份知识会散在两个地方 ——
//     而它们本来就是同一个问题（这条路径谁能进）。
//
// 策略表的内容与每一条的设计理由见 access.go。
func authGate(issuer *auth.TokenIssuer, paymentProvider string) func(http.Handler) http.Handler {
	table := routePolicy(paymentProvider)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pol := policyFor(table, r.Method, r.URL.Path)

			switch pol.level {
			case levelDeny:
				// 未登记的路由一律拒绝。这里回 401 而不是 404 —— 路径是否存在归 chi 管，
				// 认证层不该替它回答「这个路径有没有东西」，那等于白送一个接口探测入口。
				WriteProblem(w, r, domain.ErrUnauthorized)
				return
			case levelPublic:
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

			if pol.level == levelRoles && !pol.allowsRole(claims.Role) {
				// 回 403 而不是 404：调用方已经通过认证、知道自己是哪个角色，
				// 「你没这个权限」不透露任何它本来不知道的信息。
				WriteProblem(w, r, domain.ErrForbidden)
				return
			}

			next.ServeHTTP(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
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
