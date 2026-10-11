package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis 给用例一个干净可用的 Redis。
//
// ★ 这里曾经埋着一个很贵的坑：原来连不上 Redis 就 t.Skipf。
// 结果是 13 个限流测试里有 10 个在**静默跳过**，`go test` 退出码仍然是 0，
// 看起来"全绿"—— 而滑动窗口、Lua 原子脚本、并发计数这些核心逻辑
// **一次都没被执行过**。更糟的是它们还让变异验证给出了假结论：
// 施加 M16/M19 后"测试仍然全绿"，被当成"守卫是假的"，实际是守卫根本没运行。
//
// 所以规则改成：默认走 miniredis（进程内，永远能跑，保证逻辑真的被执行）；
// 设置 RATELIMIT_TEST_REAL_REDIS=1 时才连真机，用来验证 miniredis 与真 Redis
// 在 Lua 上的行为差异。
//
// 连接失败一律 t.Fatal 而非 t.Skip —— 一个跑不起来的守卫必须让门禁红，
// 不能让门禁"绿着但什么也没检查"。
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	if os.Getenv("RATELIMIT_TEST_REAL_REDIS") == "1" {
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			addr = "localhost:6379"
		}
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = rdb.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			t.Fatalf("指定了 RATELIMIT_TEST_REAL_REDIS=1 但连不上 Redis(%s): %v", addr, err)
		}
		return rdb
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newTestLimiter 建一个带唯一前缀的限流器，避免用例之间互相污染计数。
//
// 唯一前缀是必须的：限流键是 ZSET，测试之间共享同一个 key 的话，
// 上一个用例留下的记录会让下一个用例的第一次判定就被拒 ——
// 表现为"单跑绿、全跑红"，是最难查的一类测试故障。
//
// 注意不再手动 SCAN+DEL 清理：miniredis 是每个用例独占的进程内实例，
// 用例结束即销毁；真机模式下残留的 key 有 TTL 且带唯一前缀，不清理也不会互相影响。
func newTestLimiter(t *testing.T, rdb redis.UniversalClient, failsClosed bool) *Limiter {
	t.Helper()
	return New(rdb, "rltest:"+fmt.Sprint(time.Now().UnixNano()), 2*time.Second, failsClosed)
}

func testDefaults() Defaults {
	return Defaults{Window: time.Minute, Limit: 5}
}

func testPolicy(action Action) Policy {
	return Policy{Scope: ScopeAuth, Action: action, Window: time.Minute, Limit: 5}
}

// 阈值必须在窗口内被真正拦住，且放行次数恰好等于阈值。
//
// 这是限流器最基本的一条：不变量写死成"允许 N 次、第 N+1 次拒绝"。
// 只断言"最终有拒绝"是不够的 —— 一个把阈值当窗口用的实现也能通过那种断言。
func TestAllow_BlocksAtThreshold(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()
	p := testPolicy("block")

	for i := 1; i <= 5; i++ {
		res, err := l.Allow(ctx, p, "u1", testDefaults())
		if err != nil {
			t.Fatalf("第 %d 次判定出错: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("第 %d 次应放行（阈值 5），却被拒。Count=%d", i, res.Count)
		}
		if res.Count != int64(i) {
			t.Fatalf("第 %d 次的计数应为 %d，实际 %d", i, i, res.Count)
		}
	}

	res, err := l.Allow(ctx, p, "u1", testDefaults())
	if err != nil {
		t.Fatalf("第 6 次判定出错: %v", err)
	}
	if res.Allowed {
		t.Fatal("第 6 次应被拒绝（已超阈值 5）")
	}
	if res.RetryAfter <= 0 {
		t.Fatalf("被拒时必须给出 RetryAfter，实际 %v", res.RetryAfter)
	}
	// RetryAfter 不能超过窗口长度：它是"最早可重试时刻"，
	// 超过窗口就意味着让用户白等。
	if res.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter %v 超过窗口 1m，说明算错了", res.RetryAfter)
	}
}

