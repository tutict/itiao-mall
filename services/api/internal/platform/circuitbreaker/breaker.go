// Package circuitbreaker 实现一个最简三态熔断器（closed / open / half-open）。
//
// # 它解决的具体问题
//
// 一个**每次请求都要访问**的远端依赖（本项目里是限流用的 Redis）在故障时，
// 会把它的故障时间**乘以**请求量转嫁成自己的故障：
//
//	Redis 卡住 → 每个请求都要等一次判定超时（300ms）→ 这些请求占着 goroutine、
//	连接与内存不放 → 吞吐暴跌 → 网关超时 → 用户看到整站挂了。
//
// 值得注意的是：`ErrBackendUnavailable` 的降级逻辑（见 ratelimit.Decide）
// **并没有**解决这个问题 —— 它只决定了"失败之后怎么答"，没决定"还要不要每次都去试"。
// 于是 Redis 挂掉的整段时间里，服务仍然为每一个请求付一次完整的超时。
// 熔断补的就是这一段：连续失败若干次之后，**不再去试**，直接给结论。
//
// # 三个状态
//
//   - closed（正常）：放行所有调用。连续失败达到阈值 → open。
//   - open（熔断）：不放行任何调用，立刻返回 ErrOpen。冷却时间到 → half-open。
//   - half-open（试探）：放行**一个**探测调用。成功 → closed；失败 → open 并重新计时。
//
// # 为什么 half-open 只放一个探测
//
// 如果冷却一到就放行所有调用，那等于在依赖尚未恢复时再给它一次全量冲击 ——
// 故障常常就是这么被"试探性流量"二次打垮的。放一个探测，等它的结论，
// 是让恢复过程本身也是渐进的。
//
// # 为什么 open 时不做重试/排队
//
// 熔断器的价值在于"立刻给结论"。若在 open 时还去重试或等待，
// 它就退化成了另一种形式的下游排队 —— 而排队正是它要消除的东西。
package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen 表示熔断器处于打开状态，调用被直接拒绝（未触及远端）。
var ErrOpen = errors.New("熔断器已打开")

// State 是熔断器的状态。
type State int

const (
	// StateClosed 正常放行。
	StateClosed State = iota
	// StateOpen 熔断中，拒绝一切调用。
	StateOpen
	// StateHalfOpen 半开，放行单个探测。
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker 是熔断器。零值不可用，必须用 New 构造。
//
// 并发安全：所有状态转换都在一把互斥锁下完成。
type Breaker struct {
	failThreshold int
	openDuration  time.Duration
	now           func() time.Time

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time

	// halfOpenInFlight 标记"已经放出去的那个探测还没回来"。
	// 没有它，半开期间每个到达的请求都会认为自己是那个探测，于是又变成全量放行。
	halfOpenInFlight bool
}

// New 构造熔断器。
//
// failThreshold 是**连续**失败多少次后打开。用连续而不是累计：
// 一个 50% 失败率的抖动依赖不应该被熔断（它还能用），
// 而"连续 N 次都失败"才说明它现在是真的不可用。
func New(failThreshold int, openDuration time.Duration) *Breaker {
	if failThreshold < 1 {
		panic("circuitbreaker: failThreshold 必须 ≥ 1（0 会让熔断器形同虚设）")
	}
	if openDuration <= 0 {
		panic("circuitbreaker: openDuration 必须为正数")
	}
	return &Breaker{
		failThreshold: failThreshold,
		openDuration:  openDuration,
		now:           time.Now,
		state:         StateClosed,
	}
}

// Allow 回答"这次调用可以放行吗"。返回 ErrOpen 表示熔断器打开、**不该**触及远端。
//
// 调用方约定：Allow 返回 nil 之后，**必须**恰好调用一次 Report 或 Abort，
// 否则半开状态的 in-flight 标记会把熔断器永久卡住。用 defer 兜住这条约定。
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateOpen:
		if b.now().Sub(b.openedAt) >= b.openDuration {
			// 冷却结束 → 半开，放行**一个**探测。
			b.state = StateHalfOpen
			b.halfOpenInFlight = true
			return nil
		}
		return ErrOpen

	case StateHalfOpen:
		if b.halfOpenInFlight {
			return ErrOpen
		}
		b.halfOpenInFlight = true
		return nil

	default: // StateClosed
		return nil
	}
}

// Report 上报一次调用的结果。
func (b *Breaker) Report(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		b.halfOpenInFlight = false
		if success {
			// 探测成功 = 依赖恢复了。
			b.state = StateClosed
			b.failures = 0
		} else {
			// 探测失败 = 还没恢复，重新开始冷却。
			// 这里若不清零 failures 也无所谓：closed 分支会在下次进入时重建计数。
			b.state = StateOpen
			b.openedAt = b.now()
		}

	case StateOpen:
		// Allow 在 open 时返回 ErrOpen，调用方不应触及远端、也就不会 Report。
		// 走到这里的只可能是"延迟到达的旧结果"。忽略它，但**不改变**冷却计时：
		// 若在这里推后 openedAt，一个持续有旧结果回流的高并发场景会把熔断
		// 无限期延长下去（每次旧结果都重置计时）—— 那是"因为忙所以永远不解封"。
		// 冷却的起点只有一个：真正打开熔断的那一刻。

	default: // StateClosed
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.failThreshold {
			b.state = StateOpen
			b.openedAt = b.now()
		}
	}
}

// Abort 表示这次放行的调用**没有结论**（典型情况：调用方取消了 ctx），
// 因此不作为判断依赖是否恢复的证据。
//
// 必须存在这个方法：没有它，一个在半开期被放出去、又在拿到结果前被取消的探测，
// 会把 halfOpenInFlight 永久留在 true —— 于是熔断器再也放不出任何探测，
// 表现为"依赖早就恢复了，服务却一直 503 到重启"。
func (b *Breaker) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateHalfOpen {
		b.halfOpenInFlight = false
	}
}

// State 返回当前状态，供日志与测试观测。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
