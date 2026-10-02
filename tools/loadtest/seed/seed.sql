-- 压测与验收用的固定种子数据。
--
-- 三条纪律：
--   1. 全部用**确定性 ID**（不是 random），这样每个场景都能硬编码同一个 SKU。
--   2. 全部可重复执行（ON CONFLICT DO UPDATE），且会**重置库存** ——
--      否则第二次跑压测时库存已经被上一轮买走了，数字没有可比性。
--   3. 商品库存刻意放大：主压测链路测的是吞吐，不是"卖光了"。
--      超卖路径由 9999 号「热点 SKU」单独覆盖。

BEGIN;

-- ---------- 商户账号 ----------
-- 密码明文是 merchant123（本地/压测环境专用，生产绝不可用这个种子）。
INSERT INTO users (id, role, phone, password_hash, nickname)
VALUES (
    '99999999-0000-4000-8000-000000000001',
    'merchant',
    '13800000000',
    '$2a$10$7tskK/c0XD8nYLgZmVnIluMgiiPukVyuFgVSfknSFqoqWXwMnXXDC',
    '测试商户'
)
ON CONFLICT (id) DO UPDATE
SET phone = EXCLUDED.phone, password_hash = EXCLUDED.password_hash, updated_at = now();

-- ---------- 商品 ----------
INSERT INTO products (id, merchant_id, title, description, category, cover_url, status)
SELECT
    ('aaaaaaaa-0000-4000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
    '99999999-0000-4000-8000-000000000001'::uuid,
    '测试商品 ' || i,
    '这是第 ' || i || ' 个用于压测与验收的商品。',
    CASE (i % 4)
        WHEN 0 THEN 'digital'
        WHEN 1 THEN 'clothing'
        WHEN 2 THEN 'food'
        ELSE 'home'
    END,
    '',
    'ON_SALE'
FROM generate_series(1, 20) AS i
ON CONFLICT (id) DO UPDATE
SET title = EXCLUDED.title,
    description = EXCLUDED.description,
    category = EXCLUDED.category,
    status = EXCLUDED.status,
    updated_at = now();

-- ---------- SKU ----------
-- 每个商品 2 个 SKU，共 40 个。价格用 i*137 制造差异，避免所有单价相同掩盖金额计算错误。
INSERT INTO skus (id, product_id, name, price_cents, stock)
SELECT
    ('bbbbbbbb-0000-4000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
    ('aaaaaaaa-0000-4000-8000-' || lpad(to_hex(((i - 1) / 2) + 1), 12, '0'))::uuid,
    '规格 ' || CASE WHEN i % 2 = 1 THEN '标准版' ELSE '尊享版' END,
    1000 + i * 137,
    1000000
FROM generate_series(1, 40) AS i
ON CONFLICT (id) DO UPDATE
SET stock = EXCLUDED.stock,
    price_cents = EXCLUDED.price_cents,
    updated_at = now();

-- ---------- 热点 SKU（专用于超卖验证）----------
-- 库存只有 50，并发压它才能验证"绝不超卖"。
INSERT INTO skus (id, product_id, name, price_cents, stock)
VALUES (
    'bbbbbbbb-0000-4000-8000-000000009999',
    'aaaaaaaa-0000-4000-8000-000000000001',
    '热点秒杀规格',
    9900,
    50
)
ON CONFLICT (id) DO UPDATE
SET stock = EXCLUDED.stock, price_cents = EXCLUDED.price_cents, updated_at = now();

COMMIT;
