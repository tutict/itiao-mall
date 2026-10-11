package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
)

// probeExemptPaths 是并发闸门**必须无条件放行**的路径。
//
// # 为什么探针要豁免
//
// /healthz 与 /readyz 由编排系统（k8s liveness/readiness、compose healthcheck）
// 周期性调用。如果业务过载把探针也挡了，编排系统会判定这个实例不健康并**重启**它 ——
// 一个正在被压垮但本身还能自愈的实例，被重启后冷启动阶段吞吐更低，
// 于是过载加剧、更多实例被判不健康：这是标准的「过载 → 重启 → 更过载」雪崩。
// 而探针请求不做任何业务、不查库，放行它的成本是零。
//
// # 为什么是精确集合，不是前缀匹配
//
// 写成 `strings.HasPrefix(path, "/health")` 这类前缀判断会顺带放行所有
// `/health*` 路径；将来任何以它为前缀的新接口（`/health-admin` 之类）
// 都会在不知不觉中绕过闸门。这份闸门是 fail-closed 的容量保护，
// 它的豁免表必须是**逐条列举**的，改一条要动一次代码、过一次评审。
var probeExemptPaths = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
}

// isProbeExempt 判断这次请求是不是探针，是才豁免闸门。
//
// ★ 为什么不能直接查 r.URL.Path（第一版就是这么写的，是个 fail-open）
//
// chi 路由用的是 `r.URL.RawPath`（非空时优先，见 chi v5 mux.go 的 routeHTTP），
// 而 net/http 对**百分号编码**的路径会同时给出两个字段：
//
//	GET /%68ealthz  →  r.URL.Path = "/healthz"，r.URL.RawPath = "/%68ealthz"
//
// 只看 Path 的话，一个编码变体就能拿到闸门豁免；而 chi 按 RawPath 找不到
// `/healthz` 这条路由，于是它回 404 —— 但那个 404 是在**闸门之内**产生的，
// 因此还会进访问日志。合起来：攻击者用 `/%68ealthz`、`/healthzz%00` 这类变体
// 就能一边绕过容量闸门、一边让请求落进被刻意排在闸门内层的 AccessLog，
// 把"过载时日志是下一个被抽干的资源"这条防线重新打开。
//
// 判据：**RawPath 非空就说明请求用了编码形式**，而探针（k8s/compose）打的是
// 字面路径，永远不会编码。所以一律不豁免 —— fail-closed。
// 守卫：TestConcurrencyGate_DoesNotExemptPercentEncodedVariants（+ 正控制
// TestConcurrencyGate_LiteralProbeStillExempt）；变异体 M64 把这里改回只看 Path。
func isProbeExempt(r *http.Request) bool {
	if r.URL.RawPath != "" {
		return false
	}
	_, ok := probeExemptPaths[r.URL.Path]
	return ok
}

// logThrottleInterval 是过载/恢复日志的最小间隔。
//
// ★ 为什么需要"按时间节流"而不是"按状态翻转记日志"
//
// 第一版用的是 `atomic.Bool` + `CompareAndSwap`：只在"从不满变成满"（或反向）
// 的那一刻记一条。设计时看着无懈可击 —— 日志量正比于状态转移次数，恒定。
//
// **实测直接推翻了它**：60 秒、300 VU 的过载压测产生了 **268,176 条**
// "闸门已满"日志（见 docs/resource-hardening.md 第 7 节的原始数字）。
//
// 机制：槽位是**每个请求**借还一次，所以在饱和点附近，"是否满"这个**瞬时状态**
// 会随着每一次占槽/释放反复翻转。落在饱和点的系统里，状态转移本身就是高频事件。
// 于是 `CompareAndSwap` 几乎每个请求都能成功一次，日志量退回 O(请求数)——
// 而"过载时日志 I/O 是下一个被抽干的资源"正是这个组件要防的东西之一。
//
// 判据修正为：**日志按时间节流**（每 N 秒至多一条），把日志量钉在 O(时长) 上。
// 这比"状态翻转"更正确，因为人关心的是"这段时间在过载"，不是"翻了 26 万次"。
const logThrottleInterval = 10 * time.Second

// logThrottled 回答"这一类日志现在可以记吗"，最多每 every 记一次。
//
// 用 CompareAndSwap 而不是单纯读时间戳：并发下会有大量 goroutine
// 同时通过"距上次够久"的检查，CAS 保证只有一个能把时间戳推进、只有它记日志。
func logThrottled(last *atomic.Int64, every time.Duration) bool {
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(every) {
		return false
	}
	return last.CompareAndSwap(prev, now)
}

