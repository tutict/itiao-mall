package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// ============ 商品 ============

func (s *Service) ListProducts(ctx context.Context, q, category string, page, pageSize int32) ([]repo.ListProductsRow, int64, error) {
	lim, off := normalizePage(page, pageSize)
	params := repo.ListProductsParams{
		Q:        normalizeSearch(q),
		Category: normalizeSearch(category),
		Lim:      lim,
		Off:      off,
	}

	rows, err := s.Q.ListProducts(ctx, params)
	if err != nil {
		return nil, 0, fmt.Errorf("查询商品列表失败: %w", err)
	}
	total, err := s.Q.CountProducts(ctx, repo.CountProductsParams{Q: params.Q, Category: params.Category})
	if err != nil {
		return nil, 0, fmt.Errorf("统计商品数失败: %w", err)
	}
	return rows, total, nil
}

func (s *Service) GetProduct(ctx context.Context, productID string) (repo.Product, []repo.Sku, error) {
	p, err := s.Q.GetProductByID(ctx, productID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repo.Product{}, nil, domain.ErrNotFound
		}
		return repo.Product{}, nil, fmt.Errorf("查询商品失败: %w", err)
	}
	skus, err := s.Q.ListSkusByProduct(ctx, p.ID)
	if err != nil {
		return repo.Product{}, nil, fmt.Errorf("查询 SKU 失败: %w", err)
	}
	return p, skus, nil
}

// ============ 购物车 ============

func (s *Service) GetCart(ctx context.Context, userID string) ([]repo.ListCartItemsRow, int64, error) {
	items, err := s.Q.ListCartItems(ctx, userID)
	if err != nil {
		return nil, 0, fmt.Errorf("查询购物车失败: %w", err)
	}
	var total int64
	for _, it := range items {
		total += it.PriceCents * int64(it.Qty)
	}
	return items, total, nil
}

// PutCartItem 加购/改数量。
//
// 刻意不在这里校验库存 —— 购物车是"想要什么"，库存是下单时才裁决的事。
// 这里放行超库存，由响应里的 stock 字段让前端决定要不要禁用按钮；
// 否则压测和真实使用中都会出现"加购失败"这种反直觉的结果。
func (s *Service) PutCartItem(ctx context.Context, userID, skuID string, qty int32) error {
	if qty <= 0 {
		return s.Q.DeleteCartItem(ctx, repo.DeleteCartItemParams{UserID: userID, SkuID: skuID})
	}
	if _, err := s.Q.GetSkuByID(ctx, skuID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("查询 SKU 失败: %w", err)
	}
	if err := s.Q.UpsertCartItem(ctx, repo.UpsertCartItemParams{
		UserID: userID, SkuID: skuID, Qty: qty,
	}); err != nil {
		return fmt.Errorf("更新购物车失败: %w", err)
	}
	return nil
}

func (s *Service) DeleteCartItem(ctx context.Context, userID, skuID string) error {
	if err := s.Q.DeleteCartItem(ctx, repo.DeleteCartItemParams{UserID: userID, SkuID: skuID}); err != nil {
		return fmt.Errorf("删除购物车条目失败: %w", err)
	}
	return nil
}

// ============ 订单 ============

