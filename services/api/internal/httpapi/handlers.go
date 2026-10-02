package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/httpapi/gen"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// Handler 实现契约生成的 StrictServerInterface。
//
// 这里刻意不做错误 → 状态码的映射：业务方法直接返回哨兵错误，
// 由 NewStrictHandlerWithOptions 的 ResponseErrorHandlerFunc 统一调用 WriteProblem。
// 映射只存在一处，加接口时不可能漏掉。
type Handler struct {
	svc *service.Service
}

var _ gen.StrictServerInterface = (*Handler)(nil)

func NewHandler(svc *service.Service) *Handler { return &Handler{svc: svc} }

// mustUser 取出当前登录用户；未认证返回 ErrUnauthorized。
func mustUser(ctx context.Context) (*auth.Claims, error) {
	claims, ok := auth.FromContext(ctx)
	if !ok {
		return nil, domain.ErrUnauthorized
	}
	return claims, nil
}

// ---------- 运维 ----------

func (h *Handler) Healthz(_ context.Context, _ gen.HealthzRequestObject) (gen.HealthzResponseObject, error) {
	return gen.Healthz200JSONResponse{Status: "ok"}, nil
}

// ---------- 认证 ----------

func (h *Handler) LoginWechat(ctx context.Context, req gen.LoginWechatRequestObject) (gen.LoginWechatResponseObject, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("%w: 请求体为空", auth.ErrInvalidCredentials)
	}
	sess, err := h.svc.LoginWechat(ctx, req.Body.Code, derefStr(req.Body.Nickname), derefStr(req.Body.AvatarUrl))
	if err != nil {
		return nil, err
	}
	return gen.LoginWechat200JSONResponse(toGenSession(sess)), nil
}

func (h *Handler) LoginMerchant(ctx context.Context, req gen.LoginMerchantRequestObject) (gen.LoginMerchantResponseObject, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("%w: 请求体为空", domain.ErrInvalidCredentials)
	}
	sess, err := h.svc.LoginMerchant(ctx, req.Body.Phone, req.Body.Password)
	if err != nil {
		return nil, err
	}
	return gen.LoginMerchant200JSONResponse(toGenSession(sess)), nil
}

