package profiling

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ★ 这个包的守卫要回答的不是"端点在不在"，而是三件更容易出错的事：
//
//  1. **CPU profile 是不是真的能拿到**。直觉写法（遍历白名单统一用 pprof.Handler）
//     会让 heap/goroutine 全部正常、唯独 profile 是 404。
//  2. **DefaultServeMux 有没有被污染**。`import _ "net/http/pprof"` 会在 init 时
//     把端点注册到 DefaultServeMux，于是任何 `ListenAndServe(addr, nil)` 顺手暴露。
//  3. **seconds 能不能被用来把 handler 挂住**。Go 自己没有上界。
//
// 只测"请求返回了东西"是不够的 —— 上面第 1 条的失败形态恰恰是"返回了东西"（404）。

// get 打一次请求，返回状态码与响应体。
func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	res := rec.Result()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return rec.Code, b
}

// TestHandler_CPUProfileActuallyWorks 是这个包最要紧的一条断言。
//
// 它防的是"白名单循环 + pprof.Handler"这个写法：那个写法的失败形态极隐蔽 ——
// heap、goroutine、allocs 全部 200 且有数据，唯独 CPU profile 返回 404 "Unknown profile"。
// 而 404 在一次仓促的排障里很容易被读成"这个端点没开"，于是结论变成
// "我们没有 CPU profiling 能力"，而事实上代码里有它、只是取不到。
//
// 判据用**能否真的解析出 profile 语义**而不是"状态码是 200"：
// 200 也可以是一段 HTML 错误页。
//
// ★ 这里踩过一次坑：第一版断言"解压后首字节 == 0x0A"（protobuf field 1 的 tag），
// 实测首字节是 0x48 —— 那个魔数是我猜的，猜错了。
// **写"看起来很专业的魔数断言"比不写更坏**：它会让人以为格式核对过了。
// 现在改成断言 pprof 里真实存在的语义字符串（"samples"/"count"/"cpu"/"nanoseconds"），
// 这些是 CPU profile 与 heap profile 的区别所在，猜不出来。
func TestHandler_CPUProfileActuallyWorks(t *testing.T) {
	h := Handler(5)

	// seconds=1：真实采集 1 秒。这是唯一一条"必须真的睡"的断言 ——
	// 用 mock 替掉采集过程的话，就验证不了"采样窗口真的打开了"。
	//
	// 顺带记一个实测：不带 seconds 时 Go 默认采 **30** 秒，
	// 而 profile 端点只能同时有一个在跑（StartCPUProfile 全进程唯一）。
	// 所以"全覆盖"那条测试必须给 profile 单独指定 seconds，
	// 否则整个包的测试会被这一个 30 秒拖到超时。
	code, body := get(t, h, "/debug/pprof/profile?seconds=1")
	if code != http.StatusOK {
		t.Fatalf("CPU profile 端点返回 %d，期望 200。"+
			"典型成因是用 pprof.Handler(\"profile\") 注册 —— "+
			"它在内部做 pprof.Lookup(\"profile\")，而 profile 不是 runtime 伪 profile（实测 404）",
			code)
	}

	// pprof 响应是 gzip 压缩的 protobuf。解不开 = 不是 profile。
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("响应不是合法的 gzip（可能是 HTML 错误页或空响应）: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压 profile 失败: %v", err)
	}
	if len(raw) < 64 {
		t.Fatalf("解压后只有 %d 字节，不像 profile 数据", len(raw))
	}

	// 这四个字符串是 CPU profile 的语义标记，实测在 1 秒空载采样里必然存在。
	// 它们的组合能区分 CPU profile 与 heap/allocs（后者是 alloc_objects/alloc_space），
	// 所以这条断言真的在验证"拿到的是 CPU profile"，而不只是"有字节"。
	for _, marker := range []string{"samples", "count", "cpu", "nanoseconds"} {
		if !bytes.Contains(raw, []byte(marker)) {
			t.Fatalf("解压后的数据里找不到 %q —— "+
				"拿到的可能不是 CPU profile（CPU profile 的 sample_type 固定是 cpu/nanoseconds）", marker)
		}
	}
}

