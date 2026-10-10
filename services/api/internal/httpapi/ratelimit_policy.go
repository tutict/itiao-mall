package httpapi

import (
	"net/http"
	"strings"

	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
)

// 限流动作的稳定标识。
//
// 这些是**常量**而不是从 URL 推导的字符串：路由改名会让所有计数清零，
// 攻击窗口无声重开。代价是"新增路由要记得在这里登记"——
// 那由下面那条不变量测试盯着（TestRatePolicy_CoversAuthRoutes）。
const (
	actLoginMerchant = ratelimit.Action("login.merchant")
	actLoginAdmin    = ratelimit.Action("login.admin")
	actLoginWechat   = ratelimit.Action("login.wechat")
	actRegister      = ratelimit.Action("register.merchant")
	actRefresh       = ratelimit.Action("refresh")
	actLogout        = ratelimit.Action("logout")
	actCart          = ratelimit.Action("cart.write")
	actOrder         = ratelimit.Action("order.write")
	actProductRead   = ratelimit.Action("product.read")
	actMe            = ratelimit.Action("me.read")
	actMerchantWrite = ratelimit.Action("merchant.write")
	actAdminWrite    = ratelimit.Action("admin.write")
	actPayNotify     = ratelimit.Action("pay.notify")
)

// rateRule 是限流策略表的一行：哪段路径（按路径段边界匹配）用什么规则限流。
type rateRule struct {
	prefix  string
	methods []string // 空 = 不限方法
	policy  ratelimit.Policy
}

// ratePolicy 给出「哪条路径用哪个动作标识、哪个窗口、哪个阈值」。
//
// 为什么按接口而不是只按主体限流：
//   - 只按 IP 限流 → 同一个办公室/NAT 出口后面的人互相挤兑，一个用户狂刷
//     会把整栋楼的人一起挡在门外。
//   - 只按用户限流 → 攻击者在登录/注册前还没有用户身份，那两个接口等于没限。
//
// 所以认证类按「IP + 账号」双维度（见 rateLimitGate 的 subject 选取），
// 其余按 user id 优先、退回 IP。
//
// 为什么登录阈值给到 10 次/分钟：这是防撞库的上限，不是防手滑。
// 真实用户输错 3 次密码就该看到提示了，10 次足够他把密码本翻开。
// 再往下压只会先伤到记性差和用密码管理器的用户 ——
// 而他们恰恰是最不该被限流赶走的那批。
func ratePolicy(authDefaults ratelimit.Defaults) []rateRule {
	return []rateRule{
		// 认证类：窗口与阈值都走独立配置（更短更紧）。
		//
		// 三个登录入口分成三个 action 而不是共用一个：共用的话，
		// 商户端被撞库会把管理员入口的额度一起吃掉，攻击者只需打满一个阈值
		// 就能同时阻断三处的正常登录。
		{prefix: "/api/v1/auth/merchant/login", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actLoginMerchant,
				Window: authDefaults.Window, Limit: authDefaults.Limit}},
		{prefix: "/api/v1/auth/admin/login", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actLoginAdmin,
				Window: authDefaults.Window, Limit: authDefaults.Limit}},
		{prefix: "/api/v1/auth/wechat/login", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actLoginWechat,
				Window: authDefaults.Window, Limit: authDefaults.Limit}},
		{prefix: "/api/v1/auth/merchant/register", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actRegister,
				Window: authDefaults.Window, Limit: authDefaults.Limit}},

		// 刷新比登录宽裕得多：正常客户端会在 access token 快过期时自动刷新，
		// 一个用户一天可能刷几十次。10 次/分钟对登录成立，对刷新就成了
		//「用户后台放了半小时再回来，App 静默登出」——那是个真 bug，不是防护。
		{prefix: "/api/v1/auth/refresh", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actRefresh,
				Window: authDefaults.Window, Limit: authDefaults.Limit * 6}},
		{prefix: "/api/v1/auth/logout", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeAuth, Action: actLogout,
				Window: authDefaults.Window, Limit: authDefaults.Limit * 6}},

		// 支付回调：来自微信服务器，不按用户也不按 IP 限流 ——
		// 真回调的来源 IP 是腾讯的出口段，按 IP 限等于随机误杀。
		// 幂等由 payment_events 的唯一约束负责（见 0001 迁移注释），
		// 限流在这里只会挡住重试而无助于防重放，所以给一个很宽的额度。
		{prefix: "/api/v1/pay/mock/notify", methods: []string{http.MethodPost},
			policy: ratelimit.Policy{Scope: ratelimit.ScopeWrite, Action: actPayNotify,
				Limit: 600}},

		// 读：公开浏览。这是最容易被爬的路径，阈值用默认的读写档。
		{prefix: "/api/v1/products",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeRead, Action: actProductRead}},

		// 「我的信息」也限流。它看着无害，但一个已登录用户拿脚本高频打
		// /api/v1/me 有两个实际用途：探测账号是否还有效（拿到"能刷"的信号）、
		// 拖慢鉴权路径（每次都要解一次 JWT）。成本极低，收益明确，所以纳入。
		{prefix: "/api/v1/me",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeRead, Action: actMe}},

		// 写：购物车与下单。刷单脚本会先疯狂加购，所以这两个用写档的默认值。
		{prefix: "/api/v1/cart",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeWrite, Action: actCart}},
		{prefix: "/api/v1/orders",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeWrite, Action: actOrder}},

		// 商户与平台后台。阈值给写档默认值即可：这两个端的使用者数量少、
		// 行为规律，不需要额外收紧。
		{prefix: "/api/v1/merchant",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeWrite, Action: actMerchantWrite}},
		{prefix: "/api/v1/admin",
			policy: ratelimit.Policy{Scope: ratelimit.ScopeWrite, Action: actAdminWrite}},
	}
}

// lookupRateRule 查出该请求适用的限流规则。查不到返回零值（policy 为空）。
func lookupRateRule(table []rateRule, method, path string) ratelimit.Policy {
	for _, r := range table {
		if r.prefix == "" {
			continue
		}
		// 段边界匹配，与 access.go 的 matchesPath 同一套规则。
		// 用裸 HasPrefix 的话 /api/v1/products-archive 会吃上 /api/v1/products 的额度。
		if path != r.prefix && !strings.HasPrefix(path, r.prefix+"/") {
			continue
		}
		if len(r.methods) > 0 {
			hit := false
			for _, m := range r.methods {
				if m == method {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		return r.policy
	}
	return ratelimit.Policy{}
}
