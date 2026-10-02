package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
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

	detail := ""
	if status < http.StatusInternalServerError {
		detail = err.Error()
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
	case errors.Is(err, domain.ErrIdempotentHit):
		return http.StatusOK, "幂等命中"
	case errors.Is(err, auth.ErrRefreshReused):
		return http.StatusUnauthorized, "刷新令牌已被使用，请重新登录"
	case errors.Is(err, auth.ErrRefreshInvalid), errors.Is(err, auth.ErrInvalidToken),
		errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrInvalidCredentials):
		return http.StatusUnauthorized, "未认证或凭证无效"
	case errors.Is(err, domain.ErrMerchantNotApproved):
		// 与普通 ErrForbidden 分开：客户端拿到这条应该跳「入驻进度」页，
		// 而不是显示"无权限"把用户晾在那儿。
		return http.StatusForbidden, "商户入驻未通过审核"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "无权限"
	default:
		return http.StatusInternalServerError, "服务内部错误"
	}
}