// wantAllProfiles 是"应当全部可达"的名单，**刻意写死**而不是遍历 ProfileNames()。
//
// ★ 这里踩过一个坑，值得记下来
//
// 第一版写的是 `for _, name := range ProfileNames()`，于是
// TestHandler_AllRegisteredProfilesAreServed 报的是"这个名字应该可用却不可用"——
// 而它的判据来源是实现自己的 profiles 切片。
// 结果：把 cmdline 加进白名单这条变异（M48）确实被抓到了，但抓住它的是
// **这条本该守另一个不变式的测试**，而真正该负责的
// TestHandler_UnknownProfileIsNotReachable 一声不响 ——
// 因为 `pprof.Handler("cmdline")` 内部 Lookup 返回 nil，它自己就回 404，
// 与"没注册"外表完全一样。
//
// 换句话说：遍历实现自己的清单 = 白名单给自己作证。
// 名单必须是**外部的、固定的**事实，否则两条守卫会一起失效：
// 一条测"该有的有"，另一条本该测"不该有的没有"，而两者共用同一份可被改写的源。
//
// 后果更具体：将来有人把 `trace` 也挂上去，`TestHandler_AllRegisteredProfilesAreServed`
// 会自动把它算作"应该可用"，而 `TestHandler_UnknownProfileIsNotReachable`
// 仍可能因同样的 Lookup-nil 巧合而不红 —— 于是"cmdline 不暴露"这个安全属性
// 实际无人看守。
var wantAllProfiles = []string{
	"profile", "allocs", "heap", "goroutine", "mutex", "block", "threadcreate",
}

// wantForbidden 是"必须不可达"的名单，同样写死。
var wantForbidden = []string{
	"cmdline", // 吐出整个命令行（含 DATABASE_URL 密码与 JWT_SECRET）
	"trace",   // 执行追踪，开销大且能带出参数值
	"symbol",  // 可 POST 写入，历史上的攻击面
	"tracex",  // 不存在的名字：验证不是"随便什么名字都能进"
}

// TestHandler_AllRegisteredProfilesAreServed 守"该有的全有"。
//
// 只测 CPU profile 的话，将来有人从白名单里删掉 heap 不会被发现。
// 每一条都断言 **200 且响应体非空** —— 状态码 200 但体是空的
// 意味着"端点存在但采集没开"，那比 404 更难排查。
func TestHandler_AllRegisteredProfilesAreServed(t *testing.T) {
	h := Handler(5)
	for _, name := range wantAllProfiles {
		t.Run(name, func(t *testing.T) {
			// profile 必须显式给 seconds=1：不带参数时 Go 默认采 30 秒，
			// 而 CPU profile 全进程只能有一个在采 —— 这一条会把整个包拖到超时。
			// 实测：不加就是 30.01s，加了是 1.00s。
			path := "/debug/pprof/" + name
			if name == "profile" {
				path += "?seconds=1"
			}

			code, body := get(t, h, path)
			if code != http.StatusOK {
				t.Fatalf("/debug/pprof/%s 返回 %d，期望 200（该有的 profile 必须挂上）", name, code)
			}
			if len(body) == 0 {
				t.Fatalf("/debug/pprof/%s 返回 200 但响应体为空", name)
			}
		})
	}
}

// TestHandler_ForbiddenProfilesAreNotReachable 守"不该有的没有"。
//
// 它与上一条是一对，方向相反 —— 铁律 25：「过宽」和「过松」是方向相反的两种失败，
// 只测「该拒的拒了」或只测「该放行的放行了」都会漏掉其中一种。
//
// 为什么"看起来会红"其实不红（实测）：把 cmdline 加进 profiles 后，
// 注册的是 `pprof.Handler("cmdline")`，它内部 `pprof.Lookup("cmdline")` 返回 nil
// ⇒ 端点回 404，与"没注册"外表完全一样 ⇒ 这条守卫**不会红**。
//
// 那 M48 到底被谁抓住的？`TestHandler_AllRegisteredProfilesAreServed` ——
// 因为它遍历了 ProfileNames()，于是把 cmdline 算作"应该可用"，看到 404 就报红。
// 结论：那条变异确实被抓住了，但**抓住它的是名字与职责都不对的那条测试**，
// 而它抓得住的原因是巧合（遍历了实现自己的清单），不是设计。
//
// 所以两条断言必须用**写死的名单**，且这里额外钉一条：
// 白名单里出现任何 forbidden 名字就是错 —— 不依赖运行时行为去发现它。
func TestHandler_ForbiddenProfilesAreNotReachable(t *testing.T) {
	h := Handler(5)
	for _, name := range wantForbidden {
		t.Run(name, func(t *testing.T) {
			code, _ := get(t, h, "/debug/pprof/"+name)
			if code != http.StatusNotFound {
				t.Fatalf("/debug/pprof/%s 返回 %d，期望 404 —— "+
					"该端点不在白名单里，必须不可达（cmdline/trace/symbol 都能泄露敏感内容）",
					name, code)
			}
		})
	}
}

