package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/platform/circuitbreaker"
)

// countingScripter 是一个"必然失败且会计数"的假 Redis 后端。
//
// 为什么必须要能计数：熔断的**全部价值**在于"打开之后不再去试"。
// 只断言返回值仍然是错误是不够的 —— 一个"每次都去试、试完照样返回错误"的
// 实现会让所有返回值断言都通过，而它一点资源都没省下来。
// 只有数调用次数才能证明这一点。
type countingScripter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (c *countingScripter) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingScripter) failCmd(ctx context.Context) *redis.Cmd {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(c.err)
	return cmd
}

func (c *countingScripter) Eval(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return c.failCmd(ctx)
}

func (c *countingScripter) EvalRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return c.failCmd(ctx)
}

func (c *countingScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return c.failCmd(ctx)
}

func (c *countingScripter) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return c.failCmd(ctx)
}

func (c *countingScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	cmd.SetErr(c.err)
	return cmd
}

func (c *countingScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	cmd.SetErr(c.err)
	return cmd
}

// TestAllow_BreakerStopsHittingBackend 是熔断最核心的一条守卫。
//
// 它证明的是"打开之后不再碰远端"，而不只是"仍然返回错误"。
func TestAllow_BreakerStopsHittingBackend(t *testing.T) {
	be := &countingScripter{err: errors.New("redis down")}
	// 冷却给 1 分钟：保证测试期间不会自动进入半开，现象干净。
	l := New(be, "rl:breaker", time.Second, true, WithBreaker(3, time.Minute))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := l.Allow(ctx, testPolicy("b"), "u", testDefaults()); err == nil {
			t.Fatalf("第 %d 次：后端必然失败，应返回错误", i+1)
		}
	}

	tripped := be.CallCount()
	if tripped == 0 {
		t.Fatal("熔断打开前必须真的调用过后端")
	}

	// 熔断已打开：后续调用必须**一个包都不发**。
	for i := 0; i < 10; i++ {
		if _, err := l.Allow(ctx, testPolicy("b"), "u", testDefaults()); err == nil {
			t.Fatal("熔断打开后仍应返回错误（不是放行）")
		}
	}
	if got := be.CallCount(); got != tripped {
		t.Fatalf("熔断打开后又调用了 %d 次后端 —— 熔断没有真正生效（只改了返回值）",
			got-tripped)
	}
}

// TestAllow_PrecheckDoesNotFeedBreaker 守住"前置校验不吃熔断额度"。
//
// 窗口/阈值缺失是**配置错误**，不是后端故障。若它被计进熔断，
// 一个漏配的调用点就能把熔断器喂到打开 —— 真正的 Redis 故障还没发生，
// 限流已经全量降级了，而且没有任何一处会报警。
func TestAllow_PrecheckDoesNotFeedBreaker(t *testing.T) {
	be := &countingScripter{err: errors.New("redis down")}
	l := New(be, "rl:pre", time.Second, true, WithBreaker(2, time.Minute))

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		empty := Policy{Scope: ScopeAuth, Action: "bad"}
		if _, err := l.Allow(ctx, empty, "u", Defaults{}); err == nil {
			t.Fatal("窗口与阈值都缺省时应报错")
		}
	}
	if got := be.CallCount(); got != 0 {
		t.Fatalf("前置校验不该碰后端，实际调用了 %d 次", got)
	}

	// 现在来真实的失败：阈值是 2，所以恰好两次后打开。
	for i := 0; i < 2; i++ {
		if _, err := l.Allow(ctx, testPolicy("pre"), "u", testDefaults()); err == nil {
			t.Fatal("后端必然失败")
		}
	}
	if got := be.CallCount(); got != 2 {
		t.Fatalf("两次真实失败应各打一次后端（共 2），实际 %d —— "+
			"若为 0 说明熔断被前置校验提前喂开了", got)
	}
}

// TestDecide_BreakerOpenStillRespectsFailMode 守住"熔断不改变降级方向"。
//
// 熔断只决定"要不要去试"，不决定"失败之后怎么办"。后者仍归 failsClosed。
// 若熔断顺手把 fail-closed 变成放行，那就是一次静默的安全降级。
func TestDecide_BreakerOpenStillRespectsFailMode(t *testing.T) {
	ctx := context.Background()

	t.Run("fail-closed 仍拒绝且标记 degraded", func(t *testing.T) {
		be := &countingScripter{err: errors.New("down")}
		l := New(be, "rl:bo", time.Second, true, WithBreaker(1, time.Minute))

		// 第一次失败 → 熔断打开。
		_, _, _ = l.Decide(ctx, testPolicy("x"), "u", testDefaults())
		// 第二次：走的是熔断路径，而不是真实后端。
		res, degraded, _ := l.Decide(ctx, testPolicy("x"), "u", testDefaults())
		if res.Allowed {
			t.Fatal("fail-closed 下熔断打开也必须拒绝")
		}
		if !degraded {
			t.Fatal("必须标记 degraded —— HTTP 层据此回 503 而不是 429")
		}
	})

	t.Run("fail-open 仍放行", func(t *testing.T) {
		be := &countingScripter{err: errors.New("down")}
		l := New(be, "rl:bo2", time.Second, false, WithBreaker(1, time.Minute))

		_, _, _ = l.Decide(ctx, testPolicy("x"), "u", testDefaults())
		res, degraded, err := l.Decide(ctx, testPolicy("x"), "u", testDefaults())
		if err != nil {
			t.Fatalf("fail-open 不该返回错误，实际 %v", err)
		}
		if !res.Allowed {
			t.Fatal("fail-open 下熔断打开仍应放行")
		}
		if !degraded {
			t.Fatal("必须标记 degraded，否则监控上看不出限流已经失效")
		}
	})
}