// 不同主体互不影响。这是限流按主体分桶的全部意义：
// 一个用户被限不能连累另一个用户。
func TestAllow_IsolatesSubjects(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()
	p := testPolicy("iso")

	for i := 0; i < 5; i++ {
		if _, err := l.Allow(ctx, p, "noisy", testDefaults()); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := l.Allow(ctx, p, "noisy", testDefaults()); res.Allowed {
		t.Fatal("noisy 应已被限流")
	}

	res, err := l.Allow(ctx, p, "quiet", testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("quiet 不应被 noisy 的限流影响")
	}
}

// 不同 action 互不影响。
//
// 这条守的是一个具体的回归风险：三个登录入口（商户/管理员/微信）
// 刻意分成三个 action 就是为了不互相吃额度，测试要盯住这一点。
func TestAllow_IsolatesActions(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := l.Allow(ctx, testPolicy("login.merchant"), "same-user", testDefaults()); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := l.Allow(ctx, testPolicy("login.merchant"), "same-user", testDefaults()); res.Allowed {
		t.Fatal("login.merchant 应已被限流")
	}
	res, err := l.Allow(ctx, testPolicy("login.admin"), "same-user", testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("login.admin 不应被 login.merchant 的限流影响")
	}
}

// ★ 滑动窗口的核心断言：窗口滑过之后额度归还。
//
// 固定窗口在这个测试里会怎么失败：它按"当前落在哪个时间桶"计数，
// 把时间往前推一个窗口长度后落到新桶，额度同样归还 —— 所以**这个断言
// 单独存在时区分不了两者**。真正能区分的是下一个测试。
func TestAllow_WindowSlidesAndRestoresQuota(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()
	p := testPolicy("slide")

	for i := 0; i < 5; i++ {
		if _, err := l.Allow(ctx, p, "u", testDefaults()); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := l.Allow(ctx, p, "u", testDefaults()); res.Allowed {
		t.Fatal("应已被限流")
	}

	// 把时间往前推一个完整窗口：全部记录都滑出范围，额度应完全归还。
	l.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	res, err := l.Allow(ctx, p, "u", testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("窗口滑过后额度应归还，却仍被限流")
	}
}

// ★ 固定窗口与滑动窗口的唯一可观测差异：窗口末尾的突发。
//
// 场景：60 秒窗口、阈值 5。
//   - 固定窗口（桶按自然时间对齐）：第 59.9s 的 5 个填满桶1，
//     第 60.1s 落到桶2，桶2 是空的 → 又放行 5 个。0.2 秒内共 10 个。
//   - 滑动窗口：第 60.1s 时那 5 个的年龄只有 0.2s，仍在"过去 60 秒"内 → 继续拒。
//
// 注意时间轴的摆法：前 5 个必须落在窗口**末尾**（base+59.8s 起）。
// 若像直觉那样从 base 开始放，60.1s 时它们已经 60 秒出头、全部过期，
// 两种实现都会放行 —— 这个断言就失去区分能力了（而且很容易写成这样）。
func TestAllow_RejectsBoundaryBurst(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()
	p := testPolicy("boundary")

	base := time.Now()
	// 5 次请求挤在窗口末尾 59.8s ~ 60.0s 之间。
	for i := 0; i < 5; i++ {
		l.now = func() time.Time { return base.Add(59800*time.Millisecond + time.Duration(i)*50*time.Millisecond) }
		res, err := l.Allow(ctx, p, "u", testDefaults())
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("第 %d 次应放行（阈值 5）", i)
		}
	}

	// 刚越过 60 秒边界 100ms。固定窗口此刻开新桶并放行，滑动窗口不会。
	l.now = func() time.Time { return base.Add(60*time.Second + 100*time.Millisecond) }
	res, err := l.Allow(ctx, p, "u", testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("窗口边界处放行了 —— 滑动窗口被改成了固定窗口？")
	}
}

// 拒绝的请求**不占额度**。
//
// 攻击者用远超阈值的请求量把窗口塞满，会让正常用户也被挤掉
// （而且他自己的重试在帮倒忙）。所以超限请求不该被记录。
func TestAllow_RejectedRequestsDoNotConsumeQuota(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	ctx := context.Background()
	p := testPolicy("noquota")

	for i := 0; i < 5; i++ {
		if _, err := l.Allow(ctx, p, "u", testDefaults()); err != nil {
			t.Fatal(err)
		}
	}
	// 疯狂重试 20 次，全部被拒。
	for i := 0; i < 20; i++ {
		res, err := l.Allow(ctx, p, "u", testDefaults())
		if err != nil {
			t.Fatal(err)
		}
		if res.Allowed {
			t.Fatalf("第 %d 次重试不该被放行", i)
		}
	}

	// 时间推过窗口，额度必须恰好恢复到 5（而不是 5 减去重试消耗掉的量）。
	l.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	allowed := 0
	for i := 0; i < 10; i++ {
		res, err := l.Allow(ctx, p, "u", testDefaults())
		if err != nil {
			t.Fatal(err)
		}
		if res.Allowed {
			allowed++
			l.now = func() time.Time { return time.Now().Add(61*time.Second + time.Duration(allowed)*20*time.Millisecond) }
		}
	}
	if allowed != 5 {
		t.Fatalf("额度应恢复为 5，实际放行 %d 次 —— 被拒的请求占用了额度", allowed)
	}
}

// ★ 并发下放行数必须**恰好等于**阈值。
//
// 这条是 Lua 存在的全部理由。如果判定拆成"ZREMRANGEBYSCORE / ZCARD / ZADD"
// 三次往返，并发的 N 个请求会读到同一个基数，于是放行数超过阈值。
// 这条测试在拆分实现下必然失败（实测放行数会显著大于阈值）。
func TestAllow_ConcurrentExactlyHitsLimit(t *testing.T) {
	rdb := newTestRedis(t)
	l := newTestLimiter(t, rdb, true)
	ctx := context.Background()
	p := testPolicy("concurrent")

	const threshold = 20
	p.Limit = threshold
	p.Window = time.Minute

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 让所有 goroutine 同时起跑，最大化竞争
			res, err := l.Allow(ctx, p, "race", Defaults{Window: time.Minute, Limit: threshold})
			if err != nil {
				t.Errorf("并发判定出错: %v", err)
				return
			}
			if res.Allowed {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != threshold {
		t.Fatalf("并发下放行 %d 次，期望恰好 %d 次。超出即说明判定不是原子的。", got, threshold)
	}
}

// 后端不可用时的两种降级方向。
//
// 这条测试的价值在于它同时钉住了两个方向 —— 只测 fail-open 的话，
// 有人把 failsClosed 判断写反（生产环境静默变成放行）测试依然绿。
func TestDecide_BackendDown(t *testing.T) {
	// 指向一个必然连不上的地址。
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = dead.Close() })

	t.Run("fail-open 放行", func(t *testing.T) {
		l := New(dead, "rl:open", 500*time.Millisecond, false)
		res, degraded, err := l.Decide(context.Background(), testPolicy("x"), "u", testDefaults())
		if err != nil {
			t.Fatalf("fail-open 时不应返回错误，实际 %v", err)
		}
		if !res.Allowed {
			t.Fatal("fail-open 必须放行")
		}
		if !degraded {
			t.Fatal("必须标记为 degraded —— 否则监控上看不出限流失效了")
		}
	})

	t.Run("fail-closed 拒绝", func(t *testing.T) {
		l := New(dead, "rl:closed", 500*time.Millisecond, true)
		res, degraded, _ := l.Decide(context.Background(), testPolicy("x"), "u", testDefaults())
		if res.Allowed {
			t.Fatal("fail-closed 必须拒绝")
		}
		if !degraded {
			t.Fatal("必须标记为 degraded —— HTTP 层据此回 503 而不是 429")
		}
	})
}