// CreateOrder 是 P0 最关键的一条路径：预占库存 + 建单 + 幂等，全部在一个事务里。
//
// 防超卖靠 DecrementSkuStock 的条件更新（WHERE stock >= qty），影响行数为 0 即库存不足，
// 由 Postgres 的行锁保证原子性 —— 不需要分布式锁，也不需要分布式事务。
func (s *Service) CreateOrder(
	ctx context.Context, userID, idempotencyKey string, lines []OrderLineInput,
) (*OrderView, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: 下单明细为空", domain.ErrCartEmpty)
	}

	// 幂等快路径：命中直接返回既有订单，不重放开事务。
	if idempotencyKey != "" {
		if existing, err := s.Q.GetOrderByIdempotencyKey(ctx, repo.GetOrderByIdempotencyKeyParams{
			UserID: userID, IdempotencyKey: idempotencyKey,
		}); err == nil {
			items, ierr := s.Q.ListOrderItems(ctx, existing.ID)
			if ierr != nil {
				return nil, fmt.Errorf("查询订单明细失败: %w", ierr)
			}
			return &OrderView{Order: existing, Items: items}, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("幂等查询失败: %w", err)
		}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.Q.WithTx(tx)

	type resolvedLine struct {
		sku repo.Sku
		qty int32
	}
	resolved := make([]resolvedLine, 0, len(lines))
	var total int64

	for _, line := range lines {
		if line.Qty <= 0 {
			return nil, fmt.Errorf("%w: 数量必须大于 0", domain.ErrConflict)
		}

		sku, err := q.GetSkuByID(ctx, line.SkuID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: SKU %s", domain.ErrNotFound, line.SkuID)
			}
			return nil, fmt.Errorf("查询 SKU 失败: %w", err)
		}

		// ★ 原子预占。ErrNoRows 即库存不足。
		if _, err := q.DecrementSkuStock(ctx, repo.DecrementSkuStockParams{
			SkuID: sku.ID, Qty: line.Qty,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: SKU %s 当前库存 %d，需要 %d",
					domain.ErrInsufficientStock, sku.ID, sku.Stock, line.Qty)
			}
			return nil, fmt.Errorf("扣减库存失败: %w", err)
		}

		total += sku.PriceCents * int64(line.Qty)
		resolved = append(resolved, resolvedLine{sku: sku, qty: line.Qty})
	}

	// 让状态机真正成为生产路径的一部分，而不是只活在测试里。
	status, err := domain.OrderCreated.TransitionTo(domain.OrderPendingPayment)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	order, err := q.CreateOrder(ctx, repo.CreateOrderParams{
		OrderNo:        newOrderNo(now),
		UserID:         userID,
		TotalCents:     total,
		IdempotencyKey: nullIfEmpty(idempotencyKey),
		ExpiresAt:      pgtypeTimestamptz(now.Add(s.PendingTTL)),
		Status:         string(status),
	})
	if err != nil {
		// 并发重复提交：唯一约束兜底。回滚后读回已经建好的那一单。
		if isUniqueViolation(err) && idempotencyKey != "" {
			_ = tx.Rollback(ctx)
			existing, gerr := s.Q.GetOrderByIdempotencyKey(ctx, repo.GetOrderByIdempotencyKeyParams{
				UserID: userID, IdempotencyKey: idempotencyKey,
			})
			if gerr != nil {
				return nil, fmt.Errorf("幂等回读失败: %w", gerr)
			}
			items, ierr := s.Q.ListOrderItems(ctx, existing.ID)
			if ierr != nil {
				return nil, fmt.Errorf("查询订单明细失败: %w", ierr)
			}
			return &OrderView{Order: existing, Items: items}, nil
		}
		return nil, fmt.Errorf("创建订单失败: %w", err)
	}

	items := make([]repo.OrderItem, 0, len(resolved))
	for _, r := range resolved {
		item, err := q.CreateOrderItem(ctx, repo.CreateOrderItemParams{
			OrderID:        order.ID,
			SkuID:          r.sku.ID,
			TitleSnapshot:  r.sku.Name,
			UnitPriceCents: r.sku.PriceCents,
			Qty:            r.qty,
		})
		if err != nil {
			return nil, fmt.Errorf("创建订单明细失败: %w", err)
		}
		items = append(items, item)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return &OrderView{Order: order, Items: items}, nil
}

