package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
)

// Problem 是 RFC7807 风格的问题响应。对外只暴露稳定的 type/title/status，
// 内部细节（比如 SQL 错误）绝不外泄。
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteJSON 写 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("写 JSON 响应失败", slog.Any("err", err))
	}
}

// WriteProblem 把 domain 哨兵错误映射成 HTTP 状态码。
// 这是唯一的映射点 —— 业务层不需要知道 HTTP 的存在。
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	status, title := mapError(err)
	if status >= http.StatusInternalServerError {
		// 5xx 记完整错误，但对外只回一句泛化描述
		logging.From(r.Context()).Error("请求处理失败",
			slog.String("path", r.URL.Path),
			slog.Any("err", err),
		)
	}

	// 4xx 的 detail 也不能回 err 原文。
	//
	// 看着"只回 4xx 就没事"，但 err 链上常挂着第三方库的原文，而那些原文会把内部结构
	// 反射给任何能发请求的人。实测过真实响应：
	//
	//	{"status":409,"detail":"资源冲突: can't decode JSON body: json: cannot unmarshal
	//	 number into Go struct field PasswordLoginRequest.phone of type string"}
	//
	// 泄露了 Go 类型名与字段名 —— 攻击者据此就能画出请求体 schema，而字段名往往正是
	// 业务语义里抢不到、猜不着的部分。
	//
	// 但也不能一律抹掉：我们自己写的校验文案（"密码至少 8 位"、"驳回必须给出理由"）
	// 对用户是有用的，抹掉等于逼前端把错误提示写死。所以规则是**默认不外泄**，
	// 业务层用 domain.UserFacingf 显式声明的那段才给。
	detail := domain.ExposeUserFacing(err)

	// 4xx 也记一笔：原文只在日志里，但排查时需要能按 path 捞到。
	if status < http.StatusInternalServerError && detail == "" {
		logging.From(r.Context()).Debug("请求被拒",
			slog.String("path", r.URL.Path),
			slog.Any("err", err),
		)
	}

	WriteJSON(w, status, Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

func mapError(err error) (int, string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "资源不存在"
	case errors.Is(err, domain.ErrRateLimitUnavailable):
		// 503 而不是 429：这里的真实原因是"限流器坏了"，不是"你太频繁"。
		// 回 429 会让客户端把它当成自己的问题而退避不重试，
		// 于是限流器挂掉的整段时间里全站被客户端主动放弃。
		return http.StatusServiceUnavailable, "服务暂不可用"
	case errors.Is(err, domain.ErrRateLimited):
		return http.StatusTooManyRequests, "请求过于频繁"
	case errors.Is(err, domain.ErrPayloadTooLarge):
		return http.StatusRequestEntityTooLarge, "请求体过大"
	case errors.Is(err, domain.ErrLengthRequired):
		return http.StatusLengthRequired, "缺少 Content-Length"
	case errors.Is(err, domain.ErrInsufficientStock):
		return http.StatusConflict, "库存不足"
	case errors.Is(err, domain.ErrCartEmpty):
		return http.StatusBadRequest, "购物车为空"
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest, "参数校验失败"
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, "资源冲突"
	case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrPaymentNotPending):
		return http.StatusConflict, "状态不允许该操作"
	case errors.Is(err, domain.ErrPaymentAmountMismatch):
		// 400 而不是 500：这是对端报文的问题（金额对不上），不是我们坏了。
		// 回 500 会让支付平台按"我们暂时故障"的逻辑反复重投同一笔错误回调。
		return http.StatusBadRequest, "支付金额与订单不符"
	case errors.Is(err, domain.ErrIdempotentHit):
		return http.StatusOK, "幂等命中"
	case errors.Is(err, auth.ErrRefreshReused):
		return http.StatusUnauthorized, "刷新令牌已被使用，请重新登录"
	case errors.Is(err, auth.ErrRefreshInvalid), errors.Is(err, domain.ErrRefreshTokenInvalid),
		errors.Is(err, auth.ErrInvalidToken),
		errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrInvalidCredentials):
		return http.StatusUnauthorized, "未认证或凭证无效"
	// domain 与 auth 各有一套同义的 refresh 错误，两套都在 mapError 里显式登记。
	// 只登记一套的话，误用另一套就会掉进 default 变成 500 —— 用户拿到"服务内部错误"
	// 去重登，而真实原因只是令牌过期。
	case errors.Is(err, domain.ErrRefreshTokenReused):
		return http.StatusUnauthorized, "刷新令牌已被使用，请重新登录"
	case errors.Is(err, domain.ErrMerchantNotApproved):
		// 与普通 ErrForbidden 分开：客户端拿到这条应该跳「入驻进度」页，
		// 而不是显示"无权限"把用户晾在那儿。
		// 具体的当前状态（PENDING / REJECTED / …）由业务层用 UserFacing 带上。
		return http.StatusForbidden, "商户入驻未通过审核"
	case errors.Is(err, payment.ErrInvalidNotify), errors.Is(err, payment.ErrNotImplemented):
		// 支付侧的哨兵此前没有登记，全部掉进 default 变成 500。后果是客户端拿到
		// "服务内部错误"去重试，而真实原因只是报文格式不对 —— 重试一万次也一样。
		return http.StatusBadRequest, "支付回调格式非法"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "无权限"
	default:
		return http.StatusInternalServerError, "服务内部错误"
	}
}
