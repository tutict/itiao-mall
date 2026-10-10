-- 商户商品维护与平台审核。
--
-- 三条贯穿本文件的规矩：
--   1. **所有商户侧查询都必须带 merchant_id 条件。** 少一个就是跨租户越权。
--      merchantStatusGate 只保证"这个商户已被放行经营"，它不保证
--      "这个商户只能改自己的商品" —— 后者是每条 SQL 自己的责任。
--   2. **所有状态流转都用带旧状态条件的 UPDATE（CAS）**，不用先查后写。
--      先 SELECT 再 UPDATE 之间有窗口，并发提交会互相覆盖状态。
--   3. **影响行数为 0 一律当 404**，不区分"不存在"与"已被处理"。

-- ============ 商户侧：列表与详情 ============

-- name: ListMerchantProducts :many
-- ★ merchant_id 出现在 WHERE 里是这个查询存在的全部意义。
-- 少它一个字符，这个接口就从"我的商品"变成"全平台商品列表"，
-- 而返回的 200 响应不会有任何异常特征 —— 越权在客户端完全不可见。
SELECT
    p.id,
    p.merchant_id,
    p.title,
    p.description,
    p.category,
    p.cover_url,
    p.status,
    p.reject_reason,
    p.submitted_at,
    p.reviewed_at,
    p.created_at,
    p.updated_at,
    COALESCE(MIN(s.price_cents), 0)::bigint AS min_price_cents,
    COALESCE(SUM(s.stock), 0)::bigint        AS total_stock
