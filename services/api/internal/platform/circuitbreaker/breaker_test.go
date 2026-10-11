package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

// newTestBreaker 返回一个时钟可注入的熔断器，以及推进时钟的函数。
//
// 注入时钟而不是 time.Sleep：真实冷却时间是秒级，靠 sleep 测会让
// 测试又慢又脆（CI 上抖一下就成了假红）。这里时间由测试完全掌控。
func newTestBreaker(t *testing.T, threshold int, open time.Duration) (*Breaker, func(time.Duration)) {
	t.Helper()
	now := time.Now()
	b := New(threshold, open)
	b.now = func() time.Time { return now }
	return b, func(d time.Duration) { now = now.Add(d) }
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker(t, 3, time.Second)

	// 前 2 次失败：还不到阈值，保持闭合。
	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("第 %d 次调用不该被熔断: %v", i+1, err)
		}
		b.Report(false)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("未达阈值时状态应为 closed，实际 %s", got)
	}

	// 第 3 次失败：打开。
	if err := b.Allow(); err != nil {
		t.Fatalf("达到阈值的那次调用本身应被放行: %v", err)
	}
	b.Report(false)
	if got := b.State(); got != StateOpen {
		t.Fatalf("连续 %d 次失败后状态应为 open，实际 %s", 3, got)
	}

	// 打开之后：**立刻**拒绝，且不应该有任何远端调用的机会。
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("打开后 Allow 应返回 ErrOpen（不去试远端），实际 %v", err)
	}
}

// TestBreaker_SuccessResetsFailureCount 钉住"连续失败"而不是"累计失败"。
//
// 若实现改成累计计数，一个 50% 失败率的抖动依赖会被熔断 ——
// 而它其实还能用，熔断它等于自伤。
func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker(t, 3, time.Second)

	for _, ok := range []bool{false, false, true, false, false} {
		if err := b.Allow(); err != nil {
			t.Fatalf("交错成功不该熔断，却在某次被拒: %v", err)
		}
		b.Report(ok)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("失败从未连续达到 3 次，状态应保持 closed，实际 %s", got)
	}
}

func TestBreaker_HalfOpenAllowsSingleProbeAndClosesOnSuccess(t *testing.T) {
	b, advance := newTestBreaker(t, 1, 5*time.Second)

	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Report(false) // → open
	if got := b.State(); got != StateOpen {
		t.Fatalf("应打开，实际 %s", got)
	}

	// 冷却未到：仍拒。
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("冷却未到时应拒，实际 %v", err)
	}

	advance(5 * time.Second)

	// 冷却结束：放行**一个**探测。
	if err := b.Allow(); err != nil {
		t.Fatalf("冷却结束应放行探测，实际 %v", err)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("应进入 half-open，实际 %s", got)
	}

	// 第二个并发调用不该也被当成探测 —— 否则半开就成了全量放行。
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("半开期只应放行一个探测，第二个应被拒，实际 %v", err)
	}

	// 探测成功 → 闭合。
	b.Report(true)
	if got := b.State(); got != StateClosed {
		t.Fatalf("探测成功后应闭合，实际 %s", got)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("闭合后应恢复放行，实际 %v", err)
	}
}

func TestBreaker_HalfOpenFailureReopensAndRestartsCooldown(t *testing.T) {
	b, advance := newTestBreaker(t, 1, 5*time.Second)

	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Report(false) // → open

	advance(5 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("应放行探测: %v", err)
	}
	b.Report(false) // 探测失败

	if got := b.State(); got != StateOpen {
		t.Fatalf("探测失败应重新打开，实际 %s", got)
	}
	// 且冷却**重新**计时：刚打开就调用应仍被拒。
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("探测失败后应重新冷却，实际 %v", err)
	}
}

// TestBreaker_AbortUnsticksHalfOpen 守住"无结论的探测不能卡死熔断器"。
//
// 场景：半开期放出的那个探测，在拿到结果前调用方就取消了 ctx（客户端断开）。
// 若不给熔断器一个"无结论"的信号，halfOpenInFlight 会永远留在 true，
// 于是再也放不出任何探测 —— 依赖早就恢复了，服务却一直 503 到重启。
func TestBreaker_AbortUnsticksHalfOpen(t *testing.T) {
	b, advance := newTestBreaker(t, 1, time.Second)

	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Report(false) // → open

	advance(time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("应放行探测: %v", err)
	}
	b.Abort() // 探测无结论

	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("Abort 不该改变状态（没有证据就不下结论），实际 %s", got)
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("Abort 之后必须能再放一个探测，否则熔断器永久卡死: %v", err)
	}
}

func TestNew_PanicsOnInvalidArgs(t *testing.T) {
	cases := []struct {
		threshold int
		open      time.Duration
	}{
		{0, time.Second},
		{-1, time.Second},
		{1, 0},
		{1, -time.Second},
	}
	for _, tc := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%d, %v) 应 panic，实际静默接受", tc.threshold, tc.open)
				}
			}()
			New(tc.threshold, tc.open)
		}()
	}
}
