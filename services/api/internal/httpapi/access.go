package httpapi

import (
	"net/http"
	"strings"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
)

// accessLevel 是一次请求被允许的程度。
//
// 零值刻意是 levelDeny —— 任何「忘了设置」的策略都会落到最严格的一档。
// 这是 fail-closed 的类型化表达：不靠额外的布尔字段兜底，零值本身就是安全的。
type accessLevel int

const (
	// levelDeny 拒绝。未登记的路由归到这里。
	levelDeny accessLevel = iota
	// levelPublic 无需登录。
	levelPublic
	// levelAnyRole 任意已登录角色都可以（用于「看自己的」这类接口）。
	levelAnyRole
	// levelRoles 必须命中 roles 里列举的角色之一。
	levelRoles
)

// access 描述一组路径的访问要求。
type access struct {
	level   accessLevel
	prefix  string
	methods []string // 空 = 不限方法
	roles   []string // 仅 levelRoles 时生效
}

// matchesPath 按**路径段边界**匹配前缀。
//
// 刻意不用裸 strings.HasPrefix：那样 `/api/v1/me` 会匹配到 `/api/v1/members`，
// 将来随手加一个接口就可能把权限意外放宽。段边界要求 path 恰好等于 prefix，
// 或者以 `prefix + "/"` 开头。
func (a access) matchesPath(path string) bool {
	if a.prefix == "" {
		return false
	}
	return path == a.prefix || strings.HasPrefix(path, a.prefix+"/")
}

func (a access) matchesMethod(method string) bool {
	if len(a.methods) == 0 {
		return true
	}
	for _, m := range a.methods {
		if m == method {
			return true
		}
	}
	return false
}

func (a access) allowsRole(role string) bool {
	for _, r := range a.roles {
		if r == role {
			return true
		}
	}
	return false
}

// routePolicy 构建访问策略表。
//
// 为什么是「表」而不是逐条 if：oapi-codegen 的 `HandlerFromMux` 是**统一挂载**的，
// 没有给部分路由挂中间件的位置 —— 这正是 router.go 里那个 `RequireRole` 写好了
// 却挂不上去、沦为死代码的原因。既然挂不上，就只能把「哪段路径要什么角色」
// 收敛成数据，由 authGate 在路由匹配**之前**按前缀查表。
// 表还有个实打实的好处：它能被表驱动测试穷举，而 if 分支只能靠人想全。
//
// 公开端点刻意**逐条列举**，不用 `/api/v1/auth` 这种大前缀。大前缀看着省事，
// 但它是 fail-open 的：将来往 auth 下加一个 `GET /auth/sessions`（查我的登录设备），
// 会被前缀顺带放行成公开接口，而且没有任何信号提醒你。逐条列举时，
// 忘记登记的新接口会被默认拒掉（401）—— 坏的后果从「数据泄露」降级为「功能不通」。
//
// paymentProvider 参与构表：mock 支付回调只在 mock 模式下存在。换成真实支付后
// 这个入口必须当作不存在，绝不能留一个能凭空把订单改成已支付的公开入口。
func routePolicy(paymentProvider string) []access {
	table := []access{
		// 探针。只放 GET —— 探针永远只用 GET，能收窄就收窄。
		{level: levelPublic, prefix: "/healthz", methods: []string{http.MethodGet}},
		{level: levelPublic, prefix: "/readyz", methods: []string{http.MethodGet}},

		// 认证：本身就是为了拿凭证，不能要求先有凭证。
		// 只用 POST —— 这些动作都是「提交凭证换取结果」，没有 GET 的余地。
		{level: levelPublic, prefix: "/api/v1/auth/wechat/login", methods: []string{http.MethodPost}},
		{level: levelPublic, prefix: "/api/v1/auth/merchant/login", methods: []string{http.MethodPost}},
		{level: levelPublic, prefix: "/api/v1/auth/admin/login", methods: []string{http.MethodPost}},
		{level: levelPublic, prefix: "/api/v1/auth/merchant/register", methods: []string{http.MethodPost}},
		{level: levelPublic, prefix: "/api/v1/auth/refresh", methods: []string{http.MethodPost}},
		{level: levelPublic, prefix: "/api/v1/auth/logout", methods: []string{http.MethodPost}},

		// 商品浏览对未登录用户开放，但**只限 GET**。
		// 商户的写操作走 /api/v1/merchant/products，不在这条里。
		{level: levelPublic, prefix: "/api/v1/products", methods: []string{http.MethodGet}},

		// 看自己的信息：三个角色的前端都要用，所以是「任意已登录」。
		{level: levelAnyRole, prefix: "/api/v1/me"},

		// 用户端业务。刻意只放 user —— 商户账号去加购物车、下单没有业务意义，
		// 放进来只会让「这个 token 到底能干什么」变模糊，也让越权测试多一堆无意义的组合。
		{level: levelRoles, prefix: "/api/v1/cart", roles: []string{auth.RoleUser}},
		{level: levelRoles, prefix: "/api/v1/orders", roles: []string{auth.RoleUser}},

		// 商户端业务。
		{level: levelRoles, prefix: "/api/v1/merchant", roles: []string{auth.RoleMerchant}},

		// 平台运营。管理员**不能**走商户端接口 —— 平台身份与商户身份刻意不重叠，
		// 否则「这次操作到底是谁干的」在审计上说不清。
		// 真要代操作应该另做 impersonation（带显式授权与痕迹），而不是默认放行。
		{level: levelRoles, prefix: "/api/v1/admin", roles: []string{auth.RoleAdmin}},
	}

	if paymentProvider == "mock" {
		table = append(table, access{
			level:   levelPublic,
			prefix:  "/api/v1/pay/mock/notify",
			methods: []string{http.MethodPost},
		})
	}

	return table
}

// policyFor 查出某次请求适用的访问策略。查不到返回零值（levelDeny），即拒绝。
func policyFor(table []access, method, path string) access {
	// 预检请求按规范不带 Authorization 头 —— 它本身就是用来问「我能不能带这个头」的，
	// 对它做认证没有意义。正常情况下 CORS 中间件已经在 authGate 之前短路掉了，
	// 这里再放行一次是**纵深防御**：万一将来有人调整中间件顺序，也不会退化成
	// 「所有跨域请求 401」。
	//
	// 注意：正因为有这一条，「中间件顺序被改错」单靠这一层是掩盖得住的。
	// 真正把它钉死的是 smoke.js 里的预检断言 —— 实测同时改掉顺序和这一条，
	// 预检才会退回 401。
	if method == http.MethodOptions {
		return access{level: levelPublic}
	}

	for _, a := range table {
		if a.matchesPath(path) && a.matchesMethod(method) {
			return a
		}
	}
	return access{}
}