FROM products p
LEFT JOIN skus s ON s.product_id = p.id
WHERE p.merchant_id = sqlc.arg('merchant_id')::uuid
  AND (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status')::text)
GROUP BY p.id
ORDER BY p.updated_at DESC, p.id
LIMIT sqlc.arg('lim')::int OFFSET sqlc.arg('off')::int;

-- name: CountMerchantProducts :one
SELECT COUNT(*)::bigint
FROM products p
WHERE p.merchant_id = sqlc.arg('merchant_id')::uuid
  AND (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status')::text);

-- ★ 商户看自己的商品详情。merchant_id 与 id 一起出现在 WHERE 里，
-- 于是"改别人的商品 id"得到的是 404 而不是那条数据 ——
-- 跨租户访问统一收敛到 404，不区分"不存在"与"不是你的"。
--
-- 存在性检查写在应用层（见 service）而不是用两个查询：多一次往返，
-- 而这里的调用方是商户后台的列表页详情跳转，不在热路径上。
-- name: GetMerchantProduct :one
SELECT * FROM products
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid;

-- ============ 商户侧：写入 ============

-- name: CreateProduct :one
INSERT INTO products (merchant_id, title, description, category, cover_url, status)
VALUES (
    sqlc.arg('merchant_id')::uuid,
    sqlc.arg('title'),
    sqlc.arg('description'),
    sqlc.arg('category'),
    sqlc.arg('cover_url'),
    'DRAFT'
)
RETURNING *;

-- name: CreateSku :one
INSERT INTO skus (product_id, name, price_cents, stock)
VALUES (
    sqlc.arg('product_id')::uuid,
    sqlc.arg('name'),
    sqlc.arg('price_cents'),
    sqlc.arg('stock')
)
RETURNING *;

-- ★ 编辑商品。状态流转刻意写在 CASE 里：
--
--   DRAFT         -> DRAFT（普通编辑）
--   REJECTED      -> PENDING_REVIEW（改完自动重新提交。
--                    不给"驳回后原样再提交"留路 —— 那会让驳回变得没有意义，
--                    商户只要反复点一下就能绕过）
--   PENDING_REVIEW-> DRAFT（撤回。继续编辑已提交的商品等于换一个版本送审，
--                    审核员看到的与商户想的可能不是同一份）
--   ON_SALE/OFF_SALE -> DRAFT（**下架并回到草稿**。改在售商品必须先下架，
--                    这里一步到位：改完就是草稿，要重新提交审核才能再上架。
--                    绝不允许"改完仍在售" —— 那样平台审核就形同虚设）
--
-- 已通过审核的商品被编辑时清空 reviewed_by / reviewed_at：
-- 那两个字段记录的是"这一版被谁审的"。内容变了之后这个记录就不再成立，
-- 留着会让运营看到"已审核"却其实没有。
--
-- reject_reason 同样清空：它描述的是上一版的驳回理由，内容都换了，
-- 留着旧理由只会误导商户。
-- name: UpdateMerchantProduct :one
UPDATE products
SET title       = sqlc.arg('title'),
    description = sqlc.arg('description'),
    category    = sqlc.arg('category'),
    cover_url   = sqlc.arg('cover_url'),
    status = CASE
        WHEN status = 'REJECTED'      THEN 'PENDING_REVIEW'
        ELSE 'DRAFT'
    END,
    reject_reason = '',
    reviewed_by   = NULL,
    reviewed_at   = NULL,
    submitted_at  = CASE
        WHEN status = 'REJECTED' THEN now()
        ELSE NULL
    END,
    updated_at    = now()
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  -- ★ 只有草稿与被驳回的可编辑。ON_SALE 走到这里会变成"下架并回到草稿"，
  -- 由上面的 CASE 决定最终状态；PENDING_REVIEW 也被这条挡住，
  -- 因为它既不该被改也不该被静默撤回（撤回是显式动作，见 WithdrawProduct）。
  AND status IN ('DRAFT', 'REJECTED')
RETURNING *;

-- 删 SKU。价格与库存在编辑时被整体替换，所以先删后插。
-- 用 product_id 而不是 sku 列表做条件：service 层已经确保了
-- 这些 SKU 都属于这个商品（它刚拿到商品的 merchant_id 所有权）。
-- name: DeleteSkusByProduct :exec
DELETE FROM skus WHERE product_id = sqlc.arg('product_id')::uuid;

-- ★ 提交审核：CAS，只有 DRAFT 能提交。
--
-- 影响行数为 0 = 不是草稿（可能已被别人提交、或已在待审队列里）。
-- service 层据此回 409 而不是 404 —— 这里"存在但状态不对"对商户是有用的信息
-- （"你已经提交过了，去等审核"），与跨租户的 404 是两回事。
-- name: SubmitProductForReview :one
UPDATE products
SET status       = 'PENDING_REVIEW',
    submitted_at = now(),
    updated_at   = now()
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  AND status = 'DRAFT'
RETURNING *;

-- ★ 撤回：PENDING_REVIEW -> DRAFT。商户提交完发现填错了要改。
-- 没有这条边的话，商户只能等审核员驳回，而审核员可能几小时后才看。
-- name: WithdrawProduct :one
UPDATE products
SET status       = 'DRAFT',
    submitted_at = NULL,
    updated_at   = now()
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  AND status = 'PENDING_REVIEW'
RETURNING *;

-- ★ 下架：ON_SALE -> OFF_SALE。商户随时可以做（可能是缺货、可能是改价）。
-- name: OffShelfProduct :one
UPDATE products
SET status     = 'OFF_SALE',
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  AND status = 'ON_SALE'
RETURNING *;

-- ★ 上架：OFF_SALE -> ON_SALE。
--
-- 刻意**不要求**重新审核：这条边上的商品已经审过一次。
-- 而 ON_SALE -> ON_SALE 不存在（那是幂等空操作），
-- 所以"重复上架"会因 CAS 未命中而回 409，客户端能立刻知道。
-- name: OnShelfProduct :one
UPDATE products
SET status     = 'ON_SALE',
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  AND status = 'OFF_SALE'
RETURNING *;

-- name: DeleteMerchantProduct :one
-- ★ 只允许删草稿与被驳回的商品。
--
-- 在售商品不能删：它有历史订单（order_items 通过 sku_id 外键引用），
-- 删掉会让那些订单的明细失去归属。所以下架之后也删不掉 ——
-- 这是刻意的，"从架上消失"用下架表达，"根本不存在"用删除表达。
-- 强行删除被 order_items 引用的 sku 会触发外键错误，那是数据库在替我们守规矩。
DELETE FROM products
WHERE id = sqlc.arg('id')::uuid
  AND merchant_id = sqlc.arg('merchant_id')::uuid
  AND status IN ('DRAFT', 'REJECTED')
RETURNING *;

-- ============ 平台侧 ============

-- name: ListProductsForReview :many
-- 待审队列。按提交时间倒序 —— 最早的申请排最前，否则新申请会插到队尾，
-- 而审核员要处理的是"等得最久"的那批。
--
-- 支持按状态过滤而不只是查 PENDING_REVIEW：管理员需要看
-- "我今天驳回了哪些"这类列表，那是非待审状态。
--
-- ★ 排序必须带 id 作为 tie-break：`now()` 的精度取决于平台，
-- 同一事务里的多条提交可能拿到**完全相同**的 submitted_at。
-- 实测（2026-10-02，Windows）：连续 3 次提交里有 2 次时间戳相同，
-- 于是仅按 submitted_at 排序时它们的相对顺序由数据库随意决定 ——
-- 表现为"审核队列的顺序每次刷新都不一样"，而管理员会以为
-- 队列自己在动。ORDER BY ... , p.id 让排序成为全序、可复现。
SELECT
    p.id,
    p.merchant_id,
    p.title,
    p.description,
    p.category,
    p.cover_url,
    p.status,
    p.reject_reason,
    p.submitted_at,
    p.reviewed_at,
    p.created_at,
    p.updated_at,
    mp.shop_name,
    COALESCE(MIN(s.price_cents), 0)::bigint AS min_price_cents,
    COALESCE(SUM(s.stock), 0)::bigint        AS total_stock
FROM products p
JOIN merchant_profiles mp ON mp.user_id = p.merchant_id
LEFT JOIN skus s ON s.product_id = p.id
WHERE (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status')::text)
GROUP BY p.id, mp.shop_name
ORDER BY COALESCE(p.submitted_at, p.updated_at) DESC, p.id DESC
LIMIT sqlc.arg('lim')::int OFFSET sqlc.arg('off')::int;

-- name: CountProductsForReview :one
SELECT COUNT(*)::bigint
FROM products p
WHERE (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status')::text);

-- name: GetProductForReview :one
-- 平台看任意商品（不受 merchant_id 限制 —— 平台身份与商户身份刻意不重叠，
-- 见 access.go 里 /api/v1/admin 的注释）。
SELECT * FROM products WHERE id = sqlc.arg('id')::uuid;

-- ★ 审核 CAS：只有 PENDING_REVIEW 能被裁决。
--
-- 与商户入驻审核同一个道理：两个管理员同时点"通过"与"驳回"，
-- 普通 UPDATE 会后者覆盖前者，最终状态取决于谁的网络快，
-- 而日志里两条都写着"成功"。CAS 让第二个人的影响行数为 0。
--
-- ★ 通过与驳回的分流写在一个语句里，靠 CASE 区分：
--
--   通过时清空 reject_reason —— 否则上一次驳回的文案会留在
--     "已通过"的记录上，商户看到"已通过 + 上次的驳回理由"会以为审核没过。
--   驳回时必填理由由 service 层校验（见 ReviewProduct 的注释）。
--
-- 三种裁决（通过 / 驳回 / 强制下架）也在这条语句里分流：
--   ON_SALE 与 OFF_SALE 都可能被"强制下架"，此时不影响 submitted_at。
-- name: ReviewProduct :one
UPDATE products
SET status = sqlc.arg('status')::text,
    reject_reason = CASE
        WHEN sqlc.arg('status')::text = 'REJECTED' THEN sqlc.arg('reject_reason')
        ELSE ''
    END,
    reviewed_by = sqlc.arg('reviewed_by')::uuid,
    reviewed_at = now(),
    -- 被强制下架时保留 submitted_at：它记录的是"这一版提交过审核"这个事实，
    -- 与当前在不在售无关。抹掉它会让审计日志对不上。
    submitted_at = CASE
        WHEN sqlc.arg('status')::text IN ('ON_SALE', 'REJECTED') THEN COALESCE(submitted_at, now())
        ELSE submitted_at
    END,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND (
        -- 通过 / 驳回：只有待审的能被裁决
        (sqlc.arg('status')::text IN ('ON_SALE', 'REJECTED') AND status = 'PENDING_REVIEW')
        -- 强制下架：在售与已下架的都能被强制下架（前者是"立即停止销售"）
     OR (sqlc.arg('status')::text = 'OFF_SALE' AND status IN ('ON_SALE', 'OFF_SALE'))
      )
RETURNING *;
