package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// Mock 是不依赖任何外部资质的支付实现。
// 它让"下单 → 支付 → 订单变已支付"这条闭环在没有任何商户号的情况下也能端到端跑通，
// 同时也是压测造支付回调的唯一手段。
type Mock struct{}

func NewMock() *Mock { return &Mock{} }

func (*Mock) Name() string { return "mock" }

func (*Mock) Prepay(_ context.Context, in PrepayInput) (PrepayOutput, error) {
	return PrepayOutput{
		Provider: "mock",
		PrepayID: "mock_prepay_" + in.OrderNo,
		Params: map[string]string{
			"provider":     "mock",
			"order_no":     in.OrderNo,
			"amount_cents": strconv.FormatInt(in.AmountCents, 10),
			// 客户端"跳转"到这个地址即可完成支付；压测直接 POST 它
			"notify_url": "/api/v1/pay/mock/notify",
		},
	}, nil
}

type mockNotifyBody struct {
	OrderNo       string `json:"order_no"`
	TransactionID string `json:"transaction_id"`
	AmountCents   int64  `json:"amount_cents"`
}

// ParseNotify 解析 mock 回调。
//
// transaction_id 缺省时由 order_no 派生 —— 这是刻意的：同一单重复触发会得到同一个
// transaction_id，从而命中 payment_events 的唯一约束，让"回调重复投递只生效一次"
// 这条幂等路径可以被真实地测出来。
func (*Mock) ParseNotify(_ context.Context, body []byte, _ http.Header) (NotifyEvent, error) {
	var b mockNotifyBody
	if err := json.Unmarshal(body, &b); err != nil {
		return NotifyEvent{}, fmt.Errorf("%w: %v", ErrInvalidNotify, err)
	}
	if b.OrderNo == "" {
		return NotifyEvent{}, fmt.Errorf("%w: 缺少 order_no", ErrInvalidNotify)
	}

	txn := b.TransactionID
	if txn == "" {
		sum := sha256.Sum256([]byte("mock-txn:" + b.OrderNo))
		txn = "mock_txn_" + hex.EncodeToString(sum[:12])
	}

	return NotifyEvent{
		Provider:      "mock",
		OutTradeNo:    b.OrderNo,
		TransactionID: txn,
		EventType:     "PAYMENT_SUCCESS",
		AmountCents:   b.AmountCents,
		Success:       true,
		Raw: map[string]any{
			"order_no":       b.OrderNo,
			"transaction_id": txn,
			"amount_cents":   b.AmountCents,
		},
	}, nil
}
