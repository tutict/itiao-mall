package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// 这组测试盯的是**越权**：三种角色各自能进哪段路径。
//
// 为什么值得写这么细：权限矩阵属于"写错了本地也一切正常"的那类东西 ——
// 用户端能用、商户端能用、管理员端也能用，只有拿 A 的 token 去打 B 的接口才会露馅。
// 而这类问题一旦上线就是数据泄露（商户改别家库存、用户看全平台订单）。
//
// 所以分两层测：
//  1. 策略表当纯函数穷举（快、覆盖全、失败信息直接指到哪条规则错）
//  2. 再穿过真实 authGate 端到端验一遍（证明"表"和"链路"是一回事，不是两张皮）

func TestRoutePolicy_AccessMatrix(t *testing.T) {
	table := routePolicy("mock")

	cases := []struct {
		name   string
		method string
		path   string
		want   accessLevel
		why    string
	}{
		// ---- 公开 ----
		{"健康检查", http.MethodGet, "/healthz", levelPublic, "探针不该要 token"},
		{"商户登录", http.MethodPost, "/api/v1/auth/merchant/login", levelPublic, "登录就是为了拿凭证"},
		{"管理员登录", http.MethodPost, "/api/v1/auth/admin/login", levelPublic, "管理员要有自己的入口"},
		{"商户注册", http.MethodPost, "/api/v1/auth/merchant/register", levelPublic, "注册必须匿名可访问"},
		{"商品列表", http.MethodGet, "/api/v1/products", levelPublic, "未登录也要能逛"},
		{"商品详情", http.MethodGet, "/api/v1/products/abc", levelPublic, "同上"},

		// ---- 任意已登录 ----
		{"我的信息", http.MethodGet, "/api/v1/me", levelAnyRole, "三个角色都要看自己"},

		// ---- 用户端 ----
		{"购物车", http.MethodGet, "/api/v1/cart", levelRoles, "只有 user"},
		{"加购", http.MethodPut, "/api/v1/cart/items/x", levelRoles, "只有 user"},
		{"下单", http.MethodPost, "/api/v1/orders", levelRoles, "只有 user"},
		{"支付", http.MethodPost, "/api/v1/orders/x/pay", levelRoles, "只有 user"},

		// ---- 商户端 ----
		{"商户商品列表", http.MethodGet, "/api/v1/merchant/products", levelRoles, "只有 merchant"},
		{"商户改商品", http.MethodPut, "/api/v1/merchant/products/x", levelRoles, "只有 merchant"},
		{"商户订单", http.MethodGet, "/api/v1/merchant/orders", levelRoles, "只有 merchant"},
		{"商户发货", http.MethodPost, "/api/v1/merchant/orders/x/ship", levelRoles, "只有 merchant"},
		{"入驻资料", http.MethodGet, "/api/v1/merchant/profile", levelRoles, "只有 merchant"},
		{"改入驻资料", http.MethodPut, "/api/v1/merchant/profile", levelRoles, "只有 merchant"},

		// ---- 管理员端 ----
		{"审核商户队列", http.MethodGet, "/api/v1/admin/merchants", levelRoles, "只有 admin"},
		{"审核商户", http.MethodPost, "/api/v1/admin/merchants/x/review", levelRoles, "只有 admin"},
		{"审核商品队列", http.MethodGet, "/api/v1/admin/products", levelRoles, "只有 admin"},

		// ---- 必须被拒绝 ----
		{"未登记路径", http.MethodGet, "/api/v1/nonexistent", levelDeny, "fail closed"},
		{"未登记的新前缀", http.MethodPost, "/api/v1/promotions", levelDeny, "新增接口忘登记就该被拒"},
		{"商品写操作", http.MethodPost, "/api/v1/products", levelDeny, "写操作必须走商户端，不能蹭公开的 GET 规则"},
		{"商品删除", http.MethodDelete, "/api/v1/products/abc", levelDeny, "同上"},
		{"探针的写", http.MethodPost, "/healthz", levelDeny, "只放 GET"},
		{"登录用 GET", http.MethodGet, "/api/v1/auth/merchant/login", levelDeny, "登录只该是 POST"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policyFor(table, tc.method, tc.path).level
			if got != tc.want {
				t.Fatalf("%s %s 应为 level=%d（%s），实际 level=%d",
					tc.method, tc.path, tc.want, tc.why, got)
			}
		})
	}
}

