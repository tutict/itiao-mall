package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
)

// 这一组测试盯的是一个很隐蔽的坑：
// 预检请求（OPTIONS）按规范**不带** Authorization 头，而 authGate 是默认拒绝的。
// 如果 CORS 排在 authGate 之后，或者 isPublicRoute 不放行 OPTIONS，
// 每一个带 Authorization 的跨域请求都会先撞一个 401 —— 而且用 curl 逐个测接口时
// 完全看不出来，只有真实浏览器会炸。所以这里必须把两者放在**同一条链**上测。

func testChain(cors CORSConfig, issuer *auth.TokenIssuer, reached *bool) http.Handler {
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
	// 顺序与 NewRouter 保持一致：CORS 在 authGate 之前。
	return CORS(cors)(authGate(issuer, "mock")(terminal))
}

func TestCORS_PreflightFromAllowedOrigin(t *testing.T) {
	reached := false
	h := testChain(CORSConfig{AllowLocalhost: true}, auth.NewTokenIssuer("s", time.Minute), &reached)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8081" {
		t.Fatalf("应回显具体 origin，实际 %q", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatal("缺少 Vary: Origin —— 中间缓存会把一个站的响应喂给另一个站")
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatal("Allow-Headers 里必须有 Authorization，否则浏览器不会放行带 token 的请求")
	}
	if reached {
		t.Fatal("预检不应进入业务处理器")
	}
}

func TestCORS_PreflightFromDisallowedOriginGetsNoGrant(t *testing.T) {
	reached := false
	// 关掉 localhost 便利开关，且不给任何精确来源
	h := testChain(CORSConfig{}, auth.NewTokenIssuer("s", time.Minute), &reached)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("未授权来源绝不能拿到 Allow-Origin，实际 %q", got)
	}
	// 未授权来源的响应体（无 ACAO）与授权来源的响应体内容不同但 URL 相同，
	// 所以 Vary 也必须出现在这里，否则共享缓存会把这一份喂给合法来源。
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatal("未授权来源的响应同样需要 Vary: Origin，否则共享缓存会串味")
	}
}

// 这个测试盯的是 Vary 的"无条件"性质：它必须出现在放行与不放行**两条**分支上。
// 只在放行分支加 Vary 是个很常见的疏漏，而它的症状只会在有共享缓存时才显形。
func TestCORS_VaryOriginIsUnconditional(t *testing.T) {
	cors := CORSConfig{AllowedOrigins: []string{"https://shop.example.com"}}
	issuer := auth.NewTokenIssuer("s", time.Minute)

	cases := []struct {
		name   string
		origin string
	}{
		{"放行来源", "https://shop.example.com"},
		{"未放行来源", "https://evil.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := testChain(cors, issuer, &reached)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
				t.Fatalf("%s 的响应缺少 Vary: Origin", tc.name)
			}
		})
	}

	// 反过来：压根没有 Origin 的请求（原生 App / curl）不该被塞进这个缓存维度。
	reached := false
	h := testChain(cors, issuer, &reached)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Origin") {
		t.Fatalf("非跨域请求不该带 Vary: Origin，实际 %q", got)
	}
}

func TestCORS_ExactOriginIsHonoured(t *testing.T) {
	reached := false
	h := testChain(
		CORSConfig{AllowedOrigins: []string{"https://shop.example.com"}},
		auth.NewTokenIssuer("s", time.Minute), &reached,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://shop.example.com" {
		t.Fatalf("精确登记的来源应被放行，实际 %q", got)
	}
	if !reached {
		t.Fatal("实际请求应进入业务处理器")
	}
}

// 没有 Origin 的请求（原生 App、curl、同源页面）不该被加上任何 CORS 头。
func TestCORS_NonCrossOriginRequestUntouched(t *testing.T) {
	reached := false
	h := testChain(CORSConfig{AllowLocalhost: true}, auth.NewTokenIssuer("s", time.Minute), &reached)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("非跨域请求不应带 CORS 头，实际 %q", got)
	}
	if !reached {
		t.Fatal("请求应正常进入业务处理器")
	}
}

// 关键回归测试：预检必须能穿过默认拒绝的 authGate；
// 同时 authGate 本身不能因此被削弱 —— 没带 token 的真实请求仍然要 401。
func TestCORSPreflightPassesAuthGateButRealRequestStillFails(t *testing.T) {
	issuer := auth.NewTokenIssuer("test-secret", time.Minute)

	reached := false
	h := testChain(CORSConfig{AllowLocalhost: true}, issuer, &reached)

	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/me", nil)
	preflight.Header.Set("Origin", "http://localhost:8081")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	preRec := httptest.NewRecorder()
	h.ServeHTTP(preRec, preflight)

	if preRec.Code == http.StatusUnauthorized {
		t.Fatal("预检被 authGate 拦成 401 —— 所有跨域请求都会因此失败")
	}

	real := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	real.Header.Set("Origin", "http://localhost:8081")
	realRec := httptest.NewRecorder()
	h.ServeHTTP(realRec, real)

	if realRec.Code != http.StatusUnauthorized {
		t.Fatalf("没带 token 的真实请求仍应 401，实际 %d", realRec.Code)
	}
}

// 纵深防御：即使将来有人调整了中间件顺序，OPTIONS 也不该被认证拦下。
func TestPolicyFor_OptionsAlwaysPublic(t *testing.T) {
	table := routePolicy("mock")
	for _, path := range []string{"/api/v1/me", "/api/v1/orders", "/api/v1/cart", "/api/v1/anything/new"} {
		if lv := policyFor(table, http.MethodOptions, path).level; lv != levelPublic {
			t.Fatalf("OPTIONS %s 应被放行（预检不带凭证），实际 level=%d", path, lv)
		}
	}
	// 反向确认：同一个路径的 GET 仍然受保护，别把门开太大。
	if lv := policyFor(table, http.MethodGet, "/api/v1/me").level; lv != levelAnyRole {
		t.Fatalf("/api/v1/me 的 GET 应是「任意已登录」而不是公开，实际 level=%d", lv)
	}
}
