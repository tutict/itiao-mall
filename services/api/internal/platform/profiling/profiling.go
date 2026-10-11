// Package profiling 提供进程级性能剖析端点。
//
// ★ 为什么它是一个**独立监听端口**，而不是挂进业务 router
//
// pprof 的三个特性决定了它不能和业务路由共用一张表：
//
//  1. **它需要长时间占用连接**。CPU profile 默认采 30 秒，
//     业务 router 的 chimw.Timeout(30s) 与之同量级，随时会在采集途中把连接掐掉，
//     拿到一份半截的 profile —— 而半截 profile 不报错，看上去完全正常。
//  2. **它不该占用限流额度**。诊断流量挤占的是正常用户的准入配额，
//     方向正好相反：出问题时最需要能随时抓一把 profile。
//  3. **它的读权限不能和业务权限同一套**。业务路由的默认拒绝层
//     （httpapi.authGate）与"本机调试"是两件事：前者管的是"谁能调这个接口"，
//     后者管的是"谁能读到进程内存"。
//
// 挂进业务 router 还有第二个后果：契约门禁
// TestRoutePolicy_CoversEveryContractEndpoint 会因为契约里没有 /debug/pprof 而报错，
// 或者更糟 —— 有人为了让它通过而在 openapi.yaml 里登记它，
// 于是 pprof 变成一个**契约化的公开接口**。
//
// ★ 为什么默认关闭
//
// pprof 不是普通的只读端点。heap profile 会吐出**分配时的参数内容**，
// goroutine?debug=2 会吐出每个 goroutine 的完整栈，
// cmdline 会吐出整个命令行 —— 里面可能有 DATABASE_URL（含密码）与 JWT_SECRET。
// 也就是说，拿到一个可访问的 pprof 端点 ≈ 拿到一份进程的内存快照。
//
// 所以这里的默认值是"关"，开启必须显式配 PPROF_ADDR，
// 且生产环境绑非 loopback 地址要再过一道显式逃生门（见 config.validatePprof）。
package profiling

import (
	"fmt"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"strconv"
	"strings"
	"time"
)

// WriteTimeout 是诊断端口的写超时。
//
// 必须 > 0：net/http/pprof 的 configureWriteDeadline 只在
// srv.WriteTimeout > 0 时才给采集过程设置写截止时间，
// 于是 WriteTimeout=0 意味着一个卡住的 profile 连接可以永久挂着。
//
// 取 15 秒（而不是 0 或很大）：net/http/pprof 自己会把采集秒数加在这个值上，
// 15s + PPROF_MAX_SECONDS(60s) = 75 秒，对一次 profile 采集足够宽裕。
const WriteTimeout = 15 * time.Second

// DefaultMaxSeconds 是单次采集的默认上限（秒）。
//
// 存在的理由是 Go 自己不设上限：net/http/pprof.Profile 是
// `strconv.ParseInt(...)` 之后直接拿来 sleep 的，
// `?seconds=99999` 会让一个 handler 线程占用 27 小时。
// 而 CPU profiling 期间运行时会持续采样，开着 profiling 的进程比平时慢一截 ——
// 一条无上限的 profile 端点就是一条自我施加的 DoS。
const DefaultMaxSeconds = 60

// MaxSecondsHardCap 是 PPROF_MAX_SECONDS 的合法上界。
//
// 上界不是性能保护，是**配置事故的护栏**：把采样时长配成 86400（"录一整天"）
// 会让 profiling 几乎永久生效，而它的表现（服务变慢）不像错误、像负载问题。
// 有人真的需要长时间采样时，正确做法是抓多个窗口而不是拉长单个窗口。
//
// 刻意**导出**：config 侧的生产校验要拒绝超界的 PPROF_MAX_SECONDS，
// 而两处各写一个字面量 600 等于把"它们必须相等"寄存在注释里 ——
// 改一处不改另一处不会让任何测试变红，只会让校验与钳制悄悄错开。
// 单源之后，config 直接引用这个常量（见 config.pprofMaxSecondsCap）。
const MaxSecondsHardCap = 600

