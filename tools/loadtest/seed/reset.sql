-- 清空业务数据，但**保留表结构与迁移记录**。
--
-- 为什么用 TRUNCATE 而不是 DROP SCHEMA：
-- 后端 pgx 会缓存 prepared statement。DROP SCHEMA + 重建会让这些缓存失效，
-- 之后所有查询报 "cached plan must not change result type" —— 看着像后端挂了，
-- 其实只是清理方式选错了。
--
-- 顺序无关（CASCADE 会处理外键），RESTART IDENTITY 保留给未来引入序列时用。
TRUNCATE
    order_items,
    orders,
    payment_events,
    cart_items,
    skus,
    products,
    users,
    idempotency_keys
RESTART IDENTITY CASCADE;