// 阈值缺失必须报错，不能"当作不限流"。
//
// 不限流是**安全侧的失效**：它不会自己暴露，只会在某天被扫描器发现。
//
// 传一个既无窗口也无阈值的 Policy，且 Defaults 也是空的 —— 只传空 Defaults
// 是不够的，因为 testPolicy 自带窗口与阈值，会先被补上而测不到这条分支。
func TestAllow_RejectsNonPositiveConfig(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	empty := Policy{Scope: ScopeAuth, Action: "bad"}
	if _, err := l.Allow(context.Background(), empty, "u", Defaults{}); err == nil {
		t.Fatal("窗口与阈值都缺省时应报错，而不是当作不限流")
	} else if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("应返回 ErrBackendUnavailable，实际 %v", err)
	}
}

// 空主体必须被拒。空 key 会让所有匿名请求共用一个计数器 ——
// 那既不是限流，是随机封禁。
func TestAllow_RejectsEmptySubject(t *testing.T) {
	l := newTestLimiter(t, newTestRedis(t), true)
	if _, err := l.Allow(context.Background(), testPolicy("empty"), "", testDefaults()); err == nil {
		t.Fatal("空主体应被拒绝")
	}
}

// Redis 的错误原文不得外泄。
//
// 原文形如 "dial tcp 10.0.3.11:6379: connect: connection refused" ——
// 里面是内网地址与拓扑。外泄等于给攻击者画地图。
//
// ★ 用**假后端**注入一条明确带内网地址的错误，而不是真去连一个死地址：
// 真实 dial 失败的文本取决于是 refused 还是 timeout（后者常常不含地址），
// 于是这样的断言时绿时红，而且本机形态变了它就不再证明任何事。
// 旧版本还有一条 `t.Skip("Redis 未报错…")` —— 那让整条守卫变成环境相关，
// 现在改成确定性断言：假的必然失败，不存在"没报错就跑过"的分支。
func TestAllow_DoesNotLeakBackendAddress(t *testing.T) {
	const leaky = "dial tcp 10.0.3.11:6379: connect: connection refused"

	l := New(failingScripter{err: errors.New(leaky)}, "rl:leak", 500*time.Millisecond, true)

	_, err := l.Allow(context.Background(), testPolicy("leak"), "u", testDefaults())
	if err == nil {
		t.Fatal("后端必然失败，Allow 必须返回错误")
	}

	// 1) 归因必须仍然成立 —— 否则 Decide 的 fail-closed 分支会静默失效。
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("应能 errors.Is(ErrBackendUnavailable)，实际 %v", err)
	}
	// 2) 对外文本里不能有内网地址、端口，也不能有底层系统错误原文。
	got := err.Error()
	for _, bad := range []string{"10.0.3.11", "6379", "connection refused", leaky} {
		if strings.Contains(got, bad) {
			t.Fatalf("错误文本泄露了后端细节 %q: %s", bad, got)
		}
	}
	// 3) 但原文必须仍能被取出来写日志 —— 否则排查成本会转嫁给运维。
	if cause := Cause(err); cause == nil || !strings.Contains(cause.Error(), "10.0.3.11") {
		t.Fatalf("Cause 应保留底层原文供日志使用，实际: %v", cause)
	}
}

