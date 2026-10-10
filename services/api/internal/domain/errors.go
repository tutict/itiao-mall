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
	// ErrRateLimited 是限流命中。单独一个哨兵而不是复用 ErrConflict：
	// 客户端对两者的处理完全不同（429 + 退避重试 vs 409 + 提示改参数），
	// 混用会让重试策略失效 —— 对 409 重试是死循环。
	ErrRateLimited = errors.New("请求过于频繁")
	// ErrRateLimitUnavailable 是限流后端不可用且配置为 fail-closed。
	// 回 503 而不是 429：429 告诉客户端"你太频繁了，等会儿"，
	// 而这里的真实原因是"我们这边坏了"，让客户端退避重试才是对的。
	ErrRateLimitUnavailable = errors.New("限流服务暂不可用")
	// ErrPayloadTooLarge 是请求体超过上限。必须是独立的哨兵：
	// 它最终映射成 413，而 400 会让客户端以为"改改参数就能过"，
	// 于是拿同样的体积反复重试 —— 那正好是攻击者想要的效果。
	ErrPayloadTooLarge = errors.New("请求体过大")
	// ErrLengthRequired 是带 body 的请求没给 Content-Length（chunked 编码）。
	// 映射成 411。这不是"格式不对"，而是"你用了我们不接受的那种传法"——
	// 回 400 会让调用方去改内容，而他要改的是传输方式。
	ErrLengthRequired = errors.New("缺少 Content-Length")
)

// UserFacing 是一个显式声明"这段文案可以给客户端看"的错误。
//
// 为什么需要它：HTTP 层的错误响应默认**不回**错误原文，只回固定标题。
// 因为 err 链上常挂着第三方库的原文（json 解析、pgx、sqlc），实测过真实响应：
//
//	{"status":409,"detail":"资源冲突: can't decode JSON body: json: cannot unmarshal
//	 number into Go struct field PasswordLoginRequest.phone of type string"}
//
// 这把 Go 类型名与字段名白送给了任何能发请求的人 —— 而字段名往往正是业务语义里
// 抢不到、猜不着的部分（"哪个字段叫 merchant_profile_id"）。
//
// 但完全抹掉 detail 会误伤我们自己写的校验文案（"密码至少 8 位"、"驳回必须给出理由"），
// 那些对用户是有用的。所以改成显式声明：业务层用 UserFacing 包住的文案才外泄，
// 其余一律进日志。默认不外泄，要外泄得先想一下这句话给谁看。
type UserFacing struct {
	Msg string
	Err error
}

func (e *UserFacing) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *UserFacing) Unwrap() error { return e.Err }

// UserFacingf 构造一条可外泄的校验错误。
// 用法：return nil, domain.UserFacingf("密码至少 8 位", domain.ErrValidation)
func UserFacingf(msg string, err error) error {
	return &UserFacing{Msg: msg, Err: err}
}

// userFacingMsg 从错误链里取出可外泄的文案；没有就返回 ""。
// 只认我们自己构造的 UserFacing —— 第三方库的错误即使文案"看起来像人话"也不放行。
func userFacingMsg(err error) string {
	var uf *UserFacing
	if errors.As(err, &uf) {
		return uf.Msg
	}
	return ""
}

// ExposeUserFacing 供 httpapi 层调用：返回可安全外泄的文案，没有则空串。
func ExposeUserFacing(err error) string { return userFacingMsg(err) }
