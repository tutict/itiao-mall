package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
)

// 这组测试盯的是限流的三件事：
//  1. 每条契约端点都被纳入保护（漏一条 = 那条接口完全裸奔）
//  2. 限流主体选得对（已登录按用户，匿名按 IP）
//  3. 后端故障时降级方向与 HTTP 状态码对得上

func testRateDefaults() (rw, auth ratelimit.Defaults) {
	return ratelimit.Defaults{Window: time.Minute, Limit: 100},
		ratelimit.Defaults{Window: time.Minute, Limit: 10}
}

// ★ 每条契约端点都必须被限流策略覆盖。
//
// 为什么是守卫而不是"尽量覆盖"：限流表漏一条的失败形态是**静默**的 ——
// 接口照常工作、没有任何报错，只是完全不受保护。
// 而攻击者不需要知道漏了哪条。
//
// 与 access 表的 "CoversEveryContractEndpoint" 是同一个思路的两次应用：
// 权限漏了是 401（一眼看见），限流漏了是"照常工作"（看不见）。
func TestRatePolicy_CoversEveryContractEndpoint(t *testing.T) {
	routes := parseSpecRoutes(t, "../../../../docs/openapi/openapi.yaml")
	if len(routes) < 10 {
		t.Fatalf("只从契约解析出 %d 条端点，解析器可能已失效 —— 此守卫会真空通过", len(routes))
	}

	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	// 探针与支付回调刻意不在这张表里（见 ratePolicy 的注释）。
	// 但这里要验的是"除它们之外都覆盖了"，所以先记下豁免名单。
	exempt := map[string]string{
		"GET /healthz":             "存活探针，限它等于随机误杀健康检查",
		"GET /readyz":              "就绪探针，同上",
		"POST /api/v1/auth/logout": "登出的额度已在 refresh 那条上；重复计数没有收益",
	}

	seen := map[string]bool{}
	var missing []string
	for _, rt := range routes {
		key := rt.method + " " + rt.path
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := exempt[key]; ok {
			continue
		}
		if lookupRateRule(table, rt.method, rt.path).Scope == "" {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("契约里有 %d 条端点没被限流覆盖：\n  %s\n"+
			"它们可以无限调用。要么在 ratelimit_policy.go 里登记，要么在"+
			"本测试的 exempt 名单里写明豁免理由。",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// 三个登录入口必须是**三个独立 action**。
//
// 共用一个的话，撞商户登录就能把管理员登录的额度一起吃掉 ——
// 而这两个是"独立入口"这条安全设计的最后一道防线，
// 一个被限流锁死的管理员登录界面 = 平台运营完全停摆。
func TestRatePolicy_LoginEntriesAreIndependent(t *testing.T) {
	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	paths := []string{
		"/api/v1/auth/merchant/login",
		"/api/v1/auth/admin/login",
		"/api/v1/auth/wechat/login",
	}
	actions := map[ratelimit.Action]bool{}
	for _, p := range paths {
		pol := lookupRateRule(table, http.MethodPost, p)
		if pol.Scope != ratelimit.ScopeAuth {
			t.Errorf("%s 应归 auth scope，实际 %q", p, pol.Scope)
		}
		if actions[pol.Action] {
			t.Errorf("%s 与前面某个入口共用 action %q —— 它们会互相吃额度", p, pol.Action)
		}
		actions[pol.Action] = true
	}
	if len(actions) != 3 {
		t.Fatalf("三个登录入口应有 3 个独立 action，实际 %d", len(actions))
	}
}

// 前缀匹配必须按段边界。
// `/api/v1/products-archive` 不该吃 `/api/v1/products` 的额度。
func TestLookupRateRule_RequiresSegmentBoundary(t *testing.T) {
	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	if pol := lookupRateRule(table, http.MethodGet, "/api/v1/products-archive"); pol.Scope != "" {
		t.Fatal("/api/v1/products-archive 不该匹配 /api/v1/products 的限流规则")
	}
	if pol := lookupRateRule(table, http.MethodGet, "/api/v1/products"); pol.Scope == "" {
		t.Fatal("/api/v1/products 本身应命中")
	}
	if pol := lookupRateRule(table, http.MethodGet, "/api/v1/products/x"); pol.Scope == "" {
		t.Fatal("/api/v1/products/x 应命中（前缀 + 子路径）")
	}
}

// 刷新与登出的额度必须比登录宽裕。
//
// 这是一条真实的产品约束：App 会在 access token 快过期时自动刷新，
// 一个用户一天可能几十次。把它压到 10 次/分钟，用户在后台放半小时回来
// 就会静默登出 —— 那是 bug，不是防护。
func TestRatePolicy_RefreshLimitIsLooserThanLogin(t *testing.T) {
	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	login := lookupRateRule(table, http.MethodPost, "/api/v1/auth/merchant/login")
	refresh := lookupRateRule(table, http.MethodPost, "/api/v1/auth/refresh")
	if refresh.Limit <= login.Limit {
		t.Fatalf("刷新阈值(%d)应高于登录阈值(%d)，否则正常使用会被限流踢出",
			refresh.Limit, login.Limit)
	}
}

// 已登录请求按 user id 限流，匿名按 IP。
func TestRateLimitSubject_PrefersUserID(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute)
	tok, _, err := issuer.IssueAccess(testUserID, auth.RoleMerchant)
	if err != nil {
		t.Fatal(err)
	}

	// 带 claims
	r := httptest.NewRequest(http.MethodGet, "/api/v1/merchant/products", nil)
	claims, err := issuer.ParseAccess(tok)
	if err != nil {
		t.Fatal(err)
	}
	r = r.WithContext(auth.WithClaims(r.Context(), claims))
	subject, scope := rateLimitSubject(r)
	if scope != "user" || subject != testUserID {
		t.Fatalf("已登录应按 user 限流，实际 scope=%s subject=%s", scope, subject)
	}

	// 匿名
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	r2.RemoteAddr = "203.0.113.7:54321"
	subject, scope = rateLimitSubject(r2)
	if scope != "ip" || subject != "203.0.113.7" {
		t.Fatalf("匿名应按 IP 限流，实际 scope=%s subject=%s", scope, subject)
	}

	// claims 存在但 UserID 为空 → 退回 IP，不产生空 key
	r3 := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	r3.RemoteAddr = "198.51.100.9:1234"
	r3 = r3.WithContext(auth.WithClaims(r3.Context(), &auth.Claims{}))
	subject, scope = rateLimitSubject(r3)
	if scope != "ip" || subject != "198.51.100.9" {
		t.Fatalf("空 uid 的 claims 应退回 IP，实际 scope=%s subject=%s", scope, subject)
	}
}

// RemoteAddr 没有端口时也要能取到 IP。
//
// httptest.NewRequest 在部分用法下会给不带端口的 RemoteAddr，
// 走 net.SplitHostPort 会报错 —— 那时必须退回原串而不是返回空。
func TestClientIP_HandlesMissingPort(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"203.0.113.7:54321", "203.0.113.7"},
		{"203.0.113.7", "203.0.113.7"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"", "unknown"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		r.RemoteAddr = c.remote
		if got := clientIP(r); got != c.want {
			t.Errorf("RemoteAddr=%q 应解析为 %q，实际 %q", c.remote, c.want, got)
		}
	}
}

// ★ 限流中间件端到端：超阈值回 429 + Retry-After + 三个 X-RateLimit 头。
func TestRateLimitGate_Returns429WithHeaders(t *testing.T) {
	// 这里必须用一个**真能判定**的 Redis：要验的是"超阈值之后会发生什么"，
	// 用 deadRedis 的话每次判定都是后端故障，测到的只会是降级路径（503）。
	// 因此走 testRedis（默认 miniredis，ZSET 与 EVAL 都支持，能真的跑通滑动窗口）。
	limiter := newLiveLimiter(t, "gate429", 200*time.Millisecond, true)
	// 阈值必须同时喂给 ratePolicy 和 rateLimitGate。
	// 只喂给后者没用：认证类的规则在策略表里就带着自己的 Limit，
	// 而策略里的值优先级高于默认值（见 Allow 里 p.Limit > 0 的分支）。
	// 只改一处的话阈值仍是 10，这个用例会一直绿着而什么也没测到。
	authDefaults := ratelimit.Defaults{Window: time.Minute, Limit: 2}
	table := ratePolicy(authDefaults)
	_, readWrite := testRateDefaults()

	var hits int
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitGate(limiter, table, readWrite)(terminal)

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/merchant/login", nil)
		r.RemoteAddr = "203.0.113.20:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if i < 2 {
			if w.Code != http.StatusOK {
				t.Fatalf("第 %d 次应放行，实际 %d（body=%s）", i, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次应回 429，实际 %d（body=%s）", i, w.Code, w.Body.String())
		}
		if w.Header().Get("Retry-After") == "" {
			t.Error("429 必须带 Retry-After，否则客户端不知道该等多久")
		}
		if w.Header().Get("X-RateLimit-Limit") == "" ||
			w.Header().Get("X-RateLimit-Remaining") == "" {
			t.Error("被限流时也应写 X-RateLimit-* 头")
		}
		// detail 必须是可外泄文案，而不是 Redis 原文或内部结构
		if !strings.Contains(w.Body.String(), "请求过于频繁") {
			t.Errorf("429 的 detail 应是可读提示，实际 %s", w.Body.String())
		}
	}
	if hits != 2 {
		t.Fatalf("业务 handler 应只被调用 2 次，实际 %d", hits)
	}
}

// testRedis 给这组限流中间件用例一个干净可用的 Redis。
//
// ★ 原来这里连不上真 Redis 就 t.Skipf。于是 TestRateLimitGate_Returns429WithHeaders
// 在无 Redis 的本机上**静默跳过** —— 而它是唯一断言"429 一定带 Retry-After 与
// X-RateLimit-* 头、detail 不泄露内部结构"的用例。跳过 = 这组响应头从没被验证过。
//
// 改成默认走 miniredis（进程内，永远能跑，ZSET 与 EVAL 都支持）；
// 设 RATELIMIT_TEST_REAL_REDIS=1 时才连真机，用来核对与真实 Redis 的行为差异。
// 连接失败一律 t.Fatal —— 跑不起来的守卫必须让门禁红，不能让门禁"绿着但什么也没检查"。
func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	if os.Getenv("RATELIMIT_TEST_REAL_REDIS") == "1" {
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			addr = "localhost:6379"
		}
		c := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = c.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Ping(ctx).Err(); err != nil {
			t.Fatalf("指定了 RATELIMIT_TEST_REAL_REDIS=1 但连不上 Redis(%s): %v", addr, err)
		}
		return c
	}

	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newLiveLimiter 建一个真能判定的限流器，带唯一前缀并在结束时清理。
//
// 唯一前缀是必须的：限流键是 ZSET，用例之间共享同一个 key 时
// 上一个用例留下的记录会让下一个用例第一次判定就被拒 ——
// 表现为"单跑绿、全跑红"。
func newLiveLimiter(t *testing.T, tag string, timeout time.Duration, failsClosed bool) *ratelimit.Limiter {
	t.Helper()
	c := testRedis(t)

	prefix := "hltest:" + tag + ":" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		cc, c2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer c2()
		iter := c.Scan(cc, 0, prefix+":*", 200).Iterator()
		for iter.Next(cc) {
			_ = c.Del(cc, iter.Val()).Err()
		}
	})
	return ratelimit.New(c, prefix, timeout, failsClosed)
}