// failingScripter 是一个"必然失败"的假 Redis 后端，错误文本可控。
//
// 实现 redis.Scripter 的 6 个方法即可 —— 这正是把 Limiter 的依赖面
// 收窄到 Scripter（而不是 UniversalClient）换来的可测试性。
type failingScripter struct{ err error }

func (f failingScripter) failCmd(ctx context.Context) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(f.err)
	return cmd
}

func (f failingScripter) Eval(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return f.failCmd(ctx)
}

func (f failingScripter) EvalRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return f.failCmd(ctx)
}

func (f failingScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return f.failCmd(ctx)
}

func (f failingScripter) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return f.failCmd(ctx)
}

func (f failingScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	cmd.SetErr(f.err)
	return cmd
}

func (f failingScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	cmd.SetErr(f.err)
	return cmd
}

// ★ 判定必须有**自己的**超时上限，与调用方的 ctx 解耦。
//
// 为什么要这条：限流是准入控制，它的延迟由所有正常用户承担。
// 若它跟着调用方的 30 秒 HTTP 超时走，Redis 在主从切换时"连得上但不回话"
// （Dial 成功、命令挂住），每个请求都会挂满 30 秒 ——
// 表现为"服务整体变慢"而不是"限流器坏了"，后者是更容易被发现的故障。
//
// ★ 这条测试在写第一版时**抓到了一个真实缺陷**：
// 它最初只断言"ctx deadline 生效"，实测发现 go-redis v9 在这种故障下
// 完全不理会 ctx 的 deadline —— 200ms 的 ctx 实际耗了 5.0006826s，
// 等的是它自己的 ReadTimeout（默认 5s）。ctx 在这里几乎是装饰。
// 所以现在断言的是"总耗时被钉住"，而钉住它的机制是 NewDedicatedRedis
// 里的 ReadTimeout，不是 ctx。两者都要，缺一不可。
func TestAllow_HasItsOwnTimeout(t *testing.T) {
	addr := startBlackHoleRedis(t)
	// 走 NewDedicatedRedis 才是生产路径 —— 它带 ReadTimeout。
	// poolSize 传 0：本测试只关心超时，池大小与本条无关（0 会用默认）。
	rdb := NewDedicatedRedis(addr, "", 0, 200*time.Millisecond, 0)
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, "rl:blackhole", 200*time.Millisecond, true)

	done := make(chan error, 1)
	go func() {
		_, err := l.Allow(context.Background(), testPolicy("blackhole"), "u", testDefaults())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("黑洞后端不应返回成功")
		}
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("应返回 ErrBackendUnavailable，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("判定没有自己的超时上限 —— 挂在了调用方的 HTTP 超时上")
	}
}

