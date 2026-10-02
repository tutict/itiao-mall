// Package payment 定义支付端口。
//
// 之所以先定接口再写实现：用户目前没有微信支付商户号，真实支付无法联调。
// 把 mock 与微信做成同一个接口的两个实现，配置切换即可 —— 资质到位后
// 只需要让 wechat 实现通过单测，不需要动业务代码。
package payment

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrNotImplemented = errors.New("该支付方式尚未实现")
	ErrInvalidNotify  = errors.New("支付回调格式非法")
)

// PrepayInput 是预下单的入参。
type PrepayInput struct {
	OrderID     string
	OrderNo     string
	AmountCents int64
	Description string
}

// PrepayOutput 是客户端调起支付所需的参数。
type PrepayOutput struct {
	Provider string
	PrepayID string
	Params   map[string]string
}

// NotifyEvent 是归一化后的支付回调事件。
// 真实微信有复杂的验签与解密，但吐出来必须是这个统一形状。
type NotifyEvent struct {
	Provider      string
	OutTradeNo    string
	TransactionID string
	EventType     string
	AmountCents   int64
	Success       bool
	Raw           map[string]any
}

// Provider 是支付端口。
type Provider interface {
	// Name 返回 provider 标识，与配置值一致（mock / wechat）。
	Name() string
	// Prepay 预下单，返回客户端调起支付所需参数。
	Prepay(ctx context.Context, in PrepayInput) (PrepayOutput, error)
	// ParseNotify 解析并校验回调，返回归一化事件。
	// 真实实现要在这里完成验签与解密 —— 验签失败必须返回错误，绝不能放行。
	ParseNotify(ctx context.Context, body []byte, headers http.Header) (NotifyEvent, error)
}