// 未登记的路由**不受限流**（放行而不是拒绝）。
//
// 刻意 fail-open：限流是保护不是权限。拒绝会变成"每加一条路由都得记得
// 登记限流"的陷阱，漏了就是整条功能 429 且没有提示指向真实原因。
// 真正的兜底是 authGate —— 未登记的路由本来就 401。
func TestRateLimitGate_UnregisteredRoutePassesThrough(t *testing.T) {
	limiter := ratelimit.New(deadRedis(t), "gate-open", time.Second, true)
	_, authDefaults := testRateDefaults()
	authDefaults.Limit = 1

	called := 0
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitGate(limiter, ratePolicy(authDefaults), authDefaults)(terminal)

	// 用一个连 Redis 都不需要的路径：中间件在查不到策略时必须直接放行，
	// 一个 Redis 调用都不该发生（deadRedis 会失败，若走到限流就会 503）。
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/some/unregistered", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("未登记路由第 %d 次应放行，实际 %d", i, w.Code)
		}
	}
	if called != 5 {
		t.Fatalf("业务 handler 应被调用 5 次，实际 %d", called)
	}
}

// ★ fail-closed + 后端故障 → 503，**不是** 429。
//
// 回 429 会被客户端读成"你太频繁了"从而退避不重试 —— 而真实原因是
// "我们这边的限流器坏了"。那样限流器一挂，全站会被客户端主动放弃
// 到有人手工重启为止。
func TestRateLimitGate_BackendDownFailsClosedAs503(t *testing.T) {
	limiter := ratelimit.New(deadRedis(t), "gate-503", 200*time.Millisecond, true)
	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("后端故障且 fail-closed 时不该走到业务")
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitGate(limiter, table, authDefaults)(terminal)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/merchant/login", nil)
	r.RemoteAddr = "203.0.113.30:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed + 后端故障应回 503，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "127.0.0.1:1") ||
		strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("响应泄露了后端地址或系统错误原文: %s", w.Body.String())
	}
}

