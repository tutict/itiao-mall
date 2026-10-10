-- 用户与商品相关查询。
-- 注意：所有金额字段都是 bigint（分），不要在任何地方转成浮点。

-- union_id 可为空（用户未绑定开放平台时微信不返回 unionid），
-- 所以必须用 sqlc.narg 让它成为可空参数；直接写 = $1 的话 NULL 永远不会命中。
-- name: GetUserByWechatUnionID :one
SELECT * FROM users
WHERE (sqlc.narg('union_id')::text IS NOT NULL AND wechat_unionid = sqlc.narg('union_id')::text)
   OR (wechat_unionid IS NULL AND wechat_openid = sqlc.arg('open_id')::text)
LIMIT 1;

-- name: GetUserByPhone :one
SELECT * FROM users WHERE phone = sqlc.arg('phone')::text LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = sqlc.arg('id')::uuid LIMIT 1;

-- name: CreateWechatUser :one
INSERT INTO users (role, wechat_openid, wechat_unionid, nickname, avatar_url)
VALUES ('user', sqlc.arg('open_id'), sqlc.arg('union_id'), sqlc.arg('nickname'), sqlc.arg('avatar_url'))
RETURNING *;

-- name: CreateMerchantUser :one
INSERT INTO users (role, phone, password_hash, nickname)
VALUES ('merchant', sqlc.arg('phone'), sqlc.arg('password_hash'), sqlc.arg('nickname'))
RETURNING *;

-- ============ 商品 ============
-- 搜索与分类都是可选的，用 sqlc.narg 表达"参数为空则该条件不生效"，
-- 这样不用引 goqu 之类的动态 SQL 拼装库。
-- name: ListProducts :many
SELECT
    p.id,
    p.title,
    p.description,
    p.category,
    p.cover_url,
    p.status,
    p.created_at,
    COALESCE(MIN(s.price_cents), 0)::bigint AS min_price_cents
FROM products p
LEFT JOIN skus s ON s.product_id = p.id
WHERE p.status = 'ON_SALE'
  AND (sqlc.narg('q')::text IS NULL OR p.title ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('category')::text IS NULL OR p.category = sqlc.narg('category')::text)
GROUP BY p.id
ORDER BY p.created_at DESC, p.id
LIMIT sqlc.arg('lim')::int OFFSET sqlc.arg('off')::int;

-- name: CountProducts :one
SELECT COUNT(*)::bigint FROM products p
WHERE p.status = 'ON_SALE'
  AND (sqlc.narg('q')::text IS NULL OR p.title ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('category')::text IS NULL OR p.category = sqlc.narg('category')::text);

-- name: GetProductByID :one
SELECT * FROM products WHERE id = sqlc.arg('id')::uuid LIMIT 1;

-- name: ListSkusByProduct :many
SELECT * FROM skus WHERE product_id = sqlc.arg('product_id')::uuid ORDER BY price_cents, id;

-- name: ListSkusByIDs :many
SELECT * FROM skus WHERE id = ANY(sqlc.arg('ids')::uuid[]) ORDER BY id;

-- ★ 一次取回一页商品的全部 SKU。
--
-- 为什么需要它：商户后台的商品列表一页 20 个，若在 Go 里逐个查
-- ListSkusByProduct，那是 1 + 20 次往返。这一页每次打开都要走一遍，
-- 而它恰好是商户最常访问的界面。
--
-- 传入的是**已经在应用层确认过归属**的商品 id 列表 ——
-- 租户隔离由调用它的 ListMerchantProducts 完成（那条 SQL 自带 merchant_id），
-- 这里不再重复过滤：多一层 merchant_id 不会更安全，只会让两个地方
-- 各有一份"哪些 id 是允许的"的知识，改一处漏一处就是越权。
-- name: ListSkusByProductIDs :many
SELECT * FROM skus
WHERE product_id = ANY(sqlc.arg('ids')::uuid[])
ORDER BY product_id, price_cents, id;

-- name: GetSkuByID :one
SELECT * FROM skus WHERE id = sqlc.arg('id')::uuid LIMIT 1;
