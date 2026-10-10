package service

import (
	"errors"
	"net"
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
		// 这条原本写的是 http://localhost:8080/...，但它的**本意**是覆盖
		// "带端口"。加主机名白名单之后 localhost 被拒了 —— 于是它变成了
		// 一条"声称测端口、实际测 localhost"的用例。换成公网域名 + 端口，
		// 保住端口这一维；localhost 的拒绝由下面 TestValidateCoverURL_RejectsNonPublicHosts 覆盖。
		{"带端口", "http://cdn.example.com:8080/static/a.jpg", false},

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

// TestValidateCoverURL_RejectsNonPublicHosts 覆盖主机名这一层。
//
// 这一层挡的不是 XSS（scheme 那层已经挡了），而是"封面必须是公网可达地址"
// 这条业务前提。要覆盖的形态比想象的杂：
//   - 云元数据地址 169.254.169.254（这是这一层最实在的价值）
//   - RFC1918 三段私有网段、环回、未指定、链路本地、CGNAT、TEST-NET、组播、保留段
//   - IPv6 的环回 / ULA / 链路本地 / 文档段，以及 IPv4-mapped 写法
//   - 本机域名，且必须覆盖**大小写**与**末尾根点**这两种绕过写法
//     （不做归一化的话 "LOCALHOST" 和 "localhost." 会直接放行 —— 这是
//     这一类检查最常见的写坏方式）
//
// 每一类都单独列一条：合并成大用例的话，某一类漏掉时错误信息会指向
// "整体失败"，排查时还得再拆一遍。
func TestValidateCoverURL_RejectsNonPublicHosts(t *testing.T) {
	cases := []struct{ name, raw string }{
		// ---- IPv4：私有与保留 ----
		{"RFC1918 10/8", "http://10.0.0.5/a.png"},
		{"RFC1918 172.16/12", "http://172.16.3.4/a.png"},
		{"RFC1918 192.168/16", "http://192.168.1.1/a.png"},
		{"环回 127.0.0.1", "http://127.0.0.1/a.png"},
		{"环回段上界", "http://127.255.255.254/a.png"},
		{"未指定 0.0.0.0", "http://0.0.0.0/a.png"},
		{"链路本地 169.254/16", "http://169.254.1.1/a.png"},
		{"云元数据 169.254.169.254", "http://169.254.169.254/latest/meta-data/"},
		{"CGNAT 100.64/10", "http://100.64.0.1/a.png"},
		{"IETF 专用 192.0.0/24", "http://192.0.0.1/a.png"},
		{"基准测试 198.18/15", "http://198.18.0.1/a.png"},
		{"TEST-NET-2", "http://198.51.100.9/a.png"},
		{"TEST-NET-3", "http://203.0.113.9/a.png"},
		{"组播 224/4", "http://224.0.0.1/a.png"},
		{"保留 240/4", "http://240.0.0.1/a.png"},
		{"受限广播 255.255.255.255", "http://255.255.255.255/a.png"},
		// ---- IPv4：带端口 / 带凭据 / 带查询串时不能漏 ----
		//
		// 这几条专门盯"用 u.Host 而不是 u.Hostname()"这个写错方式：
		// "127.0.0.1:8080" 喂给 net.ParseIP 会解析失败，于是内网地址
		// 会被当成普通域名放行 —— 白名单开着、洞在后面。
		{"环回带端口", "http://127.0.0.1:8080/a.png"},
		{"内网带端口与查询串", "http://192.168.0.10:9000/x.png?w=1"},
		{"内网带用户信息", "http://user:pw@10.0.0.1/a.png"},
		// ---- IPv6 ----
		{"IPv6 环回", "http://[::1]/a.png"},
		{"IPv6 未指定", "http://[::]/a.png"},
		{"IPv6 ULA", "http://[fd00::1]/a.png"},
		{"IPv6 链路本地", "http://[fe80::1]/a.png"},
		{"IPv6 文档段", "http://[2001:db8::1]/a.png"},
		{"IPv4-mapped 环回", "http://[::ffff:127.0.0.1]/a.png"},
		// ---- 本机 / 私网域名 ----
		{"localhost", "http://localhost/a.png"},
		{"localhost 带端口", "http://localhost:8080/a.png"},
		{"LOCALHOST 大写", "http://LOCALHOST/a.png"},
		{"localhost 末尾根点", "http://localhost./a.png"},
		{"localhost 子域", "http://images.localhost/a.png"},
		{"mDNS .local", "http://printer.local/a.png"},
		{"私有 TLD .internal", "http://storage.internal/a.png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCoverURL(tc.raw)
			if err == nil {
				t.Fatalf("期望拒绝 %q，实际放行 —— 这个地址不可能是公网图床", tc.raw)
			}
			// 哨兵必须是 ErrValidation（→ 400），不能是别的：
			// 落成 5xx 会让商户以为是服务端故障而反复重试同一个地址。
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%q 被拒但哨兵不是 ErrValidation：%v", tc.raw, err)
			}
			// 拒绝理由必须点明原因。只说"地址非法"的话，商户会以为是自己
			// 少写了斜杠，然后开始瞎试。
			if msg := domain.ExposeUserFacing(err); !strings.Contains(msg, "内网") {
				t.Errorf("%q 的拒绝理由没点明是内网地址，实际 %q", tc.raw, msg)
			}
		})
	}
}

