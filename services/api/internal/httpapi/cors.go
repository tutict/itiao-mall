package httpapi

import (
	"net/http"
	"net/url"
)

// CORSConfig 描述跨域策略。
type CORSConfig struct {
	// AllowedOrigins 是精确匹配的 origin 列表（形如 http://localhost:5173，不含路径）。
	AllowedOrigins []string
	// AllowLocalhost 放行任意 localhost / 127.0.0.1 / [::1] 端口。
	// 只在非生产环境打开 —— Expo web、Vite dev server 每次的端口都可能不一样，
	// 逐个登记不现实。生产环境必须靠 AllowedOrigins 精确配置。
	AllowLocalhost bool
}

// CORS 处理跨域。
//
// 为什么必须放在 authGate **之前**：浏览器的预检请求（OPTIONS）不带 Authorization 头 ——
// 这是规范要求，预检本来就是用来问"我能不能带这个头"的。如果让 fail-closed 的 authGate
// 先看到它，会得到一个 401，于是**所有**带 Authorization 的跨域请求全部作废。
// 这个坑很隐蔽：单独用 curl 测每个接口都是好的，只有真实浏览器会炸。
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// 不是跨域请求（原生 App、curl、同源页面都走这里），无需任何处理。
				next.ServeHTTP(w, r)
				return
			}

			// Vary: Origin 必须**无条件**加（只要是跨域请求），不能只在放行分支里加。
			// 理由：没被放行的来源拿到的是一份"没有 ACAO"的响应，它和放行来源拿到的响应
			// 内容不同但 URL 相同。如果这份响应被共享缓存（CDN / 反向代理）缓存住，
			// 之后合法来源来取，就会拿到那份没有 ACAO 的副本 —— 浏览器照样拦掉。
			// 所以缓存键必须包含 Origin，无论这次是否放行。
			w.Header().Add("Vary", "Origin")

			if cfg.originAllowed(origin, allowed) {
				h := w.Header()
				// 回显具体 origin 而不是 "*"：既要允许多个来源，又不能用通配符
				// （带凭证的通配符是非法的，而且通配符会让将来加 Cookie 会话时踩坑）。
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				h.Set("Access-Control-Max-Age", "600")
				// 刻意不设 Access-Control-Allow-Credentials：
				// 本项目用 Bearer token，不用 Cookie，不需要它。
				// 一旦设了它，就等于允许浏览器携带用户凭证，必须配精确 origin 且不许用通配符。
			}

			if r.Method == http.MethodOptions {
				// 预检在此终止，**不进**后续中间件和路由。
				// 未授权的来源也回 204 但不带 CORS 头，让浏览器自己拒绝实际请求 ——
				// 这比回 403 更标准，也不会给攻击者额外的探测信号。
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func (c CORSConfig) originAllowed(origin string, exact map[string]struct{}) bool {
	if _, ok := exact[origin]; ok {
		return true
	}
	if !c.AllowLocalhost {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