// 上面那条矩阵测试只断言了「这一档需要角色」，**没断言具体是哪个角色**。
// 那是个真实的盲区：把 /api/v1/merchant 的角色误配成 user，矩阵测试照样全绿 ——
// level 依然是 levelRoles。所以「哪段路径归哪个角色」必须单独钉一遍。
//
// 断言用「恰好相等」而不是「包含」：包含式断言抓不到「多绑了一个角色」，
// 而多绑一个角色正是越权最常见的形态。
func TestRoutePolicy_RoleAssignment(t *testing.T) {
	table := routePolicy("mock")

	cases := []struct {
		path      string
		wantRoles []string
	}{
		{"/api/v1/cart", []string{auth.RoleUser}},
		{"/api/v1/cart/items/x", []string{auth.RoleUser}},
		{"/api/v1/orders", []string{auth.RoleUser}},
		{"/api/v1/orders/x/pay", []string{auth.RoleUser}},
		{"/api/v1/merchant/products", []string{auth.RoleMerchant}},
		{"/api/v1/merchant/orders", []string{auth.RoleMerchant}},
		{"/api/v1/merchant/profile", []string{auth.RoleMerchant}},
		{"/api/v1/admin/merchants", []string{auth.RoleAdmin}},
		{"/api/v1/admin/merchants/x/review", []string{auth.RoleAdmin}},
		{"/api/v1/admin/products", []string{auth.RoleAdmin}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			pol := policyFor(table, http.MethodGet, tc.path)
			if pol.level != levelRoles {
				t.Fatalf("%s 应为 levelRoles，实际 level=%d", tc.path, pol.level)
			}
			if len(pol.roles) != len(tc.wantRoles) {
				t.Fatalf("%s 应**恰好**绑定 %v，实际 %v（多绑一个角色就是多一条越权路径）",
					tc.path, tc.wantRoles, pol.roles)
			}
			for _, want := range tc.wantRoles {
				if !pol.allowsRole(want) {
					t.Fatalf("%s 应允许 %q，实际 %v", tc.path, want, pol.roles)
				}
			}
		})
	}

	// 通用不变量：当前设计里没有任何端点需要「多个角色之一」，全部是单角色独占。
	// 哪天真的要多角色，这里会红 —— 那时应该是有意识地改，而不是悄悄多绑一个。
	for _, a := range table {
		if a.level == levelRoles && len(a.roles) != 1 {
			t.Fatalf("%s 绑定了 %d 个角色（%v），当前设计里每个端点只归一个角色",
				a.prefix, len(a.roles), a.roles)
		}
	}
}

// 入驻审核豁免名单必须是**恰好**一条，而且只可能是 profile。
//
// 这条守卫盯的是一个后果很重的错误方向：把某条商户业务接口的 requireApprovedMerchant
// 漏设成 false，那个接口就会对「审核中」甚至「已停用」的商户开放 —— 上线后表现为
// 被平台停掉的商户照样能卖货，而单元测试全绿（因为没人断言过这个维度）。
//
// 用"恰好相等"而不是"包含 profile"：包含式断言抓不到"新加了一条豁免"。
func TestRoutePolicy_MerchantApprovalExemptions(t *testing.T) {
	const exempt = "/api/v1/merchant/profile"

	table := routePolicy("mock")

	var unprotected []string
	var sawExempt bool
	for _, a := range table {
		if !strings.HasPrefix(a.prefix, "/api/v1/merchant") {
			continue
		}
		if a.requireApprovedMerchant {
			continue
		}
		unprotected = append(unprotected, a.prefix)
		if a.prefix == exempt {
			sawExempt = true
		}
	}

	// 下限断言：万一前缀改了或表被重构，这条测试会真空通过。先钉住豁免项确实在表里。
	if !sawExempt {
		t.Fatalf("策略表里找不到 %s —— 这条守卫已经失效（真空通过）", exempt)
	}
	if len(unprotected) != 1 {
		t.Fatalf("免入驻审核的商户路径应**恰好**只有 %s，实际有 %d 条：%v\n"+
			"（每一条例外都是一条「未通过审核也能操作」的路径，加之前先想清楚）",
			exempt, len(unprotected), unprotected)
	}

	// 反向断言：豁免名单之外的商户路径必须全部要求已通过审核。
	// 上一条只保证"只有一个例外"，这条保证"宽的那一条确实是 profile"。
	for _, a := range table {
		if !strings.HasPrefix(a.prefix, "/api/v1/merchant") || a.prefix == exempt {
			continue
		}
		if !a.requireApprovedMerchant {
			t.Fatalf("%s 没有要求入驻审核通过 —— 平台停用/驳回之后它仍然可用", a.prefix)
		}
	}
}

