-- 购物车、订单、库存、支付相关查询。

-- ============ 购物车 ============
-- name: ListCartItems :many
SELECT
    c.sku_id,
    s.product_id,
    s.name      AS sku_name,
    s.price_cents,
    s.stock,
    p.title,
    p.cover_url,
    c.qty
FROM cart_items c
JOIN skus s     ON s.id = c.sku_id
JOIN products p ON p.id = s.product_id
WHERE c.user_id = sqlc.arg('user_id')::uuid
ORDER BY c.updated_at DESC;

-- name: UpsertCartItem :exec
INSERT INTO cart_items (user_id, sku_id, qty)
VALUES (sqlc.arg('user_id')::uuid, sqlc.arg('sku_id')::uuid, sqlc.arg('qty')::int)
ON CONFLICT (user_id, sku_id)
DO UPDATE SET qty = EXCLUDED.qty, updated_at = now();

-- name: DeleteCartItem :exec
DELETE FROM cart_items
WHERE user_id = sqlc.arg('user_id')::uuid AND sku_id = sqlc.arg('sku_id')::uuid;

-- name: ClearCart :exec
DELETE FROM cart_items WHERE user_id = sqlc.arg('user_id')::uuid;

-- ============ 库存 ============
-- ★ 防超卖的核心：条件更新。影响行数为 0 就代表库存不足（返回 ErrNoRows），
-- 由行锁保证原子性，不需要分布式锁、也不需要分布式事务。
-- name: DecrementSkuStock :one
UPDATE skus
SET stock = stock - sqlc.arg('qty')::int, updated_at = now()
WHERE id = sqlc.arg('sku_id')::uuid
  AND stock >= sqlc.arg('qty')::int
RETURNING stock;

-- name: IncrementSkuStock :exec
UPDATE skus
SET stock = stock + sqlc.arg('qty')::int, updated_at = now()
WHERE id = sqlc.arg('sku_id')::uuid;

-- ============ 订单 ============
-- status 由调用方传入（取自 domain 状态机），不在 SQL 里硬编码 ——
-- 这样状态机是生产路径的一部分，而不是只活在测试里。
-- name: CreateOrder :one
INSERT INTO orders (order_no, user_id, status, total_cents, idempotency_key, expires_at)
VALUES (
    sqlc.arg('order_no'),
    sqlc.arg('user_id')::uuid,
    sqlc.arg('status')::text,
    sqlc.arg('total_cents')::bigint,
    sqlc.narg('idempotency_key')::text,
    sqlc.arg('expires_at')
)
RETURNING *;

-- name: CreateOrderItem :one
INSERT INTO order_items (order_id, sku_id, title_snapshot, unit_price_cents, qty)
VALUES (
    sqlc.arg('order_id')::uuid,
    sqlc.arg('sku_id')::uuid,
    sqlc.arg('title_snapshot'),
    sqlc.arg('unit_price_cents')::bigint,
    sqlc.arg('qty')::int
)
RETURNING *;

-- name: GetOrderByID :one
SELECT * FROM orders WHERE id = sqlc.arg('id')::uuid LIMIT 1;

-- name: GetOrderForUser :one
SELECT * FROM orders WHERE id = sqlc.arg('id')::uuid AND user_id = sqlc.arg('user_id')::uuid LIMIT 1;

-- name: GetOrderByOrderNo :one
SELECT * FROM orders WHERE order_no = sqlc.arg('order_no')::text LIMIT 1;

-- name: GetOrderByIdempotencyKey :one
SELECT * FROM orders
WHERE user_id = sqlc.arg('user_id')::uuid AND idempotency_key = sqlc.arg('idempotency_key')::text
LIMIT 1;

-- name: ListOrdersForUser :many
SELECT * FROM orders
WHERE user_id = sqlc.arg('user_id')::uuid
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY created_at DESC, id
LIMIT sqlc.arg('lim')::int OFFSET sqlc.arg('off')::int;

-- name: CountOrdersForUser :one
SELECT COUNT(*)::bigint FROM orders
WHERE user_id = sqlc.arg('user_id')::uuid
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text);

-- name: ListOrderItems :many
SELECT * FROM order_items WHERE order_id = sqlc.arg('order_id')::uuid ORDER BY id;

-- name: ListOrderItemsByOrderIDs :many
SELECT * FROM order_items WHERE order_id = ANY(sqlc.arg('order_ids')::uuid[]) ORDER BY order_id, id;

-- name: SetOrderPrepayID :exec
UPDATE orders SET prepay_id = sqlc.arg('prepay_id'), updated_at = now()
WHERE id = sqlc.arg('id')::uuid;

-- ★ 支付回调改状态用 CAS：只有当前仍在待支付时才改。
-- 影响行数为 0 = 这笔回调是重复的（或订单已取消），据此实现幂等。
-- name: MarkOrderPaid :one
UPDATE orders
SET status = 'PAID', paid_at = now(), updated_at = now()
WHERE order_no = sqlc.arg('order_no')
  AND status IN ('CREATED', 'PENDING_PAYMENT')
RETURNING *;

-- name: MarkOrderCancelled :one
UPDATE orders
SET status = 'CANCELLED', cancelled_at = now(), updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND status IN ('CREATED', 'PENDING_PAYMENT')
RETURNING *;

-- name: ListExpiredPendingOrders :many
SELECT * FROM orders
WHERE status IN ('CREATED', 'PENDING_PAYMENT') AND expires_at < now()
ORDER BY expires_at
LIMIT sqlc.arg('lim')::int;

-- ============ 支付事件 ============
-- ON CONFLICT DO NOTHING + RETURNING：冲突时没有返回行（sqlc 报 ErrNoRows），
-- 这正是"这笔回调此前已处理过"的判定依据。
-- name: InsertPaymentEvent :one
INSERT INTO payment_events (provider, out_trade_no, transaction_id, event_type, amount_cents, raw)
VALUES (
    sqlc.arg('provider'),
    sqlc.arg('out_trade_no'),
    sqlc.arg('transaction_id'),
    sqlc.arg('event_type'),
    sqlc.arg('amount_cents')::bigint,
    sqlc.arg('raw')::jsonb
)
ON CONFLICT (provider, transaction_id) DO NOTHING
RETURNING id;
