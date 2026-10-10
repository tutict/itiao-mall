-- 商户入驻资料与平台审核相关查询。

-- ============ 入驻状态（鉴权用） ============
-- 专供 merchantStatusGate 使用：每次 /api/v1/merchant/* 请求都要查一次。
--
-- 为什么必须查库而不是读 JWT claims：claims 是自包含的。平台把商户驳回之后，
-- 他手里那个 access token 在过期之前一直有效 —— 如果把审核状态编进 claims，
-- 就等于留了一个「驳回后仍能操作」的窗口，而这个窗口恰好出现在最需要立刻止损的时刻。
-- 多一次 PK 查询换掉一个越权窗口，这笔账很划算。
--
-- 查不到行 = 从未入驻，调用方按「未通过」处理（fail closed）。
-- name: GetMerchantStatus :one
SELECT status FROM merchant_profiles WHERE user_id = sqlc.arg('user_id')::uuid;

-- ============ 入驻资料 ============

-- 审核员在商品审核队列里要靠店名对上号。没有它，一个混着几十家店商品的
-- 列表就只能靠 merchant_id 去别处查 —— 而那要多一次跳转。
--
-- 单独一条查询而不是复用 GetMerchantProfile：后者 JOIN 了 users 并返回
-- 十余个字段，这里只要一个名字。少查的字段不是优化，是避免把
-- 「商户的手机号」这类字段带进一个只需要店名的调用链。
-- name: GetMerchantShopName :one
SELECT shop_name FROM merchant_profiles WHERE user_id = sqlc.arg('user_id')::uuid;
-- name: GetMerchantProfile :one
SELECT
    mp.user_id,
    mp.shop_name,
    mp.contact_name,
    mp.contact_phone,
    mp.description,
    mp.logo_url,
    mp.status,
    mp.reject_reason,
    mp.reviewed_at,
    mp.created_at,
    mp.updated_at,
    u.phone
FROM merchant_profiles mp
JOIN users u ON u.id = mp.user_id
WHERE mp.user_id = sqlc.arg('user_id')::uuid;

-- 注册时建资料。用 ON CONFLICT DO NOTHING：冲突时不返回行（sqlc 报 ErrNoRows），
-- 调用方据此知道「这个账号已经有入驻资料了」，而不是把它当成写入失败。
-- name: CreateMerchantProfile :one
INSERT INTO merchant_profiles (user_id, shop_name, contact_name, contact_phone)
VALUES (
    sqlc.arg('user_id')::uuid,
    sqlc.arg('shop_name'),
    sqlc.arg('contact_name'),
    sqlc.arg('contact_phone')
)
ON CONFLICT (user_id) DO NOTHING
RETURNING *;

-- ★ 更新 / 重新提交。状态流转刻意写在 SQL 的 CASE 里而不是先查后写：
-- 先 SELECT 再 UPDATE 之间有一个窗口，两次提交并发时可能互相覆盖状态。
-- 单条语句天然原子。
--
-- 规则：
--   REJECTED  -> PENDING（并清空驳回理由）—— 改了资料就是一次重新提交
--   其余状态  -> 保持不动 —— 审核通过后改联系方式不重新触发审核（改个电话就重审的话，
--               商户会倾向于干脆不改，平台数据反而更陈旧）；SUSPENDED 也不会被一次编辑洗白
--
-- 这条规则由 TestUpsertMerchantProfile_StatusMachine 用真实数据库四种状态各验一遍。
-- name: UpsertMerchantProfile :one
INSERT INTO merchant_profiles (user_id, shop_name, contact_name, contact_phone, description, logo_url)
VALUES (
    sqlc.arg('user_id')::uuid,
    sqlc.arg('shop_name'),
    sqlc.arg('contact_name'),
    sqlc.arg('contact_phone'),
    sqlc.arg('description'),
    sqlc.arg('logo_url')
)
ON CONFLICT (user_id) DO UPDATE SET
    shop_name     = EXCLUDED.shop_name,
    contact_name  = EXCLUDED.contact_name,
    contact_phone = EXCLUDED.contact_phone,
    description   = EXCLUDED.description,
    logo_url      = EXCLUDED.logo_url,
    status        = CASE
                        WHEN merchant_profiles.status = 'REJECTED' THEN 'PENDING'
                        ELSE merchant_profiles.status
                    END,
    reject_reason = CASE
                        WHEN merchant_profiles.status = 'REJECTED' THEN ''
                        ELSE merchant_profiles.reject_reason
                    END,
    updated_at    = now()
RETURNING *;

-- ============ 平台审核 ============
-- name: ListMerchantProfiles :many
SELECT
    mp.user_id,
    mp.shop_name,
    mp.contact_name,
    mp.contact_phone,
    mp.description,
    mp.logo_url,
    mp.status,
    mp.reject_reason,
    mp.reviewed_at,
    mp.created_at,
    mp.updated_at,
    u.phone
FROM merchant_profiles mp
JOIN users u ON u.id = mp.user_id
WHERE (sqlc.narg('status')::text IS NULL OR mp.status = sqlc.narg('status')::text)
ORDER BY mp.created_at DESC, mp.user_id
LIMIT sqlc.arg('lim')::int OFFSET sqlc.arg('off')::int;

-- name: CountMerchantProfiles :one
SELECT COUNT(*)::bigint
FROM merchant_profiles mp
WHERE (sqlc.narg('status')::text IS NULL OR mp.status = sqlc.narg('status')::text);

-- ★ CAS 审核：只有仍在 PENDING 的申请能被裁决。
--
-- 为什么必须是 CAS 而不是普通 UPDATE：两个管理员同时打开同一条待审申请，
-- 一个点「通过」一个点「驳回」。普通 UPDATE 会后者覆盖前者，最终状态取决于
-- 谁的网络快 —— 而日志里两条都写着"成功"。CAS 让第二个人的影响行数为 0，
-- 返回 404，他立刻就会知道"这条已经被别人处理了"。
--
-- 影响行数为 0（ErrNoRows）的含义是「这条申请不存在，或已被处理」——
-- 对调用方而言都该当作 404：既不泄露"存在但已被处理"，也不需要区分。
-- name: ReviewMerchantProfile :one
UPDATE merchant_profiles
SET status        = sqlc.arg('status')::text,
    reject_reason = sqlc.arg('reject_reason'),
    reviewed_by   = sqlc.arg('reviewed_by')::uuid,
    reviewed_at   = now(),
    updated_at    = now()
WHERE user_id = sqlc.arg('user_id')::uuid
  AND status = 'PENDING'
RETURNING *;
