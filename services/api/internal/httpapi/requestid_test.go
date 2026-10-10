package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 跑一次 RequestID 中间件，返回响应头里的 X-Request-ID。
func runRequestID(t *testing.T, incoming string) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if incoming != "" {
		req.Header.Set("X-Request-ID", incoming)
	}

	rec := httptest.NewRecorder()
	RequestID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	return rec.Header().Get("X-Request-ID")
}

// ★ 客户端自带的 X-Request-ID 会被写进响应头与**全部日志**。
//
// 原样采信等于让调用方决定我们的日志长什么样：超长串能把日志行撑爆，
// 特殊字符能伪造关联（日志注入）。这里要求不合规的值被**丢弃并重新生成**。
func TestRequestID_RejectsUntrustedValues(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"超长（200 字符）", strings.Repeat("A", 200)},
		{"含空格", "abc def"},
		{"含中文", "订单-123"},
		{"路径穿越形状", "../../evil"},
		{"含控制字符", "abc\x01def"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRequestID(t, tc.in)

			if got == tc.in {
				t.Fatalf("原始值被原样采信了（%q）—— 调用方可以决定我们的日志内容", tc.in)
			}
			// 必须换成一个**正常形状**的 id，而不是变成空串或别的东西。
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("响应头 X-Request-ID = %q，期望是重新生成的 UUID（err=%v）", got, err)
			}
		})
	}
}

// 合规的值必须原样保留 —— 上一条的对照。
//
// 没有它，"把 request id 改成永远重新生成"也能让上面全绿，
// 但那等于把调用方自己的链路追踪 id 丢掉，跨系统关联直接断掉。
func TestRequestID_PreservesWellFormedValue(t *testing.T) {
	const id = "trace-01H_ZY-abc123"

	if got := runRequestID(t, id); got != id {
		t.Errorf("合规的 request id 应原样保留：得到 %q，期望 %q", got, id)
	}
}

// 没带这个头时必须生成一个，不能留空。
func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	got := runRequestID(t, "")
	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("缺省时应生成 UUID，实际 %q（err=%v）", got, err)
	}
}
