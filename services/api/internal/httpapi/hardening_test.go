package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler 是一个"正常业务"，只是把请求读完并回 200。
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
}

// TestSecurityHeaders_AppliedToEveryResponse 守的是"覆盖面"。
//
// 安全头最常见的失效形态不是"值写错"，而是**某些响应路径没有它们**。
// 最需要这些头的恰好是错误响应（内容可控、可能被浏览器渲染），
// 所以这里连 4xx 也要断言一遍。
func TestSecurityHeaders_AppliedToEveryResponse(t *testing.T) {
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	}

	cases := []struct {
		name string
		code int
	}{
		{"200 正常响应", http.StatusOK},
		{"404 错误响应", http.StatusNotFound},
		{"500 内部错误", http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				WriteJSON(w, tc.code, map[string]string{"k": "v"})
			})
			rec := httptest.NewRecorder()
			SecurityHeaders(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

			for k, v := range want {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s: 期望 %q，实际 %q", k, v, got)
				}
			}
		})
	}
}

// TestSecurityHeaders_OverwriteInsteadOfAppend 守的是"下游不能改写安全头"。
//
// 只在 next.ServeHTTP 之前 Set 是不牢的：下游任何一段代码写一个同名的头，
// 都会覆盖掉我们的值 —— 而安全头一旦被覆盖，"浏览器一定收到了 nosniff"
// 这句话就不成立了，它的声明性价值随之归零。
//
// 两种改写手法都要挡住：
//   - Set：直接覆盖
//   - Add：变成多值并存（浏览器对 X-Frame-Options 多值的处置是实现相关的，
//     有的取第一个，于是宽松值生效）
func TestSecurityHeaders_OverwriteInsteadOfAppend(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(w http.ResponseWriter)
	}{
		{"下游用 Set 覆盖", func(w http.ResponseWriter) {
			w.Header().Set("X-Frame-Options", "ALLOWALL")
		}},
		{"下游用 Add 追加", func(w http.ResponseWriter) {
			w.Header().Add("X-Frame-Options", "ALLOWALL")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.mutate(w)
				w.WriteHeader(http.StatusOK)
			})
			rec := httptest.NewRecorder()
			SecurityHeaders(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

			got := rec.Header().Values("X-Frame-Options")
			if len(got) != 1 {
				t.Fatalf("X-Frame-Options 应只有 1 个值，实际 %d 个：%v（多值并存时浏览器可能取到宽松的那个）", len(got), got)
			}
			if got[0] != "DENY" {
				t.Fatalf("期望被覆盖成 DENY，实际 %q", got[0])
			}
		})
	}
}

// TestSecurityHeaders_NoExplicitWriteHeader 守的是"隐式 200"这条路。
//
// 只处理 WriteHeader 的实现会漏掉"handler 直接 Write 而不调 WriteHeader"的情况
// —— 那时状态码由 net/http 在第一次 Write 时隐式补 200，覆盖逻辑会被整个跳过。
// 这是安全头实现里最典型的一个漏洞：测试只覆盖显式路径时看不出来。
func TestSecurityHeaders_NoExplicitWriteHeader(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Content-Type-Options", "sniff-away")
		_, _ = w.Write([]byte("body"))
	})
	rec := httptest.NewRecorder()
	SecurityHeaders(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("隐式 200 应保持，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("隐式 200 路径上安全头也应被覆盖，实际 %q", got)
	}
	if rec.Body.String() != "body" {
		t.Fatalf("响应体被破坏，实际 %q", rec.Body.String())
	}
}