// fail-open + 后端故障 → 放行。
func TestRateLimitGate_BackendDownFailsOpenAs200(t *testing.T) {
	limiter := ratelimit.New(deadRedis(t), "gate-open2", 200*time.Millisecond, false)
	_, authDefaults := testRateDefaults()
	table := ratePolicy(authDefaults)

	called := 0
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitGate(limiter, table, authDefaults)(terminal)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/merchant/login", nil)
	r.RemoteAddr = "203.0.113.31:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("fail-open + 后端故障应放行，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if called != 1 {
		t.Fatalf("业务 handler 应被调用 1 次，实际 %d", called)
	}
}

// 预检请求不限流。
func TestRateLimitGate_SkipsPreflight(t *testing.T) {
	limiter := ratelimit.New(deadRedis(t), "gate-options", time.Second, true)
	_, authDefaults := testRateDefaults()

	called := 0
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitGate(limiter, ratePolicy(authDefaults), authDefaults)(terminal)

	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/merchant/login", nil)
		r.RemoteAddr = "203.0.113.40:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("预检应放行，实际 %d", w.Code)
		}
	}
	if called != 3 {
		t.Fatalf("预检应直接到达业务，实际调用 %d 次", called)
	}
}

// writeRateHeaders 的边界值。
func TestWriteRateHeaders(t *testing.T) {
	h := http.Header{}
	writeRateHeaders(h, ratelimit.Result{Allowed: true, Count: 7, Limit: 10})
	if got := h.Get("X-RateLimit-Limit"); got != "10" {
		t.Errorf("Limit = %q，期望 10", got)
	}
	if got := h.Get("X-RateLimit-Remaining"); got != "3" {
		t.Errorf("Remaining = %q，期望 3", got)
	}
	if h.Get("X-RateLimit-Reset") != "" {
		t.Error("放行时不该写 Reset")
	}

	// Remaining 不能为负：Count 超过 Limit 时（并发下的瞬时状态）
	// 负数会让客户端算出"还剩 -5"这种荒谬的退避量。
	h2 := http.Header{}
	writeRateHeaders(h2, ratelimit.Result{Allowed: false, Count: 15, Limit: 10, RetryAfter: 3 * time.Second})
	if got := h2.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("Remaining = %q，期望 0（不能为负）", got)
	}
	if got := h2.Get("X-RateLimit-Reset"); got != "3" {
		t.Errorf("Reset = %q，期望 3", got)
	}

	// RetryAfter 向上取整成秒。截断成 0 会得到 "Retry-After: 0"，
	// 那等于明确邀请客户端无脑快刷。
	h3 := http.Header{}
	writeRateHeaders(h3, ratelimit.Result{Allowed: false, Limit: 10, RetryAfter: 1500 * time.Millisecond})
	if got := h3.Get("X-RateLimit-Reset"); got != "2" {
		t.Errorf("Reset = %q，期望 2（向上取整）", got)
	}
}

// 构造一个必然连不上的 Redis 客户端。
// MaxRetries: -1 是必需的：go-redis 默认重试 3 次并指数退避，
// 会让每个用例多花好几秒，而我们要的正是"立刻失败"这个语义。
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}