// TestValidateCoverURL_AllowsPublicHosts 是上面那条的**对照组**。
//
// 为什么必须有：一条"一律拒绝"的坏实现能让 RejectsNonPublicHosts 全绿。
// 那些边界值也不是凑数 —— 每一条都紧贴着某个被拦网段的**外侧**，
// 用来抓"掩码写宽了"这类错误（比如把 100.64.0.0/10 写成 /8，
// 或把 198.18.0.0/15 写成 /14）。只测"内网被拒"是抓不到这种错的：
// 掩码写宽之后内网照样被拒，只是顺带把一大片公网也拒了。
func TestValidateCoverURL_AllowsPublicHosts(t *testing.T) {
	cases := []string{
		"https://cdn.example.com/p/1.png",
		"https://s3.ap-southeast-1.amazonaws.com/bucket/a.png",
		"https://img.example.com:8443/a.png",
		"http://8.8.8.8/a.png",
		"http://9.255.255.255/a.png",   // 10/8 外侧（下）
		"http://11.0.0.0/a.png",        // 10/8 外侧（上）
		"http://100.63.255.255/a.png",  // 100.64/10 外侧（下）
		"http://100.128.0.0/a.png",     // 100.64/10 外侧（上）
		"http://172.15.255.255/a.png",  // 172.16/12 外侧（下）
		"http://172.32.0.0/a.png",      // 172.16/12 外侧（上）
		"http://192.167.255.255/a.png", // 192.168/16 外侧（下）
		"http://192.169.0.0/a.png",     // 192.168/16 外侧（上）
		"http://198.17.255.255/a.png",  // 198.18/15 外侧（下）
		"http://198.20.0.0/a.png",      // 198.18/15 外侧（上）
		"http://203.0.112.255/a.png",   // TEST-NET-3 外侧（下）
		"http://203.0.114.0/a.png",     // TEST-NET-3 外侧（上）
		"http://223.255.255.255/a.png", // 组播段外侧（下）
		"http://[2606:4700::1]/a.png",  // 公网 IPv6（ULA 之外）
		"http://example.com/a.png",     // 名字里带 local 但不是 .local 后缀
		"http://localhost.example.com/a.png",
	}

	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if err := validateCoverURL(raw); err != nil {
				t.Fatalf("公网地址被误拒 %q：%v —— 过宽的网段表会挡住真实图床", raw, err)
			}
		})
	}
}

