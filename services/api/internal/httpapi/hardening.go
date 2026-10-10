package httpapi

import (
	"net/http"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
)

// 本文件集中放"与业务无关、但决定攻击面大小"的 HTTP 层加固。
//
// 单独成文件而不是塞进 router.go：这些是**全局策略**，改动它们影响每一类请求。
// 放在一个文件里，评审时能一眼看清楚"我们对外公开放了哪些口子"。

// securityHeaders 是要无条件发出的头。用 map 而不是逐条 Set，
// 是因为下面还要在**每次写响应时**按同一份清单再覆盖一遍 ——
// 两处清单必须来自同一个源，否则迟早出现"加了一个头但只有一半路径生效"。
var securityHeaders = map[string]string{
	"X-Content-Type-Options":       "nosniff",
	"X-Frame-Options":              "DENY",
	"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	"Referrer-Policy":              "no-referrer",
	"Cross-Origin-Resource-Policy": "same-origin",
}

// SecurityHeaders 统一给所有响应加上安全响应头。
//
// 为什么一个纯 JSON API 也需要这些 —— 逐条说清楚，避免以后被当成"抄来的仪式"
// 删掉：
//
//   - X-Content-Type-Options: nosniff
//     没有它，浏览器会**忽略** Content-Type 去嗅探内容。一个返回 JSON 的 404
//     只要内容恰好长得像 HTML，就会被当 HTML 渲染。而我们的错误体里有
//     `type`/`title` 字段（RFC7807），是可控输入能影响到的位置 ——
//     嗅探 + 渲染 = XSS 的经典组合。
//
//   - X-Frame-Options: DENY + CSP frame-ancestors 'none'
//     两个都写是因为前者是旧标准、后者是新标准，覆盖面不同。
//     防的是点击劫持：把接口地址嵌进 iframe 诱导用户点击。
//
//   - Content-Security-Policy: default-src 'none'
//     我们**从来**不返回 HTML/脚本，所以完全禁掉资源加载没有任何代价。
//     这条是"万一将来某个响应被当成 HTML 解析"时的最后一道闸门。
//
//   - Referrer-Policy: no-referrer
//     避免 URL 里的查询参数顺着 Referer 头漏给第三方（订单号、分页游标这类
//     都会出现在 query 里）。
//
//   - Cross-Origin-Resource-Policy: same-origin
//     阻止别的站点用 <script>/<img> 直接引用我们的响应做旁路探测
//     （经典的 CORB/侧信道信息泄露）。
//
// 刻意**不**在这里加 Strict-Transport-Security：HSTS 要挂在域名级的 TLS 终止点上，
// 而本服务通常跑在同机 HTTP 后面（本地 :8080、容器内 :8080），由边缘/网关做 TLS。
// 在这里加要么无效（HTTP 响应里的 HSTS 会被浏览器忽略），要么在开发环境把
// localhost 钉成 HTTPS 而让人无法调试。它属于部署清单，不属于应用。
//
// # 为什么要在写响应时再覆盖一遍
//
// 只在 next.ServeHTTP 之前 Set 是不牢的：下游任何一段代码
// （handler、错误处理器、将来某个第三方中间件）写一个同名的头，
// 都会覆盖掉我们的值。而安全头一旦被覆盖，它的**声明性**就失效了 ——
// 我们无法再说"浏览器一定收到了 nosniff"。
//
// 所以这里包了一层 ResponseWriter，在 WriteHeader 真正落地的**那一刻**
// 把清单重新 Set 一遍（此时头还没发给客户端，Set 仍然有效）。
// 这样无论下游做了什么，最终发出的都是这份清单。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range securityHeaders {
			w.Header().Set(k, v)
		}
		next.ServeHTTP(&enforcingWriter{ResponseWriter: w}, r)
	})
}

// enforcingWriter 在 WriteHeader 落地前把安全头刷成权威值。
type enforcingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *enforcingWriter) WriteHeader(code int) {
	w.commit()
	w.ResponseWriter.WriteHeader(code)
}

