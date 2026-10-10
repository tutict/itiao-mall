package httpapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
)

// rateLimitSubject 从请求里解析限流主体，并给出可读的维度名。
//
// 主体选择规则：**已登录用 user id，匿名用客户端 IP**。理由与代价：
//   - 已认证请求有稳定的用户标识，比 IP 精确得多（同一 NAT 后的多人不互相挤兑）。
//   - 匿名请求只能靠 IP —— 而攻击者恰恰只在登录/注册前匿名，那正是最需要限流的时刻。
//
// 返回的第二个值是维度名，进日志与监控：只看"某 IP 被限"和"某用户被限"，
// 运维要采取的动作完全不同（前者查扫描器，后者查这个账号是不是脚本）。
func rateLimitSubject(r *http.Request) (subject, scope string) {
	if claims, ok := auth.FromContext(r.Context()); ok && claims.UserID != "" {
		return claims.UserID, "user"
	}
	return clientIP(r), "ip"
}

// clientIP 取客户端 IP。
//
// 刻意用 RemoteAddr 而**不**读 X-Forwarded-For：那个头由客户端提供，
// 无条件采信等于任何人随手换一个头就绕过限流。正确做法是只信任自己那层
// 代理写入的头 —— chi 的 RealIP 中间件按受信代理网段重写 RemoteAddr，
// 本函数读它就已经是"只信任受信代理"之后的值。
//
// 这个取舍的代价要说明白：**部署在没有受信代理的直接暴露场景下，
// 所有请求会共用一个 IP（反代自己），限流对匿名请求等于全站一刀切。**
// 修法是在网络层加反代并把它的网段登记进 RealIP，而不是在这里采信可伪造的头。
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	if r.RemoteAddr == "" {
		// 真实 server 不会给空，但测试里的 httptest.NewRequest 可能给。
		// 返回一个固定的非空值：返回空串会被 Allow 判为"主体非法"，
		// 于是测试全挂在一个与限流逻辑无关的地方。
		return "unknown"
	}
	// 没有端口的裸地址。带方括号的 IPv6 要把括号去掉 ——
	// 不去的话同一个客户端会因写法不同而拿到两个不同的限流桶，
	// 额度凭空翻倍。用 net.ParseIP 校验而不是盲切首尾字符：
	// 那样 "[2001:db8::1" 这种坏值会被切成 "2001:db8::1"，看着能用。
	if strings.HasPrefix(r.RemoteAddr, "[") && strings.HasSuffix(r.RemoteAddr, "]") {
		bare := r.RemoteAddr[1 : len(r.RemoteAddr)-1]
		if net.ParseIP(bare) != nil {
			return bare
		}
	}
	return r.RemoteAddr
}

// rateLimitGate 是限流中间件。
//
// 位置很讲究：**必须在 authGate 之后**。它要用 authGate 放进 context 的 claims
// 来选主体维度；放在前面的话所有请求都只能按 IP 限，已认证用户之间互相挤兑。
//
// 顺序上它又在 merchantStatusGate 之前 —— 限流是准入控制，
// 应该在任何业务查库之前就把超额请求挡掉。换个顺序的话，被限流的请求
// 依然会打一次库，白白消耗连接池 —— 而连接池正是限流风暴时最紧张的资源。
func rateLimitGate(limiter *ratelimit.Limiter, table []rateRule, def ratelimit.Defaults) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 预检不带 Authorization，也不携带业务语义，限它没有意义。
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			policy := lookupRateRule(table, r.Method, r.URL.Path)
			if policy.Scope == "" {
				// 未登记的路由不受限流。这里刻意**放行**而不是拒绝：
				// 限流是保护，不是权限。它不在这张表里意味着"还没被纳入保护范围"，
				// 而拒绝会变成一个新的 fail-closed 陷阱 —— 每加一条路由都必须
				// 记得登记限流规则，漏了就是整条功能 429，且没有提示指向真实原因。
				// 真正的兜底是 authGate：未登记的路由本来就 401。
				next.ServeHTTP(w, r)
				return
			}

			subject, scope := rateLimitSubject(r)
			res, degraded, err := limiter.Decide(r.Context(), policy, subject, def)

			if err != nil && !degraded {
				// 调用方 ctx 已取消（客户端断开 / 上游超时），不该再走业务。
				// 直接返回：响应已经没人接了，写它只是浪费。
				return
			}
			if degraded {
				logging.From(r.Context()).Warn("限流后端不可用，按配置降级",
					"path", r.URL.Path,
					"scope", scope,
					"fails_closed", limiter.FailsClosed(),
					"err", err,
				)
				// fail-closed 时 Decide 返回 Allowed=false，此时**不能**回 429。
				// 429 会被客户端读成"你太频繁了"从而退避不重试 ——
				// 而真实原因是"我们这边的限流器坏了"。那样限流器一挂，
				// 全站会被客户端主动放弃到有人手工重启为止。
				if !res.Allowed {
					WriteProblem(w, r, domain.ErrRateLimitUnavailable)
					return
				}
			}

			// 无论放行还是拒绝都写响应头：客户端据此能主动退避，
			// 而不是把请求打满之后才从 429 里猜节奏。
			writeRateHeaders(w.Header(), res)

			if !res.Allowed {
				retry := res.RetryAfter
				if retry <= 0 {
					retry = time.Second
				}
				// Retry-After 用秒（向上取整）：这是 HTTP 规范认可的唯一格式。
				// 向上取整而不是截断 —— 截断成 0 会得到"Retry-After: 0"，
				// 那是明确的无脑快刷邀请。
				w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
				WriteProblem(w, r, domain.UserFacingf(
					"请求过于频繁，请稍后再试", domain.ErrRateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeRateHeaders 写出限流相关的响应头。
//
// 用 X-RateLimit-* 而不是 IETF draft 的 RateLimit-*：后者还没定稿，
// 头部名与语义都变过好几轮，现在用等于把客户端绑在一个会变的东西上。
// 三个自定义头足够客户端算出该等多久。
func writeRateHeaders(h http.Header, res ratelimit.Result) {
	h.Set("X-RateLimit-Limit", strconv.Itoa(res.Limit))
	remaining := int64(res.Limit) - res.Count
	if remaining < 0 {
		remaining = 0
	}
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
	if !res.Allowed && res.RetryAfter > 0 {
		h.Set("X-RateLimit-Reset", strconv.Itoa(int((res.RetryAfter+time.Second-1)/time.Second)))
	}
}