// 路径段边界：/api/v1/me 不能因为前缀相同就匹配到 /api/v1/members。
// 裸 strings.HasPrefix 会踩这个坑 —— 将来随手加个会员接口就把权限意外放宽了。
func TestAccess_MatchesPathRequiresSegmentBoundary(t *testing.T) {
	a := access{level: levelAnyRole, prefix: "/api/v1/me"}

	for _, hit := range []string{"/api/v1/me", "/api/v1/me/profile"} {
		if !a.matchesPath(hit) {
			t.Fatalf("%s 应命中", hit)
		}
	}
	for _, miss := range []string{"/api/v1/members", "/api/v1/members/x", "/api/v1/me2", "/api/v1"} {
		if a.matchesPath(miss) {
			t.Fatalf("%s 不该命中 —— 段边界被穿破了", miss)
		}
	}
	// 空前缀必须视为"永不匹配"，而不是"匹配一切"。
	// 前者是 fail closed，后者是灾难。
	if (access{prefix: ""}).matchesPath("/anything") {
		t.Fatal("空前缀不该匹配任何路径")
	}
}

// mock 支付回调只在 mock 模式下公开。
// 换成真实支付后这个入口必须当作不存在 —— 否则就是一个能凭空把订单改成已支付的公开接口。
func TestRoutePolicy_MockNotifyOnlyInMockMode(t *testing.T) {
	const p = "/api/v1/pay/mock/notify"

	if lv := policyFor(routePolicy("mock"), http.MethodPost, p).level; lv != levelPublic {
		t.Fatalf("mock 模式下该入口应为公开，实际 level=%d", lv)
	}
	if lv := policyFor(routePolicy("wechat"), http.MethodPost, p).level; lv != levelDeny {
		t.Fatalf("非 mock 模式下该入口必须被拒，实际 level=%d", lv)
	}
}

// ---------- 契约与策略表一致性 ----------

type specRoute struct {
	method string
	path   string
}

var (
	reSpecTopLevel = regexp.MustCompile(`^([A-Za-z][\w-]*):`)
	reSpecPath     = regexp.MustCompile(`^  (/[^\s:]*):\s*$`)
	reSpecMethod   = regexp.MustCompile(`^    (get|post|put|patch|delete|head|options):\s*$`)
)

// parseSpecRoutes 从契约里抽出全部 (method, path)。
//
// 刻意用极简文本解析而不是引 yaml 库：这只是个测试守卫，需要的信息仅限 paths 段的
// 键名与方法名。为了它把 yaml 从 indirect 提成直接依赖要动 go.mod（进而要重跑 tidy、
// 锁文件跟着漂），代价不成比例。
func parseSpecRoutes(t *testing.T, specPath string) []specRoute {
	t.Helper()

	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("读不到契约文件 %s: %v\n（守卫读不到真源时必须报错，不能跳过 —— 跳过等于没有守卫）", specPath, err)
	}

	var routes []specRoute
	inPaths := false
	curPath := ""

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, " \t\r")

		if reSpecTopLevel.MatchString(line) {
			inPaths = line == "paths:"
			continue
		}
		if !inPaths {
			continue
		}
		if m := reSpecPath.FindStringSubmatch(line); m != nil {
			curPath = m[1]
			continue
		}
		if curPath != "" {
			if m := reSpecMethod.FindStringSubmatch(line); m != nil {
				routes = append(routes, specRoute{method: strings.ToUpper(m[1]), path: curPath})
			}
		}
	}
	return routes
}