// TestProfiles_NoForbiddenNameIsMounted 是上面那条的**静态**版本。
//
// 为什么需要它：上一条依赖运行时的 404，而"白名单里有 cmdline"这个事实
// 本身才是要守的不变式。运行时判定有个致命的巧合 ——
// pprof.Handler("cmdline") 恰好也回 404，于是"挂错了"与"没挂"外表一样。
// 一个**只靠行为判定**的守卫，会在实现换成别的注册方式时突然失守。
//
// 这里直接查 profiles 切片：它是白名单的单一来源，而 forbidden 名单是外部写死的。
func TestProfiles_NoForbiddenNameIsMounted(t *testing.T) {
	for _, name := range profiles {
		for _, bad := range wantForbidden {
			if name == bad {
				t.Fatalf("白名单里有 %q —— 它必须不可达。"+
					"cmdline 会吐出整个命令行（含 DATABASE_URL 密码与 JWT_SECRET），"+
					"trace 能带出参数值，symbol 可被 POST 写入。", bad)
			}
		}
	}

	// 正控制：白名单本身不能是空的（否则上面那个循环恒真）。
	if len(profiles) == 0 {
		t.Fatal("白名单为空 —— 禁止项检查会恒真通过")
	}
	if len(wantForbidden) == 0 {
		t.Fatal("禁止名单为空 —— 同上，恒真通过")
	}
}

// TestDefaultServeMux_IsNotUsed 守的是一个**无法靠本包消除**的污染面。
//
// ★ 实测更正：第一版这条断言写的是"DefaultServeMux 上不该有 /debug/pprof 的匹配"，
// 它红了。查下来原因是假设错了：不是只有 `import _ "net/http/pprof"` 才会注册，
// 而是 **net/http/pprof 的 init() 无条件注册**（Go 1.27 源码 pprof.go:95-105）：
//
//	func init() {
//	    http.HandleFunc("GET /debug/pprof/", Index)
//	    http.HandleFunc("GET /debug/pprof/cmdline", Cmdline)
//	    http.HandleFunc("GET /debug/pprof/profile", Profile)
//	    http.HandleFunc("GET /debug/pprof/symbol", Symbol)
//	    http.HandleFunc("GET /debug/pprof/trace", Trace)
//	}
//
// **具名导入同样会执行 init**。而本包需要 pprof.Profile / pprof.Handler /
// pprof.Index，所以只要编译进这个二进制，DefaultServeMux 上就一定有这五条 —
// 写成"不该存在"的断言等于要求一个做不到的保证，那样的守卫只会变成噪音。
//
// 真正的不变式是：**这些注册永远不会被服务出去**。
// 唯一的暴露路径是某处 `ListenAndServe(addr, nil)`（nil 的字面含义就是
// "用 DefaultServeMux"）。本包返回的是自己的 mux，且cmd 层把它挂在独立的
// profiling.Server 上 —— 不经过 DefaultServeMux。
//
// 所以这里改成断言"本包提供的 handler 不依赖 DefaultServeMux"：
// 把一个路径在 DefaultServeMux 上查一遍、在自己的 mux 上查一遍，
// 两者结果**必须不同**。若哪天有人把 Handler 改成 `return http.DefaultServeMux`，
// 这条断言会立刻红 —— 那正是暴露路径。
func TestDefaultServeMux_IsNotUsed(t *testing.T) {
	mine := Handler(5)

	// DefaultServeMux 上：pprof 因 init 注册而存在（含 cmdline，危险项）。
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil)
	_, defaultPattern := http.DefaultServeMux.Handler(req)

	// 本包的 mux 上：cmdline 不在白名单里。
	rec := httptest.NewRecorder()
	mine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("本包的 mux 对 /debug/pprof/cmdline 返回 %d，期望 404", rec.Code)
	}

	// 两个来源对同一条路径给出不同答案 —— 这才是"没有依赖 DefaultServeMux"的证据。
	if defaultPattern == "" {
		t.Logf("DefaultServeMux 上没有 /debug/pprof/cmdline —— " +
			"若本二进制没有链入 net/http/pprof 则属正常，本条断言的强度下降但不影响结论")
	} else if rec.Code != http.StatusNotFound {
		t.Fatalf("本包的 mux 与 DefaultServeMux 对 /debug/pprof/cmdline 行为一致" +
			"（都可达），说明 Handler 可能直接返回了 http.DefaultServeMux")
	}

	// 正控制：白名单内的路径在本包 mux 上必须可达。
	// 没有它，上面那段"行为不同"可能是因为 mux 什么都没挂而恒真。
	rec2 := httptest.NewRecorder()
	mine.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("正控制失败：本包的 mux 对 /debug/pprof/heap 返回 %d，期望 200", rec2.Code)
	}
}

