package domain

import "errors"

// 哨兵错误：domain / service 层只返回这些，由 httpapi 层统一映射成 HTTP 状态码与
// RFC7807 风格的问题响应。这样业务层不需要知道 HTTP。
var (
	ErrNotFound            = errors.New("资源不存在")
	ErrConflict            = errors.New("资源冲突")
	ErrValidation          = errors.New("参数校验失败")
	ErrInsufficientStock   = errors.New("库存不足")
	ErrInvalidTransition   = errors.New("订单状态流转非法")
	ErrUnauthorized        = errors.New("未认证")
	ErrForbidden           = errors.New("无权限")
	ErrMerchantNotApproved = errors.New("商户入驻未通过审核")
	ErrInvalidCredentials  = errors.New("凭证无效")
	ErrRefreshTokenReused  = errors.New("刷新令牌已被使用（疑似重放）")
	ErrRefreshTokenInvalid = errors.New("刷新令牌无效或已过期")
	ErrIdempotentHit       = errors.New("幂等键命中，返回既有结果")
	ErrCartEmpty           = errors.New("购物车为空")
	ErrPaymentNotPending   = errors.New("订单不处于待支付状态")
)
