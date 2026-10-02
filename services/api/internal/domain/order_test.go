package domain

import (
	"errors"
	"testing"
)

// 1. 合法流转必须全部放行。
func TestOrderStatus_ValidTransitions(t *testing.T) {
	cases := []struct{ from, to OrderStatus }{
		{OrderCreated, OrderPendingPayment},
		{OrderCreated, OrderCancelled},
		{OrderPendingPayment, OrderPaid},
		{OrderPendingPayment, OrderCancelled},
		{OrderPaid, OrderShipped},
		{OrderPaid, OrderRefunding},
		{OrderShipped, OrderCompleted},
		{OrderShipped, OrderRefunding},
		{OrderRefunding, OrderRefunded},
	}
	for _, c := range cases {
		got, err := c.from.TransitionTo(c.to)
		if err != nil {
			t.Errorf("%s → %s 应当合法，却报错: %v", c.from, c.to, err)
			continue
		}
		if got != c.to {
			t.Errorf("%s → %s 返回了错误的状态 %s", c.from, c.to, got)
		}
	}
}

// 2. 非法流转必须被拒，且返回 ErrInvalidTransition（而不是别的错）。
func TestOrderStatus_InvalidTransitions(t *testing.T) {
	cases := []struct{ from, to OrderStatus }{
		{OrderCreated, OrderPaid},    // 不能跳过待支付
		{OrderCreated, OrderShipped}, // 不能跳过支付
		{OrderPendingPayment, OrderShipped},
		{OrderPaid, OrderCancelled},      // 已支付不能直接取消（要走退款）
		{OrderCompleted, OrderCancelled}, // 终态无出边
		{OrderRefunded, OrderPaid},       // 终态无出边
		{OrderShipped, OrderPaid},        // 不能回退
	}
	for _, c := range cases {
		if _, err := c.from.TransitionTo(c.to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s → %s 应当被拒且返回 ErrInvalidTransition，实际: %v", c.from, c.to, err)
		}
	}
}

// 3. 终态必须没有任何出边——遍历所有已知状态都不该被接受。
func TestOrderStatus_TerminalStatesHaveNoOutgoing(t *testing.T) {
	terminals := []OrderStatus{OrderCompleted, OrderCancelled, OrderRefunded}
	for _, term := range terminals {
		if !term.IsTerminal() {
			t.Errorf("%s 应当是终态", term)
		}
		for _, target := range []OrderStatus{
			OrderCreated, OrderPendingPayment, OrderPaid,
			OrderShipped, OrderCompleted, OrderCancelled,
			OrderRefunding, OrderRefunded,
		} {
			if _, err := term.TransitionTo(target); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("终态 %s 不该能流转到 %s，实际: %v", term, target, err)
			}
		}
	}
}

// 4. 未知状态（拼错的字符串）必须被判非法，而不是被当成合法状态放行。
func TestOrderStatus_UnknownStatusRejected(t *testing.T) {
	bogus := OrderStatus("PAIDD")
	if bogus.Valid() {
		t.Fatal("PAIDD 不该被判为合法状态")
	}
	if _, err := OrderCreated.TransitionTo(bogus); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("流转到未知状态应返回 ErrInvalidTransition，实际: %v", err)
	}
	if _, err := bogus.TransitionTo(OrderPaid); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("从未知状态出发应返回 ErrInvalidTransition，实际: %v", err)
	}
}

// 5. 不变量：每一个声明的状态都必须能从 CREATED 走到。
// 这条能在漏写某条边、或加了状态却忘了连边时立刻报红——比逐条断言更耐脏。
func TestOrderStatus_AllStatesReachableFromCreated(t *testing.T) {
	reached := map[OrderStatus]bool{OrderCreated: true}
	queue := []OrderStatus{OrderCreated}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range allowedTransitions[cur] {
			if !reached[next] {
				reached[next] = true
				queue = append(queue, next)
			}
		}
	}
	for state := range allowedTransitions {
		if !reached[state] {
			t.Errorf("状态 %s 从 CREATED 不可达——要么漏了边，要么是多余的状态", state)
		}
	}
}

// 6. 业务约束：退款只能从 PAID / SHIPPED 发起。
// 双向断言：既验证"该放行的放行了"，也验证"不该开的口子一个都没开"。
func TestOrderStatus_RefundOnlyFromPaidOrShipped(t *testing.T) {
	for state := range allowedTransitions {
		canRefund := state.CanTransitionTo(OrderRefunding)
		shouldAllow := state == OrderPaid || state == OrderShipped
		if canRefund != shouldAllow {
			t.Errorf("状态 %s 能否退款 = %v，期望 %v", state, canRefund, shouldAllow)
		}
	}
}
