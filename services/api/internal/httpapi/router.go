package httpapi

import (
	"context"
	"errors"
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

	// 业务层先于中间件构建：merchantStatusGate 需要用它查库确认入驻状态。
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

	table := routePolicy(d.Payment.Name())

	r.Use(RequestID)
	r.Use(AccessLog(d.Logger))
	r.Use(Recoverer)
	r.Use(chimw.RealIP)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(CORS(CORSConfig{AllowedOrigins: d.CORSOrigins, AllowLocalhost: d.CORSAllowLocal}))
	r.Use(authGate(d.Issuer, d.Payment.Name()))
	// 必须排在 authGate **之后**：它要读 authGate 放进 context 的 claims。
	r.Use(merchantStatusGate(table, svc.MerchantStatus))

	// /readyz 不在契约里：它是编排层面的探针，不该因为 API 契约变动而抖动。
	// /healthz 则由契约生成（见 openapi.yaml），这样它的形状有漂移门禁兜底。
	r.Get("/readyz", readyz(d))

	strict := gen.NewStrictHandlerWithOptions(NewHandler(svc), nil, gen.StrictHTTPServerOptions{
		// 业务错误 → RFC7807。所有状态码映射都收敛在 WriteProblem 一处。
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteProblem(w, r, err)
		},
		// 请求体解析失败属于客户端问题，回 400 而不是 500。
		//
		// 刻意**不**把 err 拼进去：解析库的原文含 Go 类型名与字段名
		// （"json: cannot unmarshal number into Go struct field
		// PasswordLoginRequest.phone of type string"），回给客户端等于送 schema。
		// WriteProblem 只外泄 domain.UserFacingf 显式声明过的文案，这里不声明即回标题。
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteProblem(w, r, domain.ErrValidation)
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

// merchantStatusLookup 查一个商户当前的入驻状态。查不到返回 domain.ErrNotFound。
type merchantStatusLookup func(ctx context.Context, userID string) (string, error)

// merchantStatusGate 拦的是**业务资格的缺失**，不是身份的缺失。
//
// 为什么要独立成一层，而不是塞进 authGate：
//   - authGate 只解 token，零 IO，可以认为它永远便宜；这一层每次请求都要查一次库。
//     合在一起会让"认证开销"这个本来确定的量变得不确定。
//   - 更重要的是语义不同。authGate 回答「你是谁」，这一层回答「你现在被允许经营吗」。
//     后者是**随时间变化的**（今天驳回、明天通过），所以每次都要重新问。
//     而 token 里冻结的身份是签发那一刻的快照 —— 把审核状态编进 token，
//     等于给被驳回的商户留一个「token 过期前照常卖货」的窗口，
//     而这个窗口恰恰出现在最需要立刻止损的时刻。
//
// 只对策略表里标了 requireApprovedMerchant 的路径生效。/api/v1/merchant/profile
// 刻意不标 —— 它是被驳回的商户唯一能读到自己驳回理由的地方，一起卡掉就等于
// 切断了他和平台之间的沟通渠道，只能重新注册账号。
//
// SUSPENDED 也在这里被挡（只放行 APPROVED）。SUSPENDED 是平台的事后处置，
// 存在的意义就是立刻停掉经营动作；如果它还能源源不断地卖货，这个状态就是摆设。
func merchantStatusGate(table []access, lookup merchantStatusLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !policyFor(table, r.Method, r.URL.Path).requireApprovedMerchant {
				next.ServeHTTP(w, r)
				return
			}

			claims, ok := auth.FromContext(r.Context())
			if !ok {
				// 正常路径下 authGate 已经放好 claims 了。走到这里说明中间件顺序被改坏。
				// 报 401 而不是放行 —— 宁可功能不通，也不能静默退化成"这一层不生效"。
				WriteProblem(w, r, domain.ErrUnauthorized)
				return
			}

			status, err := lookup(r.Context(), claims.UserID)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					// 从未提交入驻资料。对外与"审核未通过"同形 —— 都是"你现在不能经营"。
					WriteProblem(w, r, domain.UserFacingf(
						"尚未提交入驻资料", domain.ErrMerchantNotApproved))
					return
				}
				WriteProblem(w, r, err)
				return
			}
			if status != service.MerchantApproved {
				// 状态值必须用 UserFacing 显式声明才能外泄：WriteProblem 默认只回固定标题。
				// 这个值是前端路由的依据（跳"审核中"还是"已驳回"），必须给出去。
				WriteProblem(w, r, domain.UserFacingf(
					fmt.Sprintf("当前入驻状态为 %s", status),
					domain.ErrMerchantNotApproved))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