// ★ 同一场景下，若限流器用的是**业务 Redis 客户端**会挂多久。
//
// 这条测试的价值是把上面那条注释里的实测数字钉住，防止将来有人
// "图省事"把 main.go 里的专用客户端换回共用客户端。
// 它断言的是一个**已知的不理想**：ctx 拦不住，所以耗时由客户端的
// ReadTimeout（这里给 1.5 秒）决定，而不是由 ctx 的 200ms 决定。
func TestAllow_CtxAloneDoesNotBoundBlackHole(t *testing.T) {
	addr := startBlackHoleRedis(t)
	// 故意用默认 ReadTimeout（5s）的客户端，模拟"图省事共用"的那个改法。
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, "rl:shared", 200*time.Millisecond, true)

	start := time.Now()
	_, err := l.Allow(context.Background(), testPolicy("shared"), "u", testDefaults())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("黑洞后端不应返回成功")
	}
	// ctx 给了 200ms，但实际耗时会接近 ReadTimeout(5s) 的一半以上 ——
	// 断言"明显超过 ctx 的 deadline"就够了，不必钉死具体秒数
	// （钉死会让这条测试在慢机器上变脆）。
	if elapsed < 500*time.Millisecond {
		t.Fatalf("共用客户端时耗时只有 %v —— 说明本机的 go-redis 行为与注释里的实测不符，"+
			"请重新核对 ratelimit.New 的注释（ctx 拦不住阻塞读）", elapsed)
	}
	t.Logf("共用客户端 + 200ms ctx 的实际耗时：%v（ctx 未生效，等的是 ReadTimeout）", elapsed)
}

// startBlackHoleRedis 起一个「接受 TCP 连接但永不回应」的本地服务器。
//
// 它模拟的是 Redis 主从切换那种"连得上但不回话"的故障 ——
// 那种故障下命令会一直挂着，直到某个超时把它打断。
func startBlackHoleRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 收下连接但一个字节都不回，也不关 —— 让客户端的命令挂住。
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