// TestIPIsNonPublic_Boundaries 直接钉住网段表的**每条边界**。
//
// 与上面两条的区别：那两条从 URL 进来，这条直接测判定函数。
// 好处是能把"紧贴边界的那一个地址"逐个列出来 —— 掩码写错一位（/10 写成 /9）
// 在中间值上是看不出来的，只有边界会立刻露馅。
// 顺带覆盖 IPv4-mapped 写法：标准库的 To4() 会把它收回 IPv4 再判定，
// 这条断言把"依赖标准库语义"这个决定固定下来。
func TestIPIsNonPublic_Boundaries(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// 10/8
		{"10.0.0.0", true}, {"10.255.255.255", true},
		{"9.255.255.255", false}, {"11.0.0.0", false},
		// 100.64/10
		{"100.63.255.255", false}, {"100.64.0.0", true},
		{"100.127.255.255", true}, {"100.128.0.0", false},
		// 172.16/12
		{"172.15.255.255", false}, {"172.16.0.0", true},
		{"172.31.255.255", true}, {"172.32.0.0", false},
		// 192.168/16
		{"192.167.255.255", false}, {"192.168.0.0", true},
		{"192.168.255.255", true}, {"192.169.0.0", false},
		// 198.18/15
		{"198.17.255.255", false}, {"198.18.0.0", true},
		{"198.19.255.255", true}, {"198.20.0.0", false},
		// 169.254/16（元数据）
		{"169.253.255.255", false}, {"169.254.0.0", true},
		{"169.254.169.254", true}, {"169.255.0.0", false},
		// 203.0.113/24（TEST-NET-3）
		{"203.0.112.255", false}, {"203.0.113.0", true},
		{"203.0.113.255", true}, {"203.0.114.0", false},
		// 组播 224/4 与保留 240/4
		{"223.255.255.255", false}, {"224.0.0.0", true},
		{"239.255.255.255", true}, {"240.0.0.0", true},
		{"255.255.255.255", true},
		// 环回与未指定
		{"127.0.0.1", true}, {"0.0.0.0", true}, {"8.8.8.8", false},
		// IPv6
		{"::1", true}, {"::", true},
		{"fbff::1", false}, {"fc00::1", true}, {"fdff::1", true},
		{"fe7f::1", false}, {"fe80::1", true}, {"febf::1", true}, {"fec0::1", false},
		{"2001:db7::1", false}, {"2001:db8::1", true},
		{"2606:4700::1", false},
		// IPv4-mapped：必须按内层 IPv4 判定
		{"::ffff:10.0.0.1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:8.8.8.8", false},
	}

	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("测试自身写错了：%q 不是合法 IP", tc.ip)
			}
			if got := ipIsNonPublic(ip); got != tc.want {
				t.Fatalf("ipIsNonPublic(%s) = %v，期望 %v —— 网段表的边界写错了", tc.ip, got, tc.want)
			}
		})
	}
}

// TestRejectNonPublicHost_NormalizesInput 把"归一化"这件事单独钉住。
//
// 大小写与末尾根点的处理如果写在调用方而不是判定函数里，任何新增的调用点
// 都会漏掉其中一条 —— 而漏掉的表现是"检查看起来在跑、某个写法直接穿过"。
func TestRejectNonPublicHost_NormalizesInput(t *testing.T) {
	rejected := []string{
		"localhost", "LOCALHOST", "LocalHost",
		"localhost.", "LOCALHOST.",
		"a.localhost", "A.LOCALHOST",
		"x.local.", "x.internal", "X.INTERNAL",
		"127.0.0.1", "10.0.0.1", "::1",
	}
	for _, h := range rejected {
		if err := rejectNonPublicHost(h); err == nil {
			t.Errorf("rejectNonPublicHost(%q) 放行了，期望拒绝", h)
		}
	}

	allowed := []string{
		"", // 空值由调用方的前置检查负责，这里不该重复报错
		"cdn.example.com",
		"example.com",
		"localhost.example.com", // 含 "localhost" 但不是它
		"local",                 // 裸 "local" 不是 .local 子域
		"internal",
		"mylocal.example.com",
		"8.8.8.8",
		"2606:4700::1",
	}
	for _, h := range allowed {
		if err := rejectNonPublicHost(h); err != nil {
			t.Errorf("rejectNonPublicHost(%q) 拒绝了合法域名：%v", h, err)
		}
	}
}
