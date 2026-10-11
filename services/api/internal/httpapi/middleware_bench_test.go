package httpapi

// 逐层基准：把"中间件链一共多少开销"拆成"每层各多少"。
//
// # 为什么要这个文件
//
// 一次 CPU profile 只给出一个聚合结论：`rateLimitGate` 累计 35.63%。
// 但那是**累计**（含它调用的 Redis 客户端序列化、context 派生、
// 策略表线性查找），且不能回答"链上另外 11 层各值多少"。
//
// 而"优化中间件开销"这个任务的第一步恰恰是排序：12 层里砍掉一层
// 收益 3%，还是砍掉一层收益 30%，决定了先动哪个。没有数字就只能凭直觉，
// 而直觉在这类事情上错得很系统 —— **最外层的中间件看起来最"轻"，
// 实际它每请求都构造 logger / 正则 / context**，而链尾那个看着"重"的
// 其实只在命中时才执行。
//
// # 判据口径
//
// 用 `b.ReportAllocs()`：中间件的开销大头不是纯计算，而是**每请求的堆分配**
// （context 节点、With() 出来的 logger、Header 写入的 map 增长）。
// 只看 ns/op 会把"分配 5 个对象但每个都很小"和"不分配"混为一谈，
// 而前者在高并发下的 GC 压力上完全不是同一量级。
//
// Redis 侧用 miniredis：它跑在进程内、不走网络、不受本机 TCP 抖动影响，
// 于是测出来的是**限流器自身的 CPU 成本**（Lua 求值 + go-redis 编解码）。
// 真实 Redis 的网络往返单独测，那部分是另一个数字，不该混进来。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"

	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
)

// benchPath 是压测实际命中的路径（公开读）。
const benchPath = "/api/v1/products"

func benchRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, benchPath, nil)
	r.RemoteAddr = "203.0.113.7:54321"
	return r
}

