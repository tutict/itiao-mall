-- +goose Up

-- ============ 用户 ============
-- role 区分用户端与商户端；用户端走微信，商户端走手机号+密码。
-- 金额一律以「分」为单位的整数存储，绝不用浮点。
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role          text        NOT NULL DEFAULT 'user',
    wechat_openid  text       UNIQUE,
    wechat_unionid text       UNIQUE,
    phone         text        UNIQUE,
    password_hash text,
    nickname      text        NOT NULL DEFAULT '',
    avatar_url    text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'ACTIVE',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_role_check   CHECK (role IN ('user', 'merchant')),
    CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'BANNED')),
    -- 用户端必须至少有一个微信标识；商户端必须有手机号与密码哈希
    CONSTRAINT users_identity_check CHECK (
        (role = 'user'     AND (wechat_unionid IS NOT NULL OR wechat_openid IS NOT NULL))
     OR (role = 'merchant' AND phone IS NOT NULL AND password_hash IS NOT NULL)
    )
);

-- ============ 商品 ============
CREATE TABLE products (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id uuid        NOT NULL REFERENCES users (id),
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    category    text        NOT NULL DEFAULT '',
    cover_url   text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT 'DRAFT',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT products_status_check CHECK (status IN ('DRAFT', 'ON_SALE', 'OFF_SALE'))
);

CREATE INDEX idx_products_status_created ON products (status, created_at DESC);
CREATE INDEX idx_products_category       ON products (category);
CREATE INDEX idx_products_merchant       ON products (merchant_id);
-- 搜索用：先用 pg_trgm 之外的最简方案（ILIKE），后续再上全文索引
CREATE INDEX idx_products_title_lower    ON products (lower(title));

-- ============ SKU / 库存 ============
-- 库存直接放在 sku 行上：P0 的防超卖靠 "UPDATE ... WHERE stock >= n" 这一条
-- 条件更新的行锁来保证，不需要额外表。P2 再把热点库存挪到 Redis Lua。
CREATE TABLE skus (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id   uuid        NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    price_cents  bigint      NOT NULL,
    stock        integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT skus_price_nonneg CHECK (price_cents >= 0),
    CONSTRAINT skus_stock_nonneg CHECK (stock >= 0)
);

CREATE INDEX idx_skus_product ON skus (product_id);

-- ============ 购物车 ============
-- 一人一 SKU 一行，靠唯一约束做 upsert。
CREATE TABLE cart_items (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    sku_id     uuid        NOT NULL REFERENCES skus (id) ON DELETE CASCADE,
    qty        integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT cart_items_qty_pos CHECK (qty > 0),
    CONSTRAINT cart_items_user_sku_uniq UNIQUE (user_id, sku_id)
);

-- ============ 订单 ============
CREATE TABLE orders (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_no        text        NOT NULL UNIQUE,
    user_id         uuid        NOT NULL REFERENCES users (id),
    status          text        NOT NULL DEFAULT 'CREATED',
    total_cents     bigint      NOT NULL,
    -- 幂等键：客户端传的 Idempotency-Key。同一用户同一 key 只会有一单。
    idempotency_key text,
    prepay_id       text,
    paid_at         timestamptz,
    cancelled_at    timestamptz,
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_status_check CHECK (status IN (
        'CREATED', 'PENDING_PAYMENT', 'PAID', 'SHIPPED',
        'COMPLETED', 'CANCELLED', 'REFUNDING', 'REFUNDED'
    )),
    CONSTRAINT orders_total_nonneg CHECK (total_cents >= 0)
);

-- 幂等兜底：应用层 Redis 命中之外，数据库这层也必须拦住重复下单
CREATE UNIQUE INDEX orders_user_idem_uniq
    ON orders (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_orders_user_created ON orders (user_id, created_at DESC);
-- 超时扫描：只扫待支付的
CREATE INDEX idx_orders_pending_expires
    ON orders (expires_at)
    WHERE status IN ('CREATED', 'PENDING_PAYMENT');

CREATE TABLE order_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    sku_id           uuid    NOT NULL REFERENCES skus (id),
    -- 下单时快照，商品改价不影响历史订单
    title_snapshot   text    NOT NULL,
    unit_price_cents bigint  NOT NULL,
    qty              integer NOT NULL,
    CONSTRAINT order_items_qty_pos CHECK (qty > 0),
    CONSTRAINT order_items_price_nonneg CHECK (unit_price_cents >= 0)
);

CREATE INDEX idx_order_items_order ON order_items (order_id);

-- ============ 支付事件 ============
-- 支付回调可能重复投递（微信会重试）。这里用 transaction_id 唯一约束 + 状态机 CAS
-- 双保险，保证同一笔支付只生效一次。先落事件、再改订单状态，失败可重放。
CREATE TABLE payment_events (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider       text        NOT NULL,
    out_trade_no   text        NOT NULL,
    transaction_id text        NOT NULL,
    event_type     text        NOT NULL,
    amount_cents   bigint      NOT NULL DEFAULT 0,
    raw            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- 同一 provider 的同一笔交易只能落一次
    CONSTRAINT payment_events_txn_uniq UNIQUE (provider, transaction_id)
);

CREATE INDEX idx_payment_events_out_trade_no ON payment_events (out_trade_no);

-- ============ 幂等记录 ============
-- 通用幂等表：任一写接口都能用它兜底（Redis 挂了也不至于重复写）。
CREATE TABLE idempotency_keys (
    key        text        PRIMARY KEY,
    user_id    uuid        NOT NULL,
    scope      text        NOT NULL,
    response   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS payment_events;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS cart_items;
DROP TABLE IF EXISTS skus;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS users;
