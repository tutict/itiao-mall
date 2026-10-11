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
	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
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
	RefreshTTL   time.Duration

	// Limiter 可为 nil —— 那是"限流整体关闭"的显式表达，
	// 此时限流中间件被整个跳过（见下方对 nil 的处理）。
	//
	// 用 nil 而不是 `Enabled bool` 是为了让"关掉"这件事在类型上无歧义：
	// 一个 bool 加一个可能为 nil 的实现，迟早出现"Enabled=false 但中间件还在"
	// 这种半开状态，而半开的限流比没有限流更难排查。
	Limiter *ratelimit.Limiter

	// RateLimits 是两档阈值。分成"认证"与"读写"两档是因为它们的攻击形态不同：
	// 认证类防撞库（阈值低、误伤成本高），读写类防刷单与爬取（阈值高、误伤成本低）。
	RateLimits ratelimit.Defaults
	AuthLimits ratelimit.Defaults

	// CORS 需要放在 authGate 之前执行，原因见 cors.go 的注释。
	CORSOrigins    []string
	CORSAllowLocal bool

	// MaxBodyBytes 是请求体上限。≤0 时用 DefaultMaxBodyBytes。
	MaxBodyBytes int64

	// MaxInflightRequests 是同时在处理中的请求数上限（并发准入闸门）。
	// ≤0 表示不启用闸门（那是显式关闭）。
	//
	// 与 MaxBodyBytes 一样，上限值本身不是安全边界而是**容量边界**，
	// 所以它由调用方（cmd 层读配置）决定，不由本层写死。
	MaxInflightRequests int
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
		RefreshTTL:   d.RefreshTTL,
	})

	table := routePolicy(d.Payment.Name())

	r.Use(RequestID)
	// 安全响应头放在最外层之一：连 Recoverer 的 500、CORS 的 204 预检响应
	// 也要带上这些头。越靠前，覆盖面越大。
	r.Use(SecurityHeaders)
	// 并发准入闸门。位置有两层讲究：
	//
	//  1. 在 SecurityHeaders **之内**：闸门回 503 时安全头也要在（否则
	//     "过载时的响应"成了一种没有 nosniff 的响应，形状与正常响应不一致）。
	//  2. 在 AccessLog **之外**：被闸门拒绝的请求不写访问日志。这不是为了省事 ——
	//     过载时若每个被拒请求都落一行日志，日志（磁盘）会成为下一个被抽干的资源，
	//     一场拒绝风暴能把服务彻底打挂。拒绝行为的可观测性改由闸门自己的两条日志
	//     提供，且**都不按请求记**：过载告警按时间节流（每 10 秒至多一条）、
	//     恢复只记一条（CAS）。与"过载时长 / 过载事件次数"成正比而非与请求数成正比。
	//
	// 它又在 authGate / rateLimitGate / merchantStatusGate **之前**：这三层里的后两层
	// 一个要连 Redis、一个要查 PG，正是过载时最紧张的资源 —— 闸门必须先把入口
	// 并发钉住，它们才不会被压垮。
	if d.MaxInflightRequests > 0 {
		r.Use(ConcurrencyGate(d.MaxInflightRequests, time.Second))
	}
	r.Use(AccessLog(d.Logger))
	r.Use(Recoverer)
	r.Use(chimw.RealIP)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(CORS(CORSConfig{AllowedOrigins: d.CORSOrigins, AllowLocalhost: d.CORSAllowLocal}))
	r.Use(authGate(d.Issuer, d.Payment.Name()))
	// 限流必须在 authGate **之后**：它要读 authGate 放进 context 的 claims 来选
	// 限流主体（已登录按 user id，匿名按 IP）。放在前面所有请求都只能按 IP 限，
	// 同一 NAT 后的正常用户会互相挤兑。
	//
	// 又必须在 merchantStatusGate **之前**：限流是准入控制，被拒的请求
	// 不该再消耗一次数据库查询 —— 而连接池正是限流风暴时最紧张的资源。
	if d.Limiter != nil {
		r.Use(rateLimitGate(d.Limiter, ratePolicy(d.AuthLimits), d.RateLimits))
	}
	// 请求体上限排在限流**之后**：超大 body 的请求应当先被计入限流统计。
	// 排到前面的话，攻击者只要把 Content-Length 写超限，就能在不触发任何
	// 限流计数的情况下持续打目标 —— 一个白送的绕过。
	//
	// 又必须排在 handler **之前**：oapi-codegen 的解码是
	// `json.NewDecoder(r.Body).Decode(...)`，会把 body 整个读进内存。
	r.Use(BodyLimit(d.MaxBodyBytes))
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

// readyz 是编排层探针。
//
// 响应体里 checks 的值只允许是**固定枚举**（"ok" / "unavailable"）。
// 依赖错误的原文绝不能出现在这里 —— err.Error() 带着内网地址与端口，
// pgx 还会带上 host=/user=/database=，而 /readyz 是公开路由（探针不该要 token），
// 等于把内网拓扑白送给任何能发请求的人。实测过的原始响应：
//
//	{"checks":{"redis":"dial tcp [::1]:6379: connectex: No connection could be made ..."},
//	 "status":"unavailable"}
//
// 细节只进日志：要排查的人有日志可看，外部不需要。
//
// 部署层还有第二道：/readyz 应限制在网关内网白名单，不该对公网暴露。
func readyz(d Deps) http.HandlerFunc {
	return readyzHandler(
		func(ctx context.Context) error { return d.Pool.Ping(ctx) },
		func(ctx context.Context) error { return d.Redis.Ping(ctx).Err() },
	)
}

// readyzHandler 与 readyz 拆开，只为了测试能注入"返回含内网地址的假依赖"。
//
// 为什么必须走注入：靠"把 Redis 停掉"来测这条，测的是**环境**而不是代码 ——
// 而且 refused 与 timeout 两种形态会得出不同结论（前者错误里带地址，后者不一定）。
// 那样的守卫时绿时红，等于没有。
func readyzHandler(pgPing, redisPing func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		checks := map[string]string{"pg": "ok", "redis": "ok"}
		healthy := true

		if err := pgPing(ctx); err != nil {
			checks["pg"] = "unavailable"
			healthy = false
			logging.From(ctx).Error("readyz: PostgreSQL 不可用", slog.Any("err", err))
		}
		if err := redisPing(ctx); err != nil {
			checks["redis"] = "unavailable"
			healthy = false
			logging.From(ctx).Error("readyz: Redis 不可用", slog.Any("err", err))
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
