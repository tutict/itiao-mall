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

-- ---------- 平台运营账号 ----------
-- 密码明文是 admin123（本地/压测环境专用）。
-- 三个角色都必须有种子账号，否则「越权矩阵」这条验收在本地根本跑不起来 ——
-- 只有 user 和 merchant 的话，admin 那一整列都是没测过的。
INSERT INTO users (id, role, phone, password_hash, nickname)
VALUES (
    '88888888-0000-4000-8000-000000000001',
    'admin',
    '13700000000',
    '$2a$10$VDwE1pJ2Fus.wmJnRbgxI.4wcotR2Kj8jhLLaztY5zJEEQsqCu9/6',
    '平台运营'
)
ON CONFLICT (id) DO UPDATE
SET phone = EXCLUDED.phone, password_hash = EXCLUDED.password_hash, updated_at = now();

-- ---------- 商户入驻资料 ----------
-- 必须和商户账号一起建，不能指望迁移去补。
-- 迁移 0002 里那段"存量商户补 profile"只在**迁移执行的那一刻**扫一遍现有的商品归属；
-- 而压测/验收每次都会 reset + seed 把 users 清空重建 —— 重建出来的商户没有 profile，
-- 于是商户端每个业务接口都会把它判定为「未入驻」。
-- 结果是：压测门禁每跑一次，就把商户重置回"审核中"状态。这种坑在本地表现为
-- "昨天还好好的"，所以在这里一次说清。
--
-- 给 APPROVED：种子商户是已经在卖货的存量商户，不该被入驻审核流程卡住。
INSERT INTO merchant_profiles (user_id, shop_name, contact_name, contact_phone, status, reviewed_at)
VALUES (
    '99999999-0000-4000-8000-000000000001',
    '测试商户旗舰店',
    '测试联系人',
    '13800000000',
    'APPROVED',
    now()
)
ON CONFLICT (user_id) DO UPDATE
SET shop_name = EXCLUDED.shop_name,
    contact_name = EXCLUDED.contact_name,
    status = EXCLUDED.status,
    updated_at = now();

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
