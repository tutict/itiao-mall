package domain

import "fmt"

// OrderStatus 是订单状态机的状态集合。
type OrderStatus string

const (
	OrderCreated        OrderStatus = "CREATED"
	OrderPendingPayment OrderStatus = "PENDING_PAYMENT"
	OrderPaid           OrderStatus = "PAID"
	OrderShipped        OrderStatus = "SHIPPED"
	OrderCompleted      OrderStatus = "COMPLETED"
	OrderCancelled      OrderStatus = "CANCELLED"
	OrderRefunding      OrderStatus = "REFUNDING"
	OrderRefunded       OrderStatus = "REFUNDED"
)

// allowedTransitions 是订单状态机唯一的真源。
//
//	CREATED → PENDING_PAYMENT → PAID → SHIPPED → COMPLETED
//	   └──────────────→ CANCELLED（超时/用户取消，释放库存）
//	PAID → REFUNDING → REFUNDED
//
// 终态（COMPLETED / CANCELLED / REFUNDED）没有再出去的边。
var allowedTransitions = map[OrderStatus][]OrderStatus{
	OrderCreated:        {OrderPendingPayment, OrderCancelled},
	OrderPendingPayment: {OrderPaid, OrderCancelled},
	OrderPaid:           {OrderShipped, OrderRefunding},
	OrderShipped:        {OrderCompleted, OrderRefunding},
	OrderCompleted:      {},
	OrderCancelled:      {},
	OrderRefunding:      {OrderRefunded},
	OrderRefunded:       {},
}

// IsTerminal 判断是否终态。
func (s OrderStatus) IsTerminal() bool {
	return len(allowedTransitions[s]) == 0
}

// Valid 判断是否是已知状态。
func (s OrderStatus) Valid() bool {
	_, ok := allowedTransitions[s]
	return ok
}

// CanTransitionTo 判断能否从当前状态流转到 next。
func (s OrderStatus) CanTransitionTo(next OrderStatus) bool {
	for _, allowed := range allowedTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// TransitionTo 校验并返回下一个状态；非法流转返回 ErrInvalidTransition。
func (s OrderStatus) TransitionTo(next OrderStatus) (OrderStatus, error) {
	if !s.Valid() {
		return s, fmt.Errorf("%w: 未知状态 %q", ErrInvalidTransition, s)
	}
	if !next.Valid() {
		return s, fmt.Errorf("%w: 未知目标状态 %q", ErrInvalidTransition, next)
	}
	if !s.CanTransitionTo(next) {
		return s, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, s, next)
	}
	return next, nil
}

// 库存预占释放的时机取决于状态流转，这里显式声明，避免散落在 service 里。
//
//	进入 CANCELLED：释放预占
//	进入 PAID：预占转为实扣
//
// CancelReleasesStock 判断流转到该状态是否需要释放库存预占。
func (s OrderStatus) CancelReleasesStock() bool {
	return s == OrderCancelled
}