// ConcurrencyGate 限制**同时在处理中**的请求数（并发准入闸门 / bulkhead）。
//
// # 它防的是什么
//
// 前面几个中间件各自守一种资源：BodyLimit 守内存、限流守频率。但它们都不限制
// **同时有多少个请求在跑**。高并发下真正的雪崩形态是：
//
//	请求涌入 → 每个请求都去抢那 20 条 PG 连接 → 抢不到的排队等待 →
//	等待的请求占着 goroutine 与内存不放 → 更多请求进来 → 池更紧张
//
// 这就是「超时瀑布」：一旦入口并发超过下游资源池能服务的量，延迟开始线性增长，
// 而**没有任何一个请求是错的** —— 它们只是在互相排队。闸门的作用是把这个正反馈
// 从中间掐断：宁可快速拒绝**一部分**，也不要让**全部**请求一起在 30 秒后超时。
//
// # 为什么是"快速失败"而不是"排队"
//
// 排队（阻塞等待槽位）等于把雪崩从下游搬到闸门里：请求仍占着连接、仍在一个个
// 累积延迟，只是换了个地方。快速失败把"我现在服务不了你"这个事实**立刻**告诉客户端，
// 让它有机会去重试别的实例 / 退避 / 降级 —— 这是背压，不是甩锅。
//
// # 为什么返回 503 而不是 429
//
// 503 = "服务暂时不行了"（服务器侧），429 = "你请求太多了"（客户端侧）。
// 这里的真实原因是**整机容量**到了，与"你"无关；回 429 会让客户端
// 按"等一秒重试"处理一个只有扩容才能解决的问题。
//
// # 位置：在访问日志之前
//
// 被闸门拒绝的请求**不产生访问日志**（闸门在 AccessLog 外层）。这是刻意的：
// 过载时若每个被拒请求都写一行访问日志，日志本身会成为下一个被抽干的资源
// （磁盘写满 → 服务彻底不可用），一次拒绝风暴会把日志系统也打挂。
// 代价是这些拒绝不进访问日志，所以下面用两条日志补可观测性，且**都不按请求记**：
//
//   - 进入/持续过载：按**时间节流**（每 logThrottleInterval 至多一条）。
//   - 恢复正常：用"曾经处于过载"这个条件做 CAS，只记一条。
//
// 合起来日志量正比于**过载时长与过载事件次数**，而不是请求数。
// ★ 这里刻意**不**用"状态翻转记一条"（第一版的设计，已实测推翻 ——
// 饱和点附近槽位每请求借还一次，状态翻转本身就是高频事件，60 秒产生 26.8 万条）。
// 详见 logThrottleInterval 的注释。
func ConcurrencyGate(max int, retryAfter time.Duration) func(http.Handler) http.Handler {
	if max <= 0 {
		// 与 connlimit 同样的理由：0 往往是从配置里读到了空值，
		// "以为设了上限、实际没有"必须响，不能静默退化成不限。
		panic("httpapi: 并发上限必须为正数（0 会静默变成'不限制'）")
	}
	if retryAfter <= 0 {
		retryAfter = time.Second
	}

	// sem 是槽位。带缓冲 chan 让"满了"这个状态在 select 的 default 分支里
	// 立刻可见，无需任何额外判断。
	sem := make(chan struct{}, max)

	// 过载状态与日志节流时间戳。
	//
	// 两者分工不同，缺一不可：
	//   - overloadedSince 回答"现在是不是正处于过载"。恢复日志必须靠它 ——
	//     只按时间节流的话，**每个正常请求**都会在节流窗口外记一条"已恢复"，
	//     哪怕这个实例从来没被压过。
	//   - lastOverloadWarn 负责"过载期间别每请求一条"。理由见 logThrottleInterval。
	var (
		overloadedSince  atomic.Bool
		lastOverloadWarn atomic.Int64
	)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbeExempt(r) {
				next.ServeHTTP(w, r)
				return
			}

			select {
			case sem <- struct{}{}:
				// defer 而不是在末尾释放：handler panic 时也要归还槽位。
				// 漏归还的后果是上限被永久吃掉一格，几十次 panic 之后闸门
				// 就永久卡死了 —— 而 panic 恰好是被 Recoverer 兜住的常规事件。
				defer func() { <-sem }()

				// 恢复日志用"曾经处于过载"作条件（CAS 保证只记一次），
				// 不能只按时间节流 —— 见上面 var 块的注释。
				if overloadedSince.CompareAndSwap(true, false) {
					logging.From(r.Context()).Info("并发闸门恢复正常，重新接受请求",
						slog.Int("max_inflight", max))
				}
				next.ServeHTTP(w, r)

			default:
				overloadedSince.Store(true)
				// 日志**按时间**节流，不是按状态翻转 —— 理由见
				// logThrottleInterval 的注释（按翻转记会退化成 26 万条）。
				if logThrottled(&lastOverloadWarn, logThrottleInterval) {
					logging.From(r.Context()).Warn("并发闸门已满，正在拒绝新请求",
						slog.Int("max_inflight", max),
						slog.String("path", r.URL.Path),
						slog.String("request_id", logging.RequestID(r.Context())),
						slog.String("throttle", "同一状态每 "+logThrottleInterval.String()+" 至多一条"),
					)
				}
				// Retry-After 用秒、向上取整：HTTP 规范只认秒。
				// 向上取整是因为截断成 0 会得到 "Retry-After: 0" ——
				// 那是明确的"立刻重试"邀请，正好加剧当前的过载。
				w.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
				WriteProblem(w, r, domain.ErrServiceOverloaded)
			}
		})
	}
}