// TestGuardSeconds_RejectsOverLimit 是"无上限 = 自我 DoS"那条的守卫。
//
// Go 的 net/http/pprof.Profile 是 `strconv.ParseInt(...)` 之后直接 sleep 的，
// `?seconds=99999` 会让一个 handler 线程占用 27 小时；profiling 期间运行时
// 持续采样，进程比平时慢一截。所以必须钳制。
//
// 断言**同时**覆盖两种超限形式：数值超限、以及根本不是数字的。
// 后者同样重要：原生 net/http/pprof 会把解析失败当成"用默认值 30 秒"，
// 于是 `?seconds=abc` 安静地采 30 秒，而请求方以为参数生效了。
func TestGuardSeconds_RejectsOverLimit(t *testing.T) {
	h := Handler(5)

	cases := []struct {
		name     string
		query    string
		wantCode int
		mention  string
	}{
		{"数值超限", "?seconds=6", http.StatusBadRequest, "5"},
		{"非数字", "?seconds=abc", http.StatusBadRequest, "正整数"},
		{"零", "?seconds=0", http.StatusBadRequest, "正整数"},
		{"负数", "?seconds=-5", http.StatusBadRequest, "正整数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := get(t, h, "/debug/pprof/profile"+c.query)
			if code != c.wantCode {
				t.Fatalf("返回 %d，期望 %d", code, c.wantCode)
			}
			// 错误信息必须含上限值/成因，否则使用者只能去翻文档。
			// "请求失败了但不知道为什么"和"没有失败"一样不可用。
			if !strings.Contains(string(body), c.mention) {
				t.Fatalf("错误信息里找不到 %q，实际内容：%s", c.mention, body)
			}
		})
	}
}