// TestBodyLimit_RejectsOversizedBeforeReading 是这一组里最关键的一条。
//
// 断言两件事：
//  1. 回 413（不是 400 —— 400 会让客户端以为"改改参数就能过"而拿同样体积重试）
//  2. 下游 handler **根本没被调用**
//
// 第 2 条才是"没读 body"的证据。只看状态码的话，一个"先读完再拒绝"的实现
// 同样能返回 413 —— 而那种实现保护不了内存，正是我们要防的东西。
func TestBodyLimit_RejectsOversizedBeforeReading(t *testing.T) {
	const limit = 1024
	reached := false

	body := bytes.NewReader(bytes.Repeat([]byte("a"), limit+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/products", body)
	if req.ContentLength <= limit {
		t.Fatalf("构造失败：ContentLength=%d 应大于 %d", req.ContentLength, limit)
	}

	rec := httptest.NewRecorder()
	BodyLimit(limit)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("期望 413，实际 %d（400 会让客户端拿同样的体积反复重试）", rec.Code)
	}
	if reached {
		t.Fatal("下游 handler 被调用了 —— 说明 body 已经被读进内存，这一层没有起到防护作用")
	}
}

// TestBodyLimit_RejectsUnknownLength 守的是 chunked 编码这条路。
//
// Transfer-Encoding: chunked 的请求在读完之前根本不知道有多大，
// "先看长度再决定收不收"对它完全无效。而它正是绕过 Content-Length 检查的
// 标准手法 —— 必须在读之前就拒掉。
func TestBodyLimit_RejectsUnknownLength(t *testing.T) {
	reached := false
	req := httptest.NewRequest(http.MethodPost, "/api/v1/products", strings.NewReader("{}"))
	req.ContentLength = -1 // 模拟 chunked
	req.TransferEncoding = []string{"chunked"}

	rec := httptest.NewRecorder()
	BodyLimit(1024)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusLengthRequired {
		t.Fatalf("期望 411，实际 %d", rec.Code)
	}
	if reached {
		t.Fatal("chunked 请求不该到达 handler")
	}
}

// TestBodyLimit_AllowsNormalRequest 是防止"把门关死"的那一半。
//
// 一个永远拒绝的实现同样能让上面两条测试变绿。必须有这条下限断言。
func TestBodyLimit_AllowsNormalRequest(t *testing.T) {
	const limit = 1024
	reached := false

	payload := []byte(`{"title":"测试商品","skus":[{"price_cents":100}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/products", bytes.NewReader(payload))

	rec := httptest.NewRecorder()
	BodyLimit(limit)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("正常请求应通过，实际 %d", rec.Code)
	}
	if !reached {
		t.Fatal("正常请求没有到达 handler")
	}
}

// TestBodyLimit_ExactLimitPasses 钉住边界：恰好等于上限应当**通过**。
//
// 写成 `>=` 就会把正好 1 MiB 的合法请求挡掉，而这类差一错误在
// 只有"超限"和"正常"两条测试时是抓不到的。
func TestBodyLimit_ExactLimitPasses(t *testing.T) {
	const limit = 512
	reached := false

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(bytes.Repeat([]byte("a"), limit)))
	if req.ContentLength != limit {
		t.Fatalf("构造失败：ContentLength=%d，期望 %d", req.ContentLength, limit)
	}

	rec := httptest.NewRecorder()
	BodyLimit(limit)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("恰好等于上限应通过，实际 code=%d reached=%v", rec.Code, reached)
	}
}

// TestBodyLimit_UntrustedContentLength 守的是"客户端撒谎"这条路。
//
// 前两道检查都依赖客户端**如实申报** Content-Length。这条测试模拟一个
// 声明 10 字节、实发 100 KiB 的客户端 —— 前两道会被绕过，
// 由 http.MaxBytesReader 兜住：下游读到的数据不会超过上限。
//
// 这里断言的是**实际读到的字节数**，不是状态码。因为这条路的状态码
// 取决于下游怎么处理读错误（我们的 handler 会回 400），
// 而安全目标只有一个：不分配超量内存。
func TestBodyLimit_UntrustedContentLength(t *testing.T) {
	const limit = 1024
	var read int

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		read = int(n)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(bytes.Repeat([]byte("a"), 100*1024)))
	req.ContentLength = 10 // 撒谎

	rec := httptest.NewRecorder()
	BodyLimit(limit)(slow).ServeHTTP(rec, req)

	if read > limit {
		t.Fatalf("下游读到了 %d 字节，超过上限 %d —— MaxBytesReader 没兜住，内存可以被无限撑大", read, limit)
	}
}

// TestBodyLimit_GetWithoutBodyPasses 钉住方法白名单的另一半：不该误伤。
//
// 对 GET/HEAD/DELETE 做长度校验会误伤：它们的请求体在 HTTP 语义里没有定义，
// 各家客户端与代理处置不一致（有的不发 Content-Length），而这些方法本来就不读
// body，也就没有内存风险。
func TestBodyLimit_GetWithoutBodyPasses(t *testing.T) {
	reached := false
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	if req.ContentLength < 0 {
		t.Fatalf("构造失败：GET 的 ContentLength 应为非负，实际 %d", req.ContentLength)
	}

	rec := httptest.NewRecorder()
	BodyLimit(1024)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("无 body 的 GET 应通过，实际 code=%d reached=%v", rec.Code, reached)
	}
}

// TestBodyLimit_CoversEveryBodyMethod 守的是方法白名单的"完整"。
//
// 这是从一次真实的"未证明"里补出来的：M36 原本假想的危害是
// 「白名单扩大到 GET 会误伤」，但实测发现扩大之后行为完全不变
// （GET 的 ContentLength 是 0，两个检查都不触发）—— 那个变异体描述的
// 风险根本不成立，所以它永远抓不到东西。
//
// 真正会出事的恰恰是**另一个方向**：白名单漏掉 PUT/PATCH。
// 更新商品走的是 PUT，漏掉它意味着"超限 body 在更新接口上不受预检保护"，
// 而这条路径上没有任何其他测试会覆盖到。
//
// 教训记在这里：写变异体之前先问「它描述的后果真的会发生吗」，
// 否则会得到一个恒为「未证明」的假条目 —— 比没有更坏，因为它占着位置。
func TestBodyLimit_CoversEveryBodyMethod(t *testing.T) {
	const limit = 512
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			reached := false
			req := httptest.NewRequest(method, "/api/v1/merchant/products/1",
				bytes.NewReader(bytes.Repeat([]byte("a"), limit+1)))

			rec := httptest.NewRecorder()
			BodyLimit(limit)(okHandler(&reached)).ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s 的超限 body 应回 413，实际 %d —— 这个方法漏出了长度预检", method, rec.Code)
			}
			if reached {
				t.Fatalf("%s 的超限请求不该到达 handler", method)
			}
		})
	}
}

// TestBodyLimit_UnknownLengthCoveredForEveryBodyMethod 同理，411 也要逐个方法验。
func TestBodyLimit_UnknownLengthCoveredForEveryBodyMethod(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			reached := false
			req := httptest.NewRequest(method, "/x", strings.NewReader("{}"))
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}

			rec := httptest.NewRecorder()
			BodyLimit(512)(okHandler(&reached)).ServeHTTP(rec, req)

			if rec.Code != http.StatusLengthRequired {
				t.Fatalf("%s 的 chunked 请求应回 411，实际 %d", method, rec.Code)
			}
			if reached {
				t.Fatalf("%s 的 chunked 请求不该到达 handler", method)
			}
		})
	}
}

// TestBodyLimit_ZeroUsesDefault 防止"配成 0 就等于关掉防护"。
func TestBodyLimit_ZeroUsesDefault(t *testing.T) {
	reached := false
	big := bytes.NewReader(bytes.Repeat([]byte("a"), int(DefaultMaxBodyBytes)+1))
	req := httptest.NewRequest(http.MethodPost, "/x", big)

	rec := httptest.NewRecorder()
	BodyLimit(0)(okHandler(&reached)).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("BodyLimit(0) 应退回默认上限并拒绝超限请求，实际 %d", rec.Code)
	}
	if reached {
		t.Fatal("BodyLimit(0) 时下游不该被调用")
	}
}