func (s *Service) ListOrders(ctx context.Context, userID, status string, page, pageSize int32) ([]repo.Order, int64, error) {
	lim, off := normalizePage(page, pageSize)
	params := repo.ListOrdersForUserParams{
		UserID: userID,
		Status: normalizeSearch(status),
		Lim:    lim,
		Off:    off,
	}
	orders, err := s.Q.ListOrdersForUser(ctx, params)
	if err != nil {
		return nil, 0, fmt.Errorf("查询订单列表失败: %w", err)
	}
	total, err := s.Q.CountOrdersForUser(ctx, repo.CountOrdersForUserParams{
		UserID: userID, Status: params.Status,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("统计订单数失败: %w", err)
	}
	return orders, total, nil
}

// GetOrder 只返回属于该用户的订单 —— 越权访问一律当作"不存在"，不泄露订单是否存在。
func (s *Service) GetOrder(ctx context.Context, userID, orderID string) (*OrderView, error) {
	order, err := s.Q.GetOrderForUser(ctx, repo.GetOrderForUserParams{ID: orderID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("查询订单失败: %w", err)
	}
	items, err := s.Q.ListOrderItems(ctx, order.ID)
	if err != nil {
		return nil, fmt.Errorf("查询订单明细失败: %w", err)
	}
	return &OrderView{Order: order, Items: items}, nil
}

// ============ 支付 ============

func (s *Service) Prepay(ctx context.Context, userID, orderID string) (payment.PrepayOutput, repo.Order, error) {
	order, err := s.Q.GetOrderForUser(ctx, repo.GetOrderForUserParams{ID: orderID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return payment.PrepayOutput{}, repo.Order{}, domain.ErrNotFound
		}
		return payment.PrepayOutput{}, repo.Order{}, fmt.Errorf("查询订单失败: %w", err)
	}
	// 只有待支付的订单能发起支付。已支付/已取消都要挡住，否则会重复下单。
	if order.Status != string(domain.OrderCreated) && order.Status != string(domain.OrderPendingPayment) {
		return payment.PrepayOutput{}, repo.Order{}, fmt.Errorf("%w: 订单当前状态为 %s",
			domain.ErrPaymentNotPending, order.Status)
	}

	out, err := s.Payment.Prepay(ctx, payment.PrepayInput{
		OrderID:     order.ID,
		OrderNo:     order.OrderNo,
		AmountCents: order.TotalCents,
		Description: "itiao-mall 订单 " + order.OrderNo,
	})
	if err != nil {
		return payment.PrepayOutput{}, repo.Order{}, fmt.Errorf("预下单失败: %w", err)
	}
	if err := s.Q.SetOrderPrepayID(ctx, repo.SetOrderPrepayIDParams{
		ID: order.ID, PrepayID: &out.PrepayID,
	}); err != nil {
		return payment.PrepayOutput{}, repo.Order{}, fmt.Errorf("记录 prepay_id 失败: %w", err)
	}
	return out, order, nil
}

// NotifyResult 是支付回调的处理结果。
type NotifyResult struct {
	Handled bool
	OrderNo string
	Status  string
}

// HandleNotify 处理支付回调。幂等靠两道闸：
//  1. payment_events 的 (provider, transaction_id) 唯一约束 —— 同一笔交易只落一次；
//  2. MarkOrderPaid 的 CAS —— 只有订单仍在待支付时才改状态。
//
// 先落事件再改状态：万一改状态失败，事件已留下痕迹，可据此重放。
func (s *Service) HandleNotify(ctx context.Context, body []byte, headers http.Header) (*NotifyResult, error) {
	ev, err := s.Payment.ParseNotify(ctx, body, headers)
	if err != nil {
		return nil, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Q.WithTx(tx)

	rawJSON := mustJSON(ev.Raw)
	if _, err := q.InsertPaymentEvent(ctx, repo.InsertPaymentEventParams{
		Provider:      ev.Provider,
		OutTradeNo:    ev.OutTradeNo,
		TransactionID: ev.TransactionID,
		EventType:     ev.EventType,
		AmountCents:   ev.AmountCents,
		Raw:           rawJSON,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 唯一约束冲突：这笔交易此前已处理过。
			_ = tx.Rollback(ctx)
			order, oerr := s.Q.GetOrderByOrderNo(ctx, ev.OutTradeNo)
			if oerr != nil {
				return nil, fmt.Errorf("幂等命中后查询订单失败: %w", oerr)
			}
			return &NotifyResult{Handled: false, OrderNo: order.OrderNo, Status: order.Status}, nil
		}
		return nil, fmt.Errorf("写入支付事件失败: %w", err)
	}

	order, err := q.MarkOrderPaid(ctx, ev.OutTradeNo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 事件是新的，但订单已不在待支付状态（已被别的交易支付/已取消）。
			// 回滚事件插入，保持"事件与状态变更同生共死"。
			_ = tx.Rollback(ctx)
			cur, cerr := s.Q.GetOrderByOrderNo(ctx, ev.OutTradeNo)
			if cerr != nil {
				return nil, fmt.Errorf("查询订单失败: %w", cerr)
			}
			s.Logger.Warn("支付回调到达时订单不在待支付状态",
				"order_no", cur.OrderNo, "status", cur.Status, "transaction_id", ev.TransactionID)
			return &NotifyResult{Handled: false, OrderNo: cur.OrderNo, Status: cur.Status}, nil
		}
		return nil, fmt.Errorf("更新订单状态失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return &NotifyResult{Handled: true, OrderNo: order.OrderNo, Status: order.Status}, nil
}