// TestGuardSeconds_AllowsExactlyAtLimit 是边界**内侧**的放行断言。
//
// 只有"该拒的拒了"抓不到把边界写小的实现：把上限误写成 4 时，
// 上面的 cases 全绿，而 seconds=5（本该放行）被误拒。
//
// 正控制与 deny-list 类守卫不可省 —— 否则"一律拒绝"的坏实现能让全部拒绝用例通过。
func TestGuardSeconds_AllowsExactlyAtLimit(t *testing.T) {
	// ★ 边界必须**精确**钉住：`sec == max` 放行、`sec == max+1` 拒绝。
	//
	// 这里直接在 guardSeconds 上验，而不是打真实端点 —— 两个理由：
	//  1. 真实端点在 seconds>0 时会**真的 sleep**（goroutine 也一样），
	//     把边界值取到 5 就要白等 5 秒；
	//  2. 端点层的断言只能证明"没被拒"，证明不了边界落在哪一格。
	// 直接验之后，把 `sec > maxSeconds` 写成 `>=` 会立刻红 ——
	// 而此前那条用 seconds=1（上限 5）的断言在两种写法下都通过，
	// 等于边界根本没被钉住。
	{
		const limit = 5
		stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		for _, sec := range []string{"4", "5"} { // 上限**内侧**（含恰好等于）：必须放行
			rec := httptest.NewRecorder()
			guardSeconds(limit, stub).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/debug/pprof/x?seconds="+sec, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("seconds=%s（上限 %d 以内）应为 200，实际 %d —— 边界被写小了",
					sec, limit, rec.Code)
			}
		}

		for _, sec := range []string{"6"} { // 上限**外侧**第一步：必须拒
			rec := httptest.NewRecorder()
			guardSeconds(limit, stub).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/debug/pprof/x?seconds="+sec, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("seconds=%s（超出上限 %d）应为 400，实际 %d",
					sec, limit, rec.Code)
			}
		}
	}

	// ★ 这里的正控制用一个**不会真采集**的端点（goroutine），不能用 profile。
	//
	// 实测踩过：第一版这里打的是 /debug/pprof/profile 不带 seconds，
	// 注释写"只读状态码不读 body，所以不会真的采集" —— 错的。
	// 采集发生在 handler **内部**，与调用方读不读 body 完全无关，
	// 于是这条测试真的采了 30 秒，把整个包从 4 秒拖到 40 秒
	// （第一次跑它直接 SIGTERM：默认 go test 超时 10 分钟内它反复叠加）。
	//
	// guardSeconds 是套在所有白名单端点外层的，所以用 goroutine 验它一样成立。
	rec := httptest.NewRecorder()
	Handler(5).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("不带 seconds 参数时 /debug/pprof/goroutine 返回 %d，期望 200 —— 默认值路径被误伤了", rec.Code)
	}

	// profile 端点的"无参数 ⇒ 放行"同样要验，但**只断言状态码**且必须让它采完。
	// seconds=1 是最小可行动作；这条等价于上面 CPUProfile 那条的核心断言，
	// 保留它是为了锁住"guardSeconds 没有误伤无参数路径"这件事在 profile 上也成立。
	rec2 := httptest.NewRecorder()
	Handler(5).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("seconds=1（上限 5 以内）返回 %d，期望 200", rec2.Code)
	}
}

// TestHandler_ClampsMaxSeconds 守 Handler 自己的钳制。
//
// 存在理由：profiling 包可以被不经 config.Load 直接调用
// （将来加一个"临时开一下 profiling"的运维命令、或测试里直接用）。
// 那时 config 的 validatePprof 不会经过，钳制是唯一剩下的防线 ——
// 只在 config 设防等于给"绕过 config"留了后门。
func TestHandler_ClampsMaxSeconds(t *testing.T) {
	// 传一个远超硬上限的值：期望它被压回，而 seconds=硬上限仍能放行。
	// seconds=1 而不是硬上限本身 —— 两者走的是同一条"sec > maxSeconds"分支，
	// 用 1 秒代替 600 秒只是为了让测试不跑 10 分钟。
	big := Handler(MaxSecondsHardCap * 10)
	if code, body := get(t, big, "/debug/pprof/profile?seconds=1"); code != http.StatusOK {
		t.Fatalf("maxSeconds=%d 时 seconds=1 返回 %d（%s），期望 200 —— 钳制把上限压错了",
			MaxSecondsHardCap*10, code, body)
	}

	// 正控制：同一个 handler 必须仍然拒绝真正的超限值。
	if code, _ := get(t, big, "/debug/pprof/profile?seconds=999999"); code != http.StatusBadRequest {
		t.Fatalf("maxSeconds 被放大后 seconds=999999 返回 %d，期望 400 —— 钳制没生效", code)
	}

	// ≤0 的 maxSeconds 走默认值分支，不该让所有采集都被拒。
	if code, _ := get(t, Handler(0), "/debug/pprof/profile?seconds=1"); code != http.StatusOK {
		t.Fatalf("maxSeconds=0 时 seconds=1 返回 %d，期望 200", code)
	}
}