// 契约里出现的每条端点都必须在策略表登记。
//
// 这条守卫把「新增接口忘了登记策略」从**运行期故障**提前到**测试期报错**。
// 忘了登记的后果是被默认拒掉（fail closed）—— 功能不通但不会泄露数据，
// 属于可接受的方向；但如果是上线后才发现，排查成本远高于这里红一行。
func TestRoutePolicy_CoversEveryContractEndpoint(t *testing.T) {
	// 从包目录往上四级是仓库根
	routes := parseSpecRoutes(t, "../../../../docs/openapi/openapi.yaml")

	// 解析器本身的下限断言：如果正则哪天失效，routes 会变成空的，
	// 这个守卫就会**真空通过**。先钉住它确实解析出了东西。
	if len(routes) < 10 {
		t.Fatalf("只从契约解析出 %d 条端点，解析器可能已失效 —— 此守卫会因此变成真空通过", len(routes))
	}

	// 用 mock 模式构表：契约里的 /api/v1/pay/mock/notify 只在 mock 下存在，
	// 这里要验的正是"每条契约端点都有归属"，不该被支付模式干扰。
	table := routePolicy("mock")

	seen := map[string]bool{}
	var missing []string
	for _, rt := range routes {
		key := rt.method + " " + rt.path
		if seen[key] {
			continue
		}
		seen[key] = true
		if policyFor(table, rt.method, rt.path).level == levelDeny {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("契约里有 %d 条端点没在策略表登记：\n  %s\n"+
			"去 internal/httpapi/access.go 的 routePolicy 里补上，并想清楚它该归哪个角色。",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// ---------- 以下穿过真实 authGate，证明"表"和"链路"是一回事 ----------

func gateChain(t *testing.T) (http.Handler, *auth.TokenIssuer) {
	t.Helper()
	issuer := auth.NewTokenIssuer("test-secret", time.Minute)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return authGate(issuer, "mock")(terminal), issuer
}

const testUserID = "11111111-1111-4111-8111-111111111111"

func TestAuthGate_RoleEnforcement(t *testing.T) {
	h, issuer := gateChain(t)

	issue := func(t *testing.T, role string) string {
		t.Helper()
		tok, _, err := issuer.IssueAccess(testUserID, role)
		if err != nil {
			t.Fatalf("签发 %q 角色的 token 失败: %v", role, err)
		}
		return tok
	}

	cases := []struct {
		name     string
		role     string // 空 = 不带 token
		method   string
		path     string
		wantCode int
	}{
		// 无 token
		{"无 token 打用户端", "", http.MethodGet, "/api/v1/cart", http.StatusUnauthorized},
		{"无 token 打商户端", "", http.MethodGet, "/api/v1/merchant/products", http.StatusUnauthorized},
		{"无 token 打管理员端", "", http.MethodGet, "/api/v1/admin/merchants", http.StatusUnauthorized},
		{"无 token 逛商品仍可", "", http.MethodGet, "/api/v1/products", http.StatusOK},

		// user
		{"user 进购物车", auth.RoleUser, http.MethodGet, "/api/v1/cart", http.StatusOK},
		{"user 进商户端", auth.RoleUser, http.MethodGet, "/api/v1/merchant/products", http.StatusForbidden},
		{"user 进管理员端", auth.RoleUser, http.MethodGet, "/api/v1/admin/merchants", http.StatusForbidden},

		// merchant
		{"merchant 进商户端", auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/products", http.StatusOK},
		{"merchant 进购物车", auth.RoleMerchant, http.MethodGet, "/api/v1/cart", http.StatusForbidden},
		{"merchant 进管理员端", auth.RoleMerchant, http.MethodGet, "/api/v1/admin/merchants", http.StatusForbidden},

		// admin —— 刻意不能走商户端：平台身份与商户身份不重叠，
		// 否则审计上「这次操作是谁干的」说不清。
		{"admin 进管理员端", auth.RoleAdmin, http.MethodGet, "/api/v1/admin/merchants", http.StatusOK},
		{"admin 进商户端", auth.RoleAdmin, http.MethodGet, "/api/v1/merchant/products", http.StatusForbidden},
		{"admin 进购物车", auth.RoleAdmin, http.MethodGet, "/api/v1/cart", http.StatusForbidden},

		// 三种角色都能看自己
		{"user 看自己", auth.RoleUser, http.MethodGet, "/api/v1/me", http.StatusOK},
		{"merchant 看自己", auth.RoleMerchant, http.MethodGet, "/api/v1/me", http.StatusOK},
		{"admin 看自己", auth.RoleAdmin, http.MethodGet, "/api/v1/me", http.StatusOK},

		// 未登记路径：即使拿着最高权限的 token 也不放行
		{"admin 打未登记路径", auth.RoleAdmin, http.MethodGet, "/api/v1/nonexistent", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.role != "" {
				req.Header.Set("Authorization", "Bearer "+issue(t, tc.role))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("%s %s（role=%q）应返回 %d，实际 %d；body=%s",
					tc.method, tc.path, tc.role, tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// token 层面的下限：签名不对、格式不对、角色不认识，都必须在认证阶段就拒掉。
//
// 特别是「角色不认识」这一条 —— 它在策略层可能被「任意已登录放行」的规则漏过去
// （那一档本来就不看具体角色），所以必须在解析阶段拦。
func TestAuthGate_RejectsBadTokens(t *testing.T) {
	h, issuer := gateChain(t)

	forged, _, err := auth.NewTokenIssuer("wrong-secret", time.Minute).IssueAccess(testUserID, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("构造伪造 token 失败: %v", err)
	}
	unknownRole, _, err := issuer.IssueAccess(testUserID, "superuser")
	if err != nil {
		t.Fatalf("构造未知角色 token 失败: %v", err)
	}
	valid, _, err := issuer.IssueAccess(testUserID, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("签发合法 token 失败: %v", err)
	}

	cases := []struct {
		name   string
		header string
	}{
		{"签名伪造", "Bearer " + forged},
		{"未知角色", "Bearer " + unknownRole},
		{"不是 JWT", "Bearer not-a-jwt"},
		{"缺 Bearer 前缀", valid},
		{"只有 Bearer", "Bearer "},
		{"空串", ""},
	}

	// 对每一组坏凭证，把"任意已登录"和"需要具体角色"两类路径都打一遍 ——
	// 只测其中一类会漏掉另一半。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/me", "/api/v1/admin/merchants"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s 访问 %s 应 401，实际 %d", tc.name, path, rec.Code)
				}
			}
		})
	}
}

// ---------- 入驻审核闸门（merchantStatusGate） ----------

const testPendingMerchantID = "22222222-2222-4222-8222-222222222222"

// statusLookupStub 按 userID 返回预设状态；未预设的按"从未入驻"处理。
func statusLookupStub(statuses map[string]string) merchantStatusLookup {
	return func(_ context.Context, userID string) (string, error) {
		if s, ok := statuses[userID]; ok {
			return s, nil
		}
		return "", domain.ErrNotFound
	}
}

// merchantGateChain 串起真实的两层闸门：authGate → merchantStatusGate。
func merchantGateChain(t *testing.T, statuses map[string]string) (http.Handler, *auth.TokenIssuer) {
	t.Helper()
	issuer := auth.NewTokenIssuer("test-secret", time.Minute)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := authGate(issuer, "mock")(
		merchantStatusGate(routePolicy("mock"), statusLookupStub(statuses))(terminal))
	return h, issuer
}

// ★ 这一组是「平台停掉一个商户之后，他还能不能继续卖货」的守卫。
//
// 为什么必须测到这一层：策略表只说了「这条路只给 merchant 角色」，
// 而「这个 merchant 现在被允许经营吗」是另一件事、在另一张表里。
// 漏掉的话，一个被 REJECTED / SUSPENDED 的商户拿着自己那个还有效的 access token
// 依然能调 /merchant/orders —— 而这恰恰是平台最需要立刻止损的时刻。
//
// 同时**必须**断言 profile 是豁免的：豁免没生效的话，被驳回的商户读不到自己的驳回理由，
// 只能反复盲提交或干脆重新注册，平台侧的沟通成本被推高。
func TestMerchantStatusGate_BlocksUnapprovedMerchants(t *testing.T) {
	statuses := map[string]string{
		testUserID:            service.MerchantApproved,
		testPendingMerchantID: service.MerchantPending,
	}
	h, issuer := merchantGateChain(t, statuses)

	issue := func(t *testing.T, userID, role string) string {
		t.Helper()
		tok, _, err := issuer.IssueAccess(userID, role)
		if err != nil {
			t.Fatalf("签发 token 失败: %v", err)
		}
		return tok
	}

	cases := []struct {
		name     string
		userID   string
		role     string
		method   string
		path     string
		wantCode int
	}{
		// 已通过：业务接口放行
		{"已通过商户访问订单", testUserID, auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/orders", http.StatusOK},
		{"已通过商户访问商品", testUserID, auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/products", http.StatusOK},

		// 审核中：业务接口必须挡住
		{"审核中商户访问订单", testPendingMerchantID, auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/orders", http.StatusForbidden},
		{"审核中商户访问商品", testPendingMerchantID, auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/products", http.StatusForbidden},
		{"审核中商户发货", testPendingMerchantID, auth.RoleMerchant, http.MethodPost, "/api/v1/merchant/orders/x/ship", http.StatusForbidden},

		// ★ 豁免：profile 对任何状态都开放，否则被驳回的人看不到驳回理由
		{"审核中商户读自己的资料", testPendingMerchantID, auth.RoleMerchant, http.MethodGet, "/api/v1/merchant/profile", http.StatusOK},
		{"审核中商户改自己的资料", testPendingMerchantID, auth.RoleMerchant, http.MethodPut, "/api/v1/merchant/profile", http.StatusOK},

		// 从未入驻（库里没有 profile 行）：同样按"不能经营"处理
		{"无入驻记录的商户访问订单", "33333333-3333-4333-8333-333333333333", auth.RoleMerchant,
			http.MethodGet, "/api/v1/merchant/orders", http.StatusForbidden},

		// 非商户角色不该受本层影响：authGate 早就把它挡住了，回的还是 403。
		// 这条是为了钉住「顺序没被改坏」—— 如果 merchantStatusGate 跑到 authGate 前面，
		// 它会因为拿不到 claims 而回 401，这条就会红。
		{"用户打商户端", testUserID, auth.RoleUser, http.MethodGet, "/api/v1/merchant/orders", http.StatusForbidden},

		// 用户端路径完全不受这一层影响
		{"用户端购物车不受影响", testUserID, auth.RoleUser, http.MethodGet, "/api/v1/cart", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+issue(t, tc.userID, tc.role))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("%s %s（userID=%s）应返回 %d，实际 %d；body=%s",
					tc.method, tc.path, tc.userID, tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// 被挡下时，响应里必须带上**当前状态**。
//
// 前端要靠它决定跳「审核中」还是「已驳回（展示理由）」——只回一句"无权限"的话，
// 商户会以为自己做错了什么，而实际上只是还没审完。
func TestMerchantStatusGate_ReportsCurrentStatus(t *testing.T) {
	h, issuer := merchantGateChain(t, map[string]string{
		testPendingMerchantID: service.MerchantRejected,
	})
	tok, _, err := issuer.IssueAccess(testPendingMerchantID, auth.RoleMerchant)
	if err != nil {
		t.Fatalf("签发 token 失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/merchant/products", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("已驳回的商户应 403，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, service.MerchantRejected) {
		t.Fatalf("响应里应带上当前状态 %s，实际 body=%s", service.MerchantRejected, body)
	}
}

// 从未入驻（没有 profile 行）与「审核未通过」对外必须同形：都是 403 + 一句可路由的提示。
// 区分这两者只会泄露「这个账号存在但没有资料」。
func TestMerchantStatusGate_NoProfileIsAlsoForbidden(t *testing.T) {
	h, issuer := merchantGateChain(t, nil)
	tok, _, err := issuer.IssueAccess(testPendingMerchantID, auth.RoleMerchant)
	if err != nil {
		t.Fatalf("签发 token 失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/merchant/products", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("无入驻记录应 403，实际 %d；body=%s", rec.Code, rec.Body.String())
	}
}
