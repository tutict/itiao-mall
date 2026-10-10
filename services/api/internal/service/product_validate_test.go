package service

import (
	"strings"
	"testing"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
)

// TestValidateCoverURL 穷举封面地址的协议面。
//
// 这个字段会进入前端渲染路径（<Image source> / <img src>，将来也可能有人
// 把它塞进 <a href>），所以它不是一个"普通文本字段"。
//
// 测试的重点是**白名单是不是真的只有两个值**：黑名单写法
// （禁止 javascript:）在下面第 3-6 条上会全部失守 —— scheme 大小写不敏感、
// 可以带前导空白、还有 data:/file:/协议相对这几种变体。
func TestValidateCoverURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		// ---- 合法 ----
		{"空字符串（封面可选）", "", false},
		{"只有空白", "   ", false},
		{"https 绝对地址", "https://cdn.example.com/p/1.png", false},
		{"http 绝对地址", "http://cdn.example.com/p/1.png", false},
		{"带查询串", "https://cdn.example.com/p/1.png?w=200&h=200", false},
		{"大写协议", "HTTPS://cdn.example.com/p/1.png", false},
		{"带端口", "http://localhost:8080/static/a.jpg", false},

		// ---- 必须拒绝：脚本与危险协议 ----
		{"javascript 协议", "javascript:alert(1)", true},
		{"javascript 大小写混淆", "JaVaScRiPt:alert(1)", true},
		{"javascript 前导空白", "  javascript:alert(1)", true},
		{"javascript 前导控制字符", "\tjavascript:alert(1)", true},
		{"data 内联文档", "data:text/html,<script>alert(1)</script>", true},
		{"data 内联图片", "data:image/svg+xml,<svg onload=alert(1)>", true},
		{"vbscript 协议", "vbscript:msgbox(1)", true},
		{"file 协议", "file:///C:/Windows/win.ini", true},

		// ---- 必须拒绝：无协议 / 协议相对 ----
		{"协议相对 URL", "//evil.example.com/x.png", true},
		{"站内相对路径", "/static/upload/a.png", true},
		{"相对路径", "a.png", true},
		{"裸域名", "cdn.example.com/a.png", true},

		// ---- 必须拒绝：http 但缺主机名 ----
		{"http 无主机名", "http:///a.png", true},
		{"https 无主机名", "https://", true},

		// ---- 必须拒绝：超长 ----
		{"超长地址", "https://cdn.example.com/" + strings.Repeat("a", maxProductCoverURLLen), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCoverURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("期望拒绝 %q，实际放行了 —— 这个值会进入前端渲染路径", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行 %q，实际拒绝: %v", tc.raw, err)
			}
			// 拒绝理由必须**对商户可读**：被拒的是"格式"，
			// 回一句笼统的"参数非法"会让商户反复试同一个值。
			if tc.wantErr {
				if msg := domain.ExposeUserFacing(err); msg == "" {
					t.Errorf("%q 被拒但没有给商户可读的理由", tc.raw)
				}
			}
		})
	}
}

// TestValidateCoverURL_ErrorMessageMentionsExpectedScheme 钉住文案质量。
//
// 商户最常犯的错是从资源管理器复制了一个本地路径。告诉他"必须以 http://
// 或 https:// 开头"比"地址非法"有用得多 —— 前者直接给出改法。
func TestValidateCoverURL_ErrorMessageMentionsExpectedScheme(t *testing.T) {
	err := validateCoverURL(`C:\Users\me\Desktop\a.png`)
	if err == nil {
		t.Fatal("本地路径应被拒绝")
	}
	msg := domain.ExposeUserFacing(err)
	if !strings.Contains(msg, "http") {
		t.Fatalf("拒绝文案应点出期望的协议形态，实际 %q", msg)
	}
}