// TestAllow_InternalJudgeTimeoutFeedsBreaker 守住"内部判定超时 = 后端故障"。
//
// ★ 这条是 2026-10-11 四轴 review 挖出来的真缺陷的守卫，形态值得记下来：
// 原实现用 `errors.Is(err, context.DeadlineExceeded)` 判"调用方取消"，看着更简洁，
// 但**内部** judgeCtx 超时时 go-redis 在池等待路径上原样返回 ctx.Err()
// （v9.22.0 internal/pool/pool.go 的 waitTurn/queuedNewConn），
// 而 BackendError.Unwrap() 会把 Cause 透出来 ⇒ 内部超时被读成"调用方取消"
// ⇒ 走 Abort() 不计失败 ⇒ **熔断器永远打不开**。
//
// 这一条必须是带**对照**的：只测"内部超时能喂开熔断"是不够的 —— 把 noConclusion
// 恒置 true 或恒置 false 都能让单侧用例通过。所以下面同时测两个方向：
//   - 内部超时（Cause = DeadlineExceeded，调用方 ctx 正常）→ 必须喂开；
//   - 调用方取消（已有 TestAllow_CallerCancelDoesNotFeedBreaker）→ 必须不喂开。
//
// 探测用假后端返回 context.DeadlineExceeded，而不是真起一个黑洞 Redis：
// 前者就是 go-redis 池等待路径**暴露到 Allow 里的那个值**，确定性、无 flake；
// 后者要制造"池被占满 + judgeCtx 先到期"的竞态，在 CI 上不可靠。
func TestAllow_InternalJudgeTimeoutFeedsBreaker(t *testing.T) {
	// err 是 go-redis 池等待路径在 judgeCtx 到期时原样上抛的值。
	be := &countingScripter{err: context.DeadlineExceeded}
	l := New(be, "rl:jtimeout", time.Second, true, WithBreaker(3, time.Minute))

	// ★ 调用方 ctx **没有**超时 —— 这正是"内部超时"与"调用方取消"的分界线。
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := l.Allow(ctx, testPolicy("jt"), "u", testDefaults()); err == nil {
			t.Fatalf("第 %d 次：后端必然失败，应返回错误", i+1)
		}
	}

	if got := l.breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("内部判定超时没有喂熔断（状态=%v，期望 open）—— "+
			"熔断器对这一类故障是聋子：它在「后端卡住 + 池被占满」时打不开，"+
			"而那正是它唯一要解决的问题。判据必须用调用方 ctx，不能用错误链。", got)
	}

	// 熔断打开后必须**一个包都不发**（与 TestAllow_BreakerStopsHittingBackend 同一判据：
	// 只断言返回值错误的话，一个"照旧每次都去试"的实现也能通过）。
	tripped := be.CallCount()
	for i := 0; i < 5; i++ {
		_, _ = l.Allow(ctx, testPolicy("jt"), "u", testDefaults())
	}
	if got := be.CallCount(); got != tripped {
		t.Fatalf("熔断打开后又调用了 %d 次后端 —— 熔断没有真正生效", got-tripped)
	}
}

// TestAllow_CallerCancelDoesNotFeedBreaker 守住"调用方取消 ≠ 后端故障"。
//
// 场景：客户端断开或上游超时，ctx 被取消。此时判定拿不到结果 —— 但如果把它
// 记成一次"后端失败"，那么一次客户端集体重连（或上游网关抖动）就足以把熔断器
// 喂开，于是**好端端的 Redis 被自己的客户端挤下线**。
// 判据：没有结论的调用不能作为证据；同时它必须撤销半开探测位（否则卡死）。
func TestAllow_CallerCancelDoesNotFeedBreaker(t *testing.T) {
	be := &countingScripter{err: errors.New("down")}
	l := New(be, "rl:cancel", time.Second, true, WithBreaker(2, time.Minute))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 5; i++ {
		_, err := l.Allow(canceled, testPolicy("c"), "u", testDefaults())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("第 %d 次应上抛 context.Canceled，实际 %v", i+1, err)
		}
	}

	// 5 次取消（远超阈值 2）之后，熔断器必须仍然闭合 ——
	// 所以下一次真实调用应当**仍然打到后端**（CallCount 会增加）。
	before := be.CallCount()
	_, _ = l.Allow(context.Background(), testPolicy("c"), "u", testDefaults())
	if got := be.CallCount(); got <= before {
		t.Fatalf("调用方取消被当成了后端失败证据 —— 熔断器被误喂开"+
			"（调用数从 %d 变成 %d，说明后续请求没打到后端）", before, got)
	}
}

// TestWithoutBreaker_RestoresPerRequestHitting 钉住那个"测试开关"的语义。
// WithoutBreaker 是给那些**专门要观察'没有熔断时会发生什么'**的用例用的
// （例如黑洞后端下的超时断言）。它必须真的恢复"每请求都去试"的行为，
// 否则那些用例测的就不是它们声称的东西了。
func TestWithoutBreaker_RestoresPerRequestHitting(t *testing.T) {
	be := &countingScripter{err: errors.New("down")}
	l := New(be, "rl:nb", time.Second, true, WithoutBreaker())

	ctx := context.Background()
	for i := 0; i < 8; i++ {
		_, _ = l.Allow(ctx, testPolicy("n"), "u", testDefaults())
	}
	if got := be.CallCount(); got != 8 {
		t.Fatalf("关闭熔断后每次调用都该打后端（期望 8 次），实际 %d", got)
	}
}