func (h *Handler) RefreshToken(ctx context.Context, req gen.RefreshTokenRequestObject) (gen.RefreshTokenResponseObject, error) {
	if req.Body == nil {
		return nil, auth.ErrRefreshInvalid
	}
	sess, err := h.svc.Refresh(ctx, req.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return gen.RefreshToken200JSONResponse(toGenSession(sess)), nil
}

func (h *Handler) Logout(ctx context.Context, req gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	if req.Body != nil {
		if err := h.svc.Logout(ctx, req.Body.RefreshToken); err != nil {
			return nil, err
		}
	}
	return gen.Logout204Response{}, nil
}

// ---------- 当前用户 ----------

func (h *Handler) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	user, err := h.svc.GetUser(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	return gen.GetMe200JSONResponse(toGenUser(user)), nil
}

// ---------- 商品 ----------

func (h *Handler) ListProducts(ctx context.Context, req gen.ListProductsRequestObject) (gen.ListProductsResponseObject, error) {
	page := derefInt32(req.Params.Page, 1)
	pageSize := derefInt32(req.Params.PageSize, 20)

	rows, total, err := h.svc.ListProducts(ctx, derefStr(req.Params.Q), derefStr(req.Params.Category), page, pageSize)
	if err != nil {
		return nil, err
	}
	items := make([]gen.ProductSummary, 0, len(rows))
	for _, r := range rows {
		items = append(items, toGenProductSummary(r))
	}
	return gen.ListProducts200JSONResponse{
		Items:    items,
		Total:    int(total),
		Page:     int(page),
		PageSize: int(pageSize),
	}, nil
}

func (h *Handler) GetProduct(ctx context.Context, req gen.GetProductRequestObject) (gen.GetProductResponseObject, error) {
	p, skus, err := h.svc.GetProduct(ctx, req.ProductId.String())
	if err != nil {
		return nil, err
	}
	return gen.GetProduct200JSONResponse(toGenProductDetail(p, skus)), nil
}

// ---------- 购物车 ----------

func (h *Handler) GetCart(ctx context.Context, _ gen.GetCartRequestObject) (gen.GetCartResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	items, total, err := h.svc.GetCart(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	return gen.GetCart200JSONResponse(toGenCart(items, total)), nil
}

func (h *Handler) PutCartItem(ctx context.Context, req gen.PutCartItemRequestObject) (gen.PutCartItemResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, fmt.Errorf("%w: 请求体为空", domain.ErrConflict)
	}
	if err := h.svc.PutCartItem(ctx, claims.UserID, req.SkuId.String(), int32(req.Body.Qty)); err != nil {
		return nil, err
	}
	items, total, err := h.svc.GetCart(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	return gen.PutCartItem200JSONResponse(toGenCart(items, total)), nil
}

func (h *Handler) DeleteCartItem(ctx context.Context, req gen.DeleteCartItemRequestObject) (gen.DeleteCartItemResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeleteCartItem(ctx, claims.UserID, req.SkuId.String()); err != nil {
		return nil, err
	}
	items, total, err := h.svc.GetCart(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	return gen.DeleteCartItem200JSONResponse(toGenCart(items, total)), nil
}

// ---------- 订单 ----------

func (h *Handler) CreateOrder(ctx context.Context, req gen.CreateOrderRequestObject) (gen.CreateOrderResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil || len(req.Body.Items) == 0 {
		return nil, fmt.Errorf("%w: 下单明细为空", domain.ErrCartEmpty)
	}

	lines := make([]service.OrderLineInput, 0, len(req.Body.Items))
	for _, it := range req.Body.Items {
		lines = append(lines, service.OrderLineInput{SkuID: it.SkuId.String(), Qty: int32(it.Qty)})
	}

	idempotencyKey := ""
	if req.Params.IdempotencyKey != nil {
		idempotencyKey = *req.Params.IdempotencyKey
	}

	view, err := h.svc.CreateOrder(ctx, claims.UserID, idempotencyKey, lines)
	if err != nil {
		return nil, err
	}
	return gen.CreateOrder201JSONResponse(toGenOrder(view)), nil
}

func (h *Handler) ListOrders(ctx context.Context, req gen.ListOrdersRequestObject) (gen.ListOrdersResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	page := derefInt32(req.Params.Page, 1)
	pageSize := derefInt32(req.Params.PageSize, 20)
	status := ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}

	orders, total, err := h.svc.ListOrders(ctx, claims.UserID, status, page, pageSize)
	if err != nil {
		return nil, err
	}

	items := make([]gen.Order, 0, len(orders))
	for _, o := range orders {
		itemsList, err := h.svc.GetOrder(ctx, claims.UserID, o.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, toGenOrder(itemsList))
	}
	return gen.ListOrders200JSONResponse{
		Items:    items,
		Total:    int(total),
		Page:     int(page),
		PageSize: int(pageSize),
	}, nil
}

func (h *Handler) GetOrder(ctx context.Context, req gen.GetOrderRequestObject) (gen.GetOrderResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	view, err := h.svc.GetOrder(ctx, claims.UserID, req.OrderId.String())
	if err != nil {
		return nil, err
	}
	return gen.GetOrder200JSONResponse(toGenOrder(view)), nil
}

// ---------- 支付 ----------

func (h *Handler) PrepayOrder(ctx context.Context, req gen.PrepayOrderRequestObject) (gen.PrepayOrderResponseObject, error) {
	claims, err := mustUser(ctx)
	if err != nil {
		return nil, err
	}
	out, order, err := h.svc.Prepay(ctx, claims.UserID, req.OrderId.String())
	if err != nil {
		return nil, err
	}

	prepayID := out.PrepayID
	return gen.PrepayOrder200JSONResponse{
		OrderId:     toUUID(order.ID),
		OrderNo:     order.OrderNo,
		Provider:    gen.PrepayResultProvider(out.Provider),
		PrepayId:    &prepayID,
		AmountCents: gen.Money(order.TotalCents),
		Params:      out.Params,
	}, nil
}

// MockPayNotify 是 mock 支付专用入口：把请求体重新序列化后交给 provider 的
// ParseNotify 处理，这样"解析 → 幂等 → 改单"走的是与真实回调完全相同的那条路径，
// 而不是另开一条捷径（捷径测不出真问题）。
func (h *Handler) MockPayNotify(ctx context.Context, req gen.MockPayNotifyRequestObject) (gen.MockPayNotifyResponseObject, error) {
	if req.Body == nil || req.Body.OrderNo == "" {
		return nil, fmt.Errorf("%w: 缺少 order_no", payment.ErrInvalidNotify)
	}
	body, err := json.Marshal(map[string]any{
		"order_no":     req.Body.OrderNo,
		"amount_cents": derefInt64(req.Body.AmountCents),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", payment.ErrInvalidNotify, err)
	}

	res, err := h.svc.HandleNotify(ctx, body, http.Header{})
	if err != nil {
		return nil, err
	}
	return gen.MockPayNotify200JSONResponse{
		Handled: res.Handled,
		OrderNo: res.OrderNo,
		Status:  gen.OrderStatus(res.Status),
	}, nil
}