// profiles 是**逐条列举**的 runtime profile 白名单。
//
// 不写 "/debug/pprof/" 前缀通配，理由与 httpapi 的路由策略表同源：
// 开放前缀意味着将来 runtime/pprof 新增一个 profile 就会被顺带暴露，
// 而没有任何东西会报错。runtime/pprof 还能被第三方库注册自定义 profile，
// 那条路径上"新 profile 自动可访问"是实打实的风险。
//
// 每一项都要能说清"排障时为什么要它"：
//   - allocs     累计分配，回答"谁在制造垃圾"，定位 GC 压力来源。
//   - heap       存活对象，回答"内存被谁占着"。
//   - goroutine  协程栈与阻塞点。并发系统的第一现场；配合 debug=2 能看到每个栈帧的参数。
//   - mutex      锁竞争。吞吐被某个单点串行化时的直接证据。
//   - block      阻塞在 channel/锁上的时间。区分"在等 IO"与"在抢锁"。
//   - threadcreate 线程创建历史。判断是否在用 cgo 或有意外的线程泄漏。
var profiles = []string{
	"allocs",
	"heap",
	"goroutine",
	"mutex",
	"block",
	"threadcreate",
}

// blocked 是 profiles 里需要先打开采样率才有内容的项。
//
// runtime 默认不采 mutex/block（SetMutexProfileFraction / SetBlockProfileRate
// 默认分别为 0 与 0）。不设这两个开关时，这两个端点会**返回 200 和一份空 profile**，
// 看上去"采过了、没有竞争"，而实际上是没开采集。
//
// 这比 404 更坏：404 会让人去查，200 + 空数据会让人得出"没有锁竞争"的结论。
// 所以这里把它们列出来，写进文档，并让 Index 的说明能指出来。
var blocked = map[string]string{
	"mutex": "需要 runtime.SetMutexProfileFraction(N>0) 才会采到数据；默认返回空 profile",
	"block": "需要 runtime.SetBlockProfileRate(N>0) 才会采到数据；默认返回空 profile",
}

// Handler 返回挂载了剖析端点的独立 mux。
//
// maxSeconds 是单次采集的秒数上限；≤ 0 时用 DefaultMaxSeconds。
//
// ★ 它**不使用** http.DefaultServeMux，但 DefaultServeMux 上确实有 pprof
//
// 这里有一处**实测更正**（第一版的注释写错了）：不是只有
// `import _ "net/http/pprof"` 才会把端点挂到 DefaultServeMux ——
// Go 1.27 的 net/http/pprof 有这么一段无条件 init：
//
//	func init() {
//	    http.HandleFunc("GET /debug/pprof/", Index)
//	    http.HandleFunc("GET /debug/pprof/cmdline", Cmdline)
//	    http.HandleFunc("GET /debug/pprof/profile", Profile)
//	    http.HandleFunc("GET /debug/pprof/symbol", Symbol)
//	    http.HandleFunc("GET /debug/pprof/trace", Trace)
//	}
//
// 具名导入同样执行 init。而本包需要 pprof.Profile / pprof.Handler / pprof.Index，
// 所以只要编译进这个二进制，DefaultServeMux 上就一定有这五条 ——
// 这是**躲不掉**的，写成"不该存在"的断言只会得到一个永久红的守卫。
//
// 于是真正的防线只能是"这些注册永远不会被服务出去"：
// 唯一的暴露路径是某处 `ListenAndServe(addr, nil)`（nil 的字面含义就是
// "用 DefaultServeMux"），而那行代码看起来与安全毫无关系。
//
// 本包显式建自己的 mux、只挂白名单路径（cmdline/trace/symbol 一律不挂，
// 它们能泄露 DATABASE_URL 密码与 JWT_SECRET）。断言见
// profiling_test.go 的 TestDefaultServeMux_IsNotUsed。
func Handler(maxSeconds int) http.Handler {
	if maxSeconds <= 0 {
		maxSeconds = DefaultMaxSeconds
	}
	if maxSeconds > MaxSecondsHardCap {
		maxSeconds = MaxSecondsHardCap
	}

	mux := http.NewServeMux()

	// CPU profile。刻意**不用** pprof.Handler("profile")：
	// 后者内部是 pprof.Lookup("profile")，而 "profile" 不是 runtime/pprof 里的伪 profile
	// （实测返回 404 "Unknown profile"）。CPU profile 的实现在 net/http/pprof.Profile 里。
	//
	// 这是这个包最容易写错的一处：直觉写法是"遍历白名单，统一用 pprof.Handler 注册"，
	// 那样 heap/goroutine 全部正常，唯独最需要的 CPU profile 是 404 ——
	// 而 404 在一次仓促的排障里很容易被读成"这个端点没开"。
	mux.Handle("/debug/pprof/profile", guardSeconds(maxSeconds, http.HandlerFunc(httppprof.Profile)))

	// 索引页。net/http/pprof.Index 自己会遍历它那份 profile 列表，
	// 其中包含 trace/cmdline/symbol —— 这三项在下面刻意没有注册，点了会得到 404。
	// 这比给一个 404 的端点回 200 空响应好：404 是明确的"没开"，不是"没有数据"。
	mux.HandleFunc("/debug/pprof/", httppprof.Index)

	for _, name := range profiles {
		mux.Handle("/debug/pprof/"+name, guardSeconds(maxSeconds, httppprof.Handler(name)))
	}

	return mux
}