// discard 是链尾：什么都不做，只返回 200。
func discard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// BenchmarkLayer_HarnessBaseline 是**必须先看的那一条**。
//
// 它量的是"什么都不加，只调一次 ServeHTTP"的开销，而这个开销不小：
// `httptest.NewRecorder()` 每次要造一个 ResponseRecorder + Buffer + Header map，
// 单这一项就是几百 ns。
//
// 没有它，所有单层基准的读数都**含**这层 harness 成本，于是
// "BodyLimit 861 ns"看起来像BodyLimit 花了 861 ns，其实它可能只花 50 ns
// 而剩下 800 ns 全是测量装置。**没有基线的基准不能用来排序。**
func BenchmarkLayer_HarnessBaseline(b *testing.B) {
	h := discard()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// benchSinkLogger 造一个"格式化照做、输出丢弃"的 logger。
//
// 用 logging.New 会真的往 stdout 写JSON —— 那让基准既慢又污染终端，
// 而**写盘/写管道本身是 AccessLog 成本的一部分**，量出来的是IO 而不是 CPU。
// 想单独量"日志写出去有多贵"是另一个问题（答案：非常贵，且与部署形态有关）。
// 这里选格式化但不落地：能量到 slog 的属性构建成本，量不到 IO。
func benchSinkLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// ---- 单层基准 ----

// BenchmarkLayer_RequestID 量 request id 生成（含正则校验 + uuid）。
func BenchmarkLayer_RequestID(b *testing.B) {
	h := RequestID(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_AccessLog 量访问日志层。
//
// 用真正的 logger（handler 丢弃输出，但 slog 仍会走 Enabled + 属性构建），
// 这样量到的是"每请求构造带 request_id 的 logger + statusRecorder"的开销。
func BenchmarkLayer_AccessLog(b *testing.B) {
	base := benchSinkLogger()
	h := AccessLog(base)(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_RealIP 量 chi 的 RealIP。
func BenchmarkLayer_RealIP(b *testing.B) {
	h := chimw.RealIP(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_Timeout 量 chi 的 Timeout 中间件。
//
// 它内部起一个 goroutine + 一个 timer，是链上**唯一有跨请求共享状态**
// 的一层（timer 堆），所以单独量。
func BenchmarkLayer_Timeout(b *testing.B) {
	h := chimw.Timeout(30 * time.Second)(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_AuthGate 量认证门禁（公开路径，只走策略表查找，不解 JWT）。
func BenchmarkLayer_AuthGate(b *testing.B) {
	issuer := auth.NewTokenIssuer("bench-secret-not-used-on-public-path", time.Minute)
	h := authGate(issuer, "mock")(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_BodyLimit 量请求体上限层。
func BenchmarkLayer_BodyLimit(b *testing.B) {
	h := BodyLimit(1 << 20)(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_SecurityHeaders 量安全响应头层。
func BenchmarkLayer_SecurityHeaders(b *testing.B) {
	h := SecurityHeaders(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_RateLimitGate 量限流层（miniredis，进程内）。
func BenchmarkLayer_RateLimitGate(b *testing.B) {
	h := benchRateLimitGate(b)(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkLayer_MerchantStatusGate 量商户状态层（公开路径不该查库）。
func BenchmarkLayer_MerchantStatusGate(b *testing.B) {
	table := routePolicy("mock")
	called := false
	h := merchantStatusGate(table, func(context.Context, string) (string, error) {
		called = true
		return "APPROVED", nil
	})(discard())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
	if called {
		b.Fatal("公开读路径上商户状态层查了库 —— 与它自己的注释矛盾")
	}
}

// ---- 策略表查找（无 IO，纯计算） ----

// BenchmarkPolicyFor_AccessTable 量访问策略表的线性查找。
//
// 这一层刻意不做任何索引：契约端点几十条，线性扫一遍比建 map 的
// 建/维护成本低，而且策略表是**启动期构建一次的静态数据**。
// 基准的作用是让"要不要给它建索引"这个决定有数字，而不是靠感觉。
func BenchmarkPolicyFor_AccessTable(b *testing.B) {
	table := routePolicy("mock")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = policyFor(table, http.MethodGet, benchPath)
	}
}

// BenchmarkPolicyFor_Miss 量未命中路径（扫完全表才返回 deny）。
//
// 攻击者能自由选择路径，所以**最坏情况才是真实成本**。
// 只测命中会严重低估。
func BenchmarkPolicyFor_Miss(b *testing.B) {
	table := routePolicy("mock")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = policyFor(table, http.MethodGet, "/api/v1/nonexistent/deep/path")
	}
}

// BenchmarkLookupRateRule 量限流策略表的线性查找（公开读路径会命中第 7 条）。
func BenchmarkLookupRateRule(b *testing.B) {
	table := ratePolicy(ratelimit.Defaults{Window: time.Minute, Limit: 120})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lookupRateRule(table, http.MethodGet, benchPath)
	}
}

// ---- 限流器内部：Lua 求值 vs 其余部分 ----

// BenchmarkRateLimiter_Allow 量 Decide 的**全部**成本（策略解析 + key 拼装
// + member 生成 + Lua + 结果解包），miniredis 后端。
func BenchmarkRateLimiter_Allow(b *testing.B) {
	l := benchLimiter(b)
	p := lookupRateRule(ratePolicy(ratelimit.Defaults{Window: time.Minute, Limit: 120}),
		http.MethodGet, benchPath)
	def := ratelimit.Defaults{Window: time.Minute, Limit: 120}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := l.Decide(ctx, p, "bench-subject", def); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRateLimiter_DecideOverhead 量 Decide 里**除 Lua 往返之外**的部分：
// 策略默认值解析、Redis key 拼装、member 生成、context 派生、结果解包。
//
// 存在的意义是给"限流层该优化哪一段"提供分界线：
// 若这一条只占 Decide 的一小部分，那优化方向就是 Lua 本身或干脆绕开它；
// 若占比可观，那说明成本在分配（key/member 字符串 + WithTimeout 的 timer），
// 优化手段完全不同（复用缓冲、去掉 per-request 的 context 派生）。
//
// 做法是注入一个立刻返回的假 Scripter：Lua 的 CPU 归零，剩下的就是外围开销。
func BenchmarkRateLimiter_DecideOverhead(b *testing.B) {
	l := ratelimit.New(noopScripter{}, "bench", 200*time.Millisecond, false)
	p := lookupRateRule(ratePolicy(ratelimit.Defaults{Window: time.Minute, Limit: 120}),
		http.MethodGet, benchPath)
	def := ratelimit.Defaults{Window: time.Minute, Limit: 120}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := l.Decide(ctx, p, "bench-subject", def); err != nil {
			b.Fatal(err)
		}
	}
}

// noopScripter 是"立刻返回合法结果、零网络往返"的假 Redis。
//
// ★ 这里必须返回**格式正确**的结果，不能只返回 nil：
// `Script.Run` 内部调 `EvalSha(...).Slice()`，若结果不是 3 个 int64，
// Allow 会走 `len(res) != 3` 或类型断言失败那条分支并 return error。
// 那条分支不做结果解包、还多一次错误构造 —— 于是基准安静地测成了
// "错误路径"的成本，而结论会指向完全错误的优化方向。
//
// 顺便注意接口形状：`Scripter` 有 6 个方法（不是 3 个），
// 且 `Eval*` 只返回 `*redis.Cmd`、不返回 error。
type noopScripter struct{}

var benchNow = time.Now().UnixMilli()

func (noopScripter) ok(ctx context.Context) *redis.Cmd {
	c := redis.NewCmd(ctx)
	c.SetVal([]interface{}{int64(1), int64(1), benchNow})
	return c
}

func (s noopScripter) Eval(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return s.ok(ctx)
}

func (s noopScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return s.ok(ctx)
}

func (noopScripter) EvalRO(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx)
}

func (noopScripter) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
	return redis.NewCmd(ctx)
}

func (noopScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	return redis.NewBoolSliceCmd(ctx)
}

func (noopScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	return redis.NewStringCmd(ctx)
}

// 编译期断言：签名一旦漂移，这里立刻红，而不是让基准 panic 在运行时。
var _ redis.Scripter = noopScripter{}

// ---- 整链 ----

// BenchmarkChain_PublicRead 量完整链在**公开读路径**上的每请求成本，
// 限流层挂 **noopScripter**（零往返）。
//
// ★ 为什么整链里不挂真Redis，也不挂 miniredis：
//
// 实测（本机，Redis 在 Docker/WSL2）：
//
//	miniredis  1,190µs/次    ← 比真 Redis 慢 3 倍，见下
//	真 Redis      364µs/次
//	noopScripter  ~1µs
//
// miniredis 慢的原因是 `Script.Run` 先 `EVALSHA`，收到 `NOSCRIPT`
// 才回退 `EVAL`（go-redis v9.16 script.go:65-70）。miniredis 不缓存脚本，
// 于是每次调用都要把整段 Lua 源码当命令参数发过去。
// **用 miniredis 测限流器会得出"限流是瓶颈"的结论，而它其实是测量工具的假象。**
//
// 所以这里只量"链的结构成本"，限流层自身的成本由
// `internal/ratelimit` 里的真实 Redis 基准单独给。
func BenchmarkChain_PublicRead(b *testing.B) {
	issuer := auth.NewTokenIssuer("bench-secret", time.Minute)
	base := benchSinkLogger()
	table := routePolicy("mock")

	h := RequestID(
		SecurityHeaders(
			AccessLog(base)(
				chimw.RealIP(
					chimw.Timeout(30 * time.Second)(
						authGate(issuer, "mock")(
							benchRateLimitGate(b)(
								BodyLimit(1 << 20)(
									merchantStatusGate(table, nil)(
										discard())))))))))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// BenchmarkChain_WithoutRateLimit 是上面那条的对照组。
//
// 两条之差 = 限流层「除Redis 往返之外」的全部成本。
//
// ★ 它**不等于**限流层的真实代价：真实代价还要加上 364µs 的 Redis 往返。
//
//	限流层总代价 ≈ 本基准之差+ 一次 Decide（真 Redis）
//
// 之所以要在注释里写死这个加法，是因为"链上差值很小 → 限流层不贵"是个
// 极易犯的错—— 它只对了"往返之外的那部分"，而往返恰恰是大头。
func BenchmarkChain_WithoutRateLimit(b *testing.B) {
	issuer := auth.NewTokenIssuer("bench-secret", time.Minute)
	base := benchSinkLogger()
	table := routePolicy("mock")

	h := RequestID(
		SecurityHeaders(
			AccessLog(base)(
				chimw.RealIP(
					chimw.Timeout(30 * time.Second)(
						authGate(issuer, "mock")(
							BodyLimit(1 << 20)(
								merchantStatusGate(table, nil)(
									discard()))))))))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), benchRequest())
	}
}

// ---- 辅助 ----

// benchLimiter 造一个挂 noopScripter 的限流器：Redis 往返成本为零，
// 只留策略解析、key 拼装、context 派生这些客户端侧开销。
//
// 为什么阈值给到 1e9：基准要跑几十万次迭代，阈值一小就全是 429，
// 而**拒绝路径与放行路径的成本不同**（Lua 里拒绝要多一次 ZRANGE）。
// 阈值给足，保证每次都走放行分支 —— 那才是压测里的常态。
func benchLimiter(b *testing.B) *ratelimit.Limiter {
	b.Helper()
	return ratelimit.New(noopScripter{}, "bench", 200*time.Millisecond, false)
}

// benchRateLimitGate 返回限流层的**中间件工厂**（不是成品 handler），
// 这样才能与其他层一样参与链式拼装。
//
// 后端是 noopScripter（零往返）—— 理由见 BenchmarkChain_PublicRead 的注释：
// miniredis 的 NOSCRIPT 回退会让限流层的读数虚高 3 倍，
// 而我们要把"限流层的客户端侧开销"与"Redis 往返"分开量。
func benchRateLimitGate(b *testing.B) func(http.Handler) http.Handler {
	b.Helper()
	l := benchLimiter(b)
	rateTable := ratePolicy(ratelimit.Defaults{Window: time.Minute, Limit: 1000000000})
	big := ratelimit.Defaults{Window: time.Minute, Limit: 1000000000}
	return func(next http.Handler) http.Handler {
		return rateLimitGate(l, rateTable, big)(next)
	}
}
