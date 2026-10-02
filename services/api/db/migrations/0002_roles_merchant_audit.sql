-- +goose Up

-- ============================ 引入平台运营角色 ============================
-- admin 负责审核商户入驻与商品上架。这是第三个角色，前两个是 user / merchant。
--
-- 注意：改 users_role_check 的同时**必须**改 users_identity_check。
-- 那条 CHECK 原本只列举了 user 和 merchant 两种身份分支，admin 两条都不满足，
-- 会被直接拦下插入失败。这类"改了一处忘另一处"的约束是最容易在本地测不出来、
-- 上线建号时才炸的 —— 所以两条放在同一个迁移里改。
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('user', 'merchant', 'admin'));

ALTER TABLE users DROP CONSTRAINT users_identity_check;
ALTER TABLE users ADD CONSTRAINT users_identity_check CHECK (
    (role = 'user'               AND (wechat_unionid IS NOT NULL OR wechat_openid IS NOT NULL))
 OR (role IN ('merchant', 'admin') AND phone IS NOT NULL AND password_hash IS NOT NULL)
);

-- ========================== 商户资料与入驻审核 ==========================
-- 为什么不把店铺信息放进 users：店铺是独立实体（店名、logo、联系方式、审核状态），
-- 账号只管身份与凭证。将来要支持「一个主体多店铺」或「店铺加店员」时扩展这张表即可，
-- users 不用动。
--
-- 审核状态刻意**不放进 JWT claims**：claims 是自包含的，审核拒绝之后老 access token
-- 照样有效，那等于留了一个越权窗口。所以审核状态一律查库判定（见 service 层）。
CREATE TABLE merchant_profiles (
    user_id       uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    shop_name     text        NOT NULL,
    contact_name  text        NOT NULL DEFAULT '',
    contact_phone text        NOT NULL DEFAULT '',
    description   text        NOT NULL DEFAULT '',
    logo_url      text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'PENDING',
    -- 驳回理由：不给商户看理由的话，商户只能反复盲提交
    reject_reason text        NOT NULL DEFAULT '',
    reviewed_by   uuid        REFERENCES users (id),
    reviewed_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT merchant_profiles_status_check
        CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'SUSPENDED'))
);

-- 审核列表按状态倒序翻页
CREATE INDEX idx_merchant_profiles_status ON merchant_profiles (status, created_at DESC);

-- 存量商户补 profile：从 products 的归属反推，一律置为 APPROVED。
-- 这些商户是 P0 阶段用种子数据建的、已经在卖货了。如果不补这一行，迁移之后
-- 它们会因为「查不到 profile」被商户端整体拒掉 —— 等于迁移顺手把整个商城下线了。
INSERT INTO merchant_profiles (user_id, shop_name, status, reviewed_at)
SELECT DISTINCT p.merchant_id,
                 COALESCE(NULLIF(u.nickname, ''), '未命名店铺'),
                 'APPROVED',
                 now()
FROM products p
         JOIN users u ON u.id = p.merchant_id
WHERE NOT EXISTS (SELECT 1 FROM merchant_profiles mp WHERE mp.user_id = p.merchant_id);

-- ============================ 商品审核状态 ============================
-- 新增 PENDING_REVIEW（已提交待审）与 REJECTED（审核驳回）。
-- 状态机：DRAFT -> PENDING_REVIEW -> ON_SALE / REJECTED，以及 ON_SALE <-> OFF_SALE。
-- 商户不能直接把商品改成 ON_SALE —— 只能提交审核，上架必须由 admin 放行。
ALTER TABLE products DROP CONSTRAINT products_status_check;
ALTER TABLE products ADD CONSTRAINT products_status_check
    CHECK (status IN ('DRAFT', 'PENDING_REVIEW', 'ON_SALE', 'OFF_SALE', 'REJECTED'));

ALTER TABLE products
    ADD COLUMN reject_reason text        NOT NULL DEFAULT '',
    ADD COLUMN submitted_at  timestamptz,
    ADD COLUMN reviewed_by   uuid        REFERENCES users (id),
    ADD COLUMN reviewed_at   timestamptz;

-- 待审队列的查询形态：按状态过滤 + 按提交时间倒序
CREATE INDEX idx_products_pending_review
    ON products (submitted_at DESC)
    WHERE status = 'PENDING_REVIEW';

-- +goose Down
-- 注意：回滚是有损的。如果库里已经存在 admin 用户，或存在 PENDING_REVIEW / REJECTED
-- 状态的商品，下面的约束会拒绝回滚（这是刻意的 —— 宁可报错也不静默丢状态）。
ALTER TABLE products
    DROP COLUMN reviewed_at,
    DROP COLUMN reviewed_by,
    DROP COLUMN submitted_at,
    DROP COLUMN reject_reason;

ALTER TABLE products DROP CONSTRAINT products_status_check;
ALTER TABLE products ADD CONSTRAINT products_status_check
    CHECK (status IN ('DRAFT', 'ON_SALE', 'OFF_SALE'));

DROP TABLE merchant_profiles;

ALTER TABLE users DROP CONSTRAINT users_identity_check;
ALTER TABLE users ADD CONSTRAINT users_identity_check CHECK (
    (role = 'user'     AND (wechat_unionid IS NOT NULL OR wechat_openid IS NOT NULL))
 OR (role = 'merchant' AND phone IS NOT NULL AND password_hash IS NOT NULL)
);

ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'merchant'));