// guardSeconds 给端点加上采集时长上限。
//
// ★ 超限时**拒绝**而不是静默钳到上限
//
// 静默钳制的后果是：运维请求 seconds=600，被采成 60 秒，
// 拿到的报告写着"60 秒 CPU 采样"，而他以为自己看的是 10 分钟窗口。
// 一个被悄悄改小的采样窗口产出的结论与请求的不一致 ——
// 这与"读不到的值填 0"是同一类错误：把"不是我以为的那个东西"呈现成正常结果。
//
// 所以这里回 400，并在响应体里写明上限值，让人能立刻改对。
func guardSeconds(maxSeconds int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("seconds")
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}

		// 只认正整数，与 Go 自身的解析口径一致。
		// 解析失败在这里回 400；而原生 net/http/pprof 会把解析失败当成"用默认值"，
		// 于是 ?seconds=abc 会安静地采 30 秒 —— 与上面的静默钳制同一类问题。
		sec, err := strconv.Atoi(raw)
		if err != nil || sec <= 0 {
			http.Error(w,
				fmt.Sprintf("seconds 必须是正整数（收到 %q）", raw),
				http.StatusBadRequest)
			return
		}
		if sec > maxSeconds {
			http.Error(w,
				fmt.Sprintf("seconds=%d 超过上限 %d。"+
					"长时间采样请分多次抓，不要拉长单个窗口 —— "+
					"profiling 期间进程会持续变慢，一次无上限的采集等于自我施加的 DoS。",
					sec, maxSeconds),
				http.StatusBadRequest)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// IsLoopbackAddr 判断监听地址是否只对本机可见。
//
// 判定刻意是**白名单**：能证明是回环的才放行，其余（含解析失败）一律拒绝。
// 依据来自 Go 的 net.Listen 语义 ——
//   - host 为空（":6060"）⇒ 绑通配地址（所有网卡）。这正是最危险的那一种，
//     而它长得最像"本地端口"。
//   - host 是 IP ⇒ 就按 IP 判。
//   - host 是域名 ⇒ 要做 DNS 才能确定，而解析意味着一次可能超时、
//     可能被 hosts 文件或 DNS 劫持改写的网络访问。
//
// 所以对域名只认**字面量** localhost，不解析：这里要回答的是
// "这份配置的本意是不是本机"，而本意不是本机的人写 localhost 只可能是配置写错了。
// （实测：net.ParseIP 对 "localhost" 返回 nil，所以那个 EqualFold 分支不是死代码，
// 但它在 ParseIP 之前 —— 顺序反了的话 localhost 会被判成非回环。）
//
// ★ 末尾的根点必须去掉（实测 localhost. 与 localhost 在 DNS 上是同一个名字）
//
// 不去根点会让 localhost. 被判成非回环 —— 方向上偏保守（拒绝而非放行），
// 看起来"安全"，但它是一处**不一致**：同一台机器、同一个意思，两种写法两种结果。
// 那种不一致会在有人写 localhost. 时表现为"启动失败，报错说必须绑回环"，
// 而他明明写的就是 localhost。
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 解析不出来不等于安全。按最坏情况处理。
		return false
	}
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	// 去末尾根点：DNS 里 "localhost." 与 "localhost" 是同一个名字。
	// 刻意只去**一个**：连续两个根点（"localhost.."）不是合法名字，
	// 不该被当成 localhost 放行。
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// normalizeAddr 把"意图是本机"的写法规范化成显式回环绑定。
//
// ★ 它只处理空 host，**不碰** 0.0.0.0
//
// 理由：0.0.0.0 是一个**明确的、可解析的**意图 —— 绑所有网卡。
// 把它悄悄改成 127.0.0.1 会让生产的 ALLOW_PUBLIC_PPROF_IN_PROD 逃生门
// 静默失效：配置说"对外开放"，实际只开了本机，而两边都显示一切正常。
// 那正是本仓库反复处理的那类故障：**以为发生了某个动作，其实没有**。
//
// （实测踩过：这条是被 TestNormalizeAddr_BindsLoopbackExplicitly 抓出来的，
// 而它之所以能抓出来，是因为守卫把 0.0.0.0 的期望值写成了"保持原样"。）
//
// 空 host 不同：它在 Go 里没有"绑所有网卡"之外的第二种解释，
// 而写 ":6060" 的人几乎总是想表达"本机端口"（意图靠字面就能推出来，
// 不需要额外假设）。把它规范化能让意图落实到 socket 上。
//
// IPv6 通配（[::]:6060）同样**不**处理：net.Listen 对 "[::]" 的语义是
// 双栈通配，而这里没有可靠的"把它等价成 127.0.0.1"的映射 ——
// 猜一个不如要求配置写清楚。
func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if strings.TrimSpace(host) == "" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// NormalizeAddr 暴露给 cmd 层，用于日志里显示真正会绑的地址。
//
// 单测不能只断言"配置被接受了"，还要断言"socket 真的绑在哪" ——
// 一个通过了 loopback 校验却在日志里显示 :6060 的诊断端口，
// 会让运维以为自己只在本机暴露了它。
func NormalizeAddr(addr string) string { return normalizeAddr(addr) }

// NewServer 构造诊断用的 http.Server。
//
// ReadTimeout 给 5 秒：诊断端点的请求都是无 body 的 GET，
// 不存在需要长时间读 body 的场景，5 秒足够防住慢速连接把 goroutine 挂住。
func NewServer(addr string, maxSeconds int) *http.Server {
	return &http.Server{
		Addr:              normalizeAddr(addr),
		Handler:           Handler(maxSeconds),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10, // 诊断请求连 header 都很小，8 KiB 绰绰有余
	}
}

// ProfileNames 返回已挂载的 profile 名，供启动日志与文档使用。
func ProfileNames() []string {
	out := make([]string, 0, len(profiles)+1)
	out = append(out, "profile")
	out = append(out, profiles...)
	return out
}

// BlockedProfiles 返回需要显式打开采样率才有内容的 profile 及其原因。
func BlockedProfiles() map[string]string {
	out := make(map[string]string, len(blocked))
	for k, v := range blocked {
		out[k] = v
	}
	return out
}