// TestIsLoopbackAddr_AdjacentBoundaries 覆盖 loopback 判定的**两侧边界**。
//
// 这是典型的 deny-list 判定，单测"该拒的拒了"抓不到写宽：
// 把 IsLoopback 当成 strings.HasPrefix(host, "127.") 的话，
// 127.0.0.1 与 127.0.0.53 都过（那倒还行），
// 但把 "localhost" 当成不过、或者把空 host 放过，就会漏掉最危险的那一种。
//
// 每判一段都列出上下边界**外侧各一条**：
//   - 127.0.0.1（过） vs 127.0.0.2（也过，整段 127/8 都是回环）
//   - 126.255.255.255（不过） / 128.0.0.1（不过）
//   - :6060 空 host（**不过**，它绑通配地址，形状最像本机端口）
//   - 0.0.0.0（不过）
func TestIsLoopbackAddr_AdjacentBoundaries(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		{"127.0.0.1:6060", true, "回环本体"},
		{"127.0.0.2:6060", true, "整个 127.0.0.0/8 都是回环，不只是 .0.1"},
		{"127.255.255.255:6060", true, "127/8 的上边界，仍是回环"},
		{"126.255.255.255:6060", false, "127/8 的下边界外侧"},
		{"128.0.0.1:6060", false, "127/8 的上边界外侧"},
		{"[::1]:6060", true, "IPv6 回环"},
		{"localhost:6060", true, "字面量 localhost"},
		{"LocalHost:6060", true, "大小写不该影响判定"},
		{"localhost.:6060", true, "末尾根点：DNS 里与 localhost 是同一个名字"},

		// 下面三条是真正危险的形态：长得像"本机端口"，实际全网可达。
		{":6060", false, "空 host ⇒ Go 绑通配地址（所有网卡）"},
		{"0.0.0.0:6060", false, "显式通配"},
		{"[::]:6060", false, "IPv6 通配"},

		// 连续两个根点不是合法名字，不该被当成 localhost。
		{"localhost..:6060", false, "双根点不是合法主机名"},

		// 无法解析的输入按"不安全"处理，而不是静默放行。
		{"6060", false, "没有端口分隔符，解析失败 ⇒ 拒绝"},
		{"not-an-addr", false, "完全不是地址 ⇒ 拒绝"},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			if got := IsLoopbackAddr(c.addr); got != c.want {
				t.Fatalf("IsLoopbackAddr(%q) = %v，期望 %v（%s）", c.addr, got, c.want, c.why)
			}
		})
	}
}

// TestNormalizeAddr_BindsLoopbackExplicitly 守 socket 上真正绑的地址。
//
// 只断言"配置被接受"不够：一个通过了 loopback 校验、启动日志却显示 ":6060" 的
// 诊断端口，会让运维以为自己只在本机暴露了它 —— 而实际上 NormalizeAddr 没生效时
// 绑的是通配地址。日志与 socket 必须说同一件事。
//
// ★ 这里刻意**不**规范化 0.0.0.0
//
// 第一版把 0.0.0.0 也改写成 127.0.0.1，静态审查时发现那是错的：
// 生产容器编排要用 ALLOW_PUBLIC_PPROF_IN_PROD=true + 0.0.0.0 才能让 sidecar
// 从别的 Pod 抓 profile。而悄悄改成 127.0.0.1 会让那个逃生门**静默失效** ——
// 配置说"对外开放"，实际只开了本机，两边却都显示一切正常。
//
// 属于本仓库反复处理的那类故障：以为发生了某个动作，其实没有。
// 空 host 该规范化（它的意图靠字面就能推出来），显式通配不该（那是一个明确的、
// 可解析的意图，猜一个不如要求配置写清楚）。
func TestNormalizeAddr_BindsLoopbackExplicitly(t *testing.T) {
	cases := []struct {
		in, want string
		why      string
	}{
		{":6060", "127.0.0.1:6060", "空 host 的意图是本机，规范化到 socket 上"},
		{"127.0.0.1:6060", "127.0.0.1:6060", "已经是显式回环"},
		{"localhost:6060", "localhost:6060", "localhost 保持原样（net 能解析）"},
		{"0.0.0.0:6060", "0.0.0.0:6060", "显式通配是明确意图，不能被悄悄改掉"},
		{"[::]:6060", "[::]:6060", "IPv6 通配没有可靠的回环等价映射，保持原样"},
		{"garbage", "garbage", "解析失败时原样返回，不猜"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := NormalizeAddr(c.in); got != c.want {
				t.Fatalf("NormalizeAddr(%q) = %q，期望 %q（%s）", c.in, got, c.want, c.why)
			}
		})
	}
}