func (w *enforcingWriter) Write(b []byte) (int, error) {
	// 没有显式 WriteHeader 时，第一次 Write 隐含 200。
	// 不在这里 commit 的话，那条路径会完全绕过覆盖逻辑。
	w.commit()
	return w.ResponseWriter.Write(b)
}

func (w *enforcingWriter) commit() {
	if w.wrote {
		return
	}
	w.wrote = true
	for k, v := range securityHeaders {
		w.Header().Set(k, v)
	}
}

// Unwrap 让 http.ResponseController（Go 1.20+）能找到原始 writer，
// 从而 Flush / Hijack 等可选能力不会因为这一层包装而丢失。
// 少了这个方法，将来加 SSE 或 WebSocket 时会遇到"莫名其妙不工作"的问题。
func (w *enforcingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// DefaultMaxBodyBytes 是请求体默认上限（1 MiB）。
//
// 定这个值的依据：我们最大的合法请求体是「创建商品 + 若干 SKU」，实测在几 KB 量级；
// 1 MiB 给了两个数量级的余量，同时把最坏情况钉在了一个可以接受的数字上。
// 图片走的是 URL 引用而不是 base64 内联 —— 如果哪天改成内联上传，这个值要重新算。
const DefaultMaxBodyBytes int64 = 1 << 20

// BodyLimit 限制请求体大小，并拒绝长度未知的请求体。
//
// 为什么必须有这一层：oapi-codegen 生成的解码逻辑是
// `json.NewDecoder(r.Body).Decode(&body)` —— 它会把 body **整个**读进来再做解析。
// 全库没有任何一处限制，意味着任何人 POST 一个 1 GiB 的 body 到公开的登录接口
// （不需要认证，authGate 只读 Authorization 头、不看 body），就能让这台机器分配
// 1 GiB。几个并发连接就够把服务打崩 —— 这是最廉价的一类 DoS，不需要任何技巧。
//
// Server.ReadTimeout（15s）挡不住它：15 秒内足够在千兆网卡上灌进接近 2 GB。
//
// 三道检查，缺一不可
// ----------------
//
//  1. **有 body 的方法必须带 Content-Length**，否则 411。
//     Transfer-Encoding: chunked 的请求在**读完之前**根本不知道有多大，
//     任何"先看长度再决定收不收"的方案对它都无效。直接拒掉是最干净的做法：
//     这是 JSON API，不是流式上传服务，没有一条业务路径需要 chunked 请求体。
//
//  2. Content-Length 超过上限 → 413，且**一个字节都不读**。
//     这是快路径：省带宽、省 CPU，也让客户端立刻知道原因。
//
//  3. 仍然用 http.MaxBytesReader 包一层，作为纵深防御。
//     第 1、2 道都依赖客户端**如实申报** Content-Length。
//     一个撒谎的客户端（声明 100 字节、实发 1 GiB）能绕过前两道 ——
//     这道保证读取在真正超过上限时立即失败，内存占用被钉死在上限附近。
//     此时下游会拿到解码错误并回 400：状态码不如 413 精确，
//     但**安全目标（不分配超量内存）已经达成**，这是可以接受的折中。
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expectsBody(r.Method) {
				if r.ContentLength < 0 {
					WriteProblem(w, r, domain.ErrLengthRequired)
					return
				}
				if r.ContentLength > maxBytes {
					WriteProblem(w, r, domain.ErrPayloadTooLarge)
					return
				}
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// expectsBody 回答"这个方法按约定会不会带请求体"。
//
// 只认 POST / PUT / PATCH。GET / HEAD / DELETE / OPTIONS 的请求体在 HTTP 语义里
// 没有定义，各家客户端与代理对它们的处置不一致（有的直接丢弃、有的转发但服务端忽略）。
// 对它们做长度校验会误伤 —— 而这些方法本来就不读 body，也就没有内存风险。
func expectsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}