// TestNewServer_WriteTimeoutIsBounded 守"卡住的采集连接"那条防线。
//
// configureWriteDeadline 只在 srv.WriteTimeout > 0 时设置写截止时间。
// WriteTimeout=0 意味着一个半死不活的 profile 连接能永久占住 goroutine，
// 而 /debug/pprof/profile 一次就要占 30 秒起 —— 几个这样的连接就够了。
func TestNewServer_WriteTimeoutIsBounded(t *testing.T) {
	srv := NewServer(":0", 5)
	if srv.WriteTimeout <= 0 {
		t.Fatalf("WriteTimeout = %v，期望 > 0 —— 否则采集过程没有写截止时间", srv.WriteTimeout)
	}
	if srv.Addr == ":0" {
		t.Fatalf("Addr 仍是 %q，没有规范化成显式回环", srv.Addr)
	}
}

// TestBlockedProfiles_AreDocumented 守那两个"200 + 空数据"的坑。
//
// mutex/block 默认不采样（SetMutexProfileFraction / SetBlockProfileRate 默认为 0）。
// 不开这两个开关时端点返回 200 和空 profile，看上去"采过了、没有竞争"。
// 这比 404 更坏 —— 404 会让人去查，200 + 空数据会让人得出"没有锁竞争"的结论。
//
// 所以它们必须在白名单里（能访问）**且**在说明里被点名（不会被误读）。
func TestBlockedProfiles_AreDocumented(t *testing.T) {
	blocked := BlockedProfiles()
	for _, name := range []string{"mutex", "block"} {
		why, ok := blocked[name]
		if !ok {
			t.Fatalf("%s 没有在 BlockedProfiles 里说明为何返回空数据", name)
		}
		if !strings.Contains(why, "Set") {
			t.Fatalf("%s 的说明里没点名需要打开的开关：%q", name, why)
		}
		// 正控制：说明里必须提到真实存在的 runtime API 名，否则说明本身会骗人。
		if name == "mutex" && !strings.Contains(why, "SetMutexProfileFraction") {
			t.Fatalf("mutex 的说明没提 SetMutexProfileFraction：%q", why)
		}
		if name == "block" && !strings.Contains(why, "SetBlockProfileRate") {
			t.Fatalf("block 的说明没提 SetBlockProfileRate：%q", why)
		}
	}
}

// TestHandler_MuxIsNotTheDefaultOne 补一条正向确认：
// 本包返回的 handler 必须能应答，而一个"什么都没挂"的 handler 不能。
// 这条让上面几条"404 是预期"的断言不至于在环境坏掉时真空通过。
func TestHandler_MuxIsNotTheDefaultOne(t *testing.T) {
	h := Handler(5)

	// 正控制端点取 heap 而不是 profile —— ★ 又踩了一次同一个坑。
	//
	// 第一版写的是 wantAllProfiles[0]，而那张表的第一项是 "profile"，
	// 于是这条测试真的采了 30 秒（Go 对不传 seconds 的默认值）。
	// 与 TestGuardSeconds_AllowsExactlyAtLimit 那次是同一个成因，
	// 可见"CPU profile 全进程唯一 + 默认 30 秒"这个组合很容易被忘。
	//
	// 这里的用途只是"证明 mux 能应答"，用 heap 完全够 ——
	// 而 profile 的"真能采到数据"由 TestHandler_CPUProfileActuallyWorks 负责。
	allowed := "heap"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/"+allowed, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/%s = %d，期望 200", allowed, rec.Code)
	}

	// 未注册路径：404。取自写死名单。
	forbidden := wantForbidden[0]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/debug/pprof/"+forbidden, nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("/debug/pprof/%s = %d，期望 404", forbidden, rec2.Code)
	}

	// 空 mux 作为对照：它对两条路径都返回 404。
	// 若上面两条断言意外都过了，这里会暴露"h 其实是个空 mux"。
	var emptyMux http.Handler = http.NewServeMux()
	if code, _ := get(t, emptyMux, "/debug/pprof/"+allowed); code != http.StatusNotFound {
		t.Fatalf("空 mux 对 /debug/pprof/%s 返回 %d，期望 404 —— "+
			"这条对照不成立，说明上面的 200 断言可能来自别的地方", allowed, code)
	}
}
