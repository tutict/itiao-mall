-- +goose Up

-- ==================== refresh token 的权威记录 ====================
--
-- 为什么要把 refresh token 落库，而不是只留在 Redis
-- ---------------------------------------------------------
-- 原实现里 refresh token 的全部状态都在 Redis。这有一个致命的不对称：
-- **Redis 里的"删掉旧 token"和"写入新 token"是两次独立的网络往返**，
-- 两次之间进程可能挂掉 / 网络可能断。结果是一个既没被消费、也没被签发的
-- 状态 —— 用户手里的 refresh token 无效了，而服务端没有任何记录能解释这件事，
-- 排查时只能靠猜。
--
-- 更要紧的是**谁说了算**这件事没有答案。Redis 挂了 / 被 flush / 主从切换丢数据，
-- 已签发的 refresh token 就凭空失效；反过来 Redis 里有条记录而库里什么都没有，
-- 我们也无法判断它是"合法的旧数据"还是"脏数据"。
--
-- 落库之后职责就清楚了：
--   **Postgres 是最终权威**（持久、可查、能对账、能审计）。
--   Redis 是**加速层 + 互斥层**：它让「旧 token 只能用一次」这件事在
--   并发下用一次 Lua 就能判定，不必每次都去数据库拿行锁。
-- 两者不一致时一律以 Postgres 为准（见 service 层的裁决逻辑）。
--
-- 为什么只存哈希不存明文
-- ---------------------------------------------------------
-- refresh token 是**持有即等同凭证**的东西（拿到它就能换 access token）。
-- 数据库被读走（备份泄露、SQL 注入读表、日志里有人打了 select *）时，
-- 存明文等于所有在线用户的会话同时失窃。存 SHA-256 之后，攻击者拿到
-- 哈希也无法完成"拿 token 去换 access token"这一步 —— 他没有明文。
--
-- 代价：无法在库里"用明文反查"（正常需求其实不需要，都是拿 token 直接算哈希）。
-- 另一个代价是**必须能承受哈希碰撞**。256 位截断到 64 个十六进制字符（256 bit）
-- 的生日碰撞概率在 10^9 条记录下仍低于 10^-9，可以忽略。
--
-- 为什么不用 HMAC
-- ---------------------------------------------------------
-- HMAC 能防"攻击者自己造一个 token 提交，看哈希会不会命中"这类探测。
-- 但那需要攻击者先能提交任意 token —— 提交明文 token 时算出的 HMAC 与
-- 他想冒充的那个人的 HMAC 无关，攻击者本来就得先知道明文。
-- 也就是说 HMAC 防的是"查表泄露"，而 SHA-256 已经让查表失去意义。
-- 保持简单优先。

CREATE TABLE refresh_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- family_id 标识"同一次登录衍生出的一串 token"。轮换时继承，
    -- 重放时整个 family 一起吊销（见 auth/refresh.go 的家族概念）。
    family_id     uuid        NOT NULL,
    -- 存 SHA-256(token) 的十六进制小写；不存明文，理由见上。
    token_hash    text        NOT NULL UNIQUE,
    role          text        NOT NULL,
    status        text        NOT NULL DEFAULT 'ACTIVE',
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    rotated_at    timestamptz,
    revoked_at    timestamptz,
    CONSTRAINT refresh_tokens_status_check
        CHECK (status IN ('ACTIVE', 'ROTATED', 'REVOKED'))
);

-- 轮换时的查询形态：拿 token_hash 定位行。上面的 UNIQUE 已经建了索引，
-- 这里不再重复声明。唯一性是这套设计的地基 —— 少这一条约束，
-- 同一个 token 就能在库里存在两行，于是"旧 token 只被使用一次"从库里就不成立。
--
-- 登出/吊销的查询形态：按 family 扫。
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens (family_id, status);

-- 按用户吊销全部会话（"我的所有设备都登出"）。
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id, status);

-- 过期清理扫描：只扫已过期的。这是唯一需要它的场景：
-- 过期 token 不必立刻删（校验时看 expires_at 即可），但不能无限堆积。
CREATE INDEX idx_refresh_tokens_expires ON refresh_tokens (expires_at);

-- 登录时补齐存量：Redis 里的存量 refresh token 在本表没有对应行。
-- 决策是**不补**。理由：
--   1. 开发阶段，存量会话作废的代价是零（重新登录一次）。
--   2. 补记录需要把明文 token 拿去算哈希 —— 而 Redis 里只有明文 key，
--      要枚举全库才能拿全，等于为了兼容而引入一次性的大范围敏感数据读取。
--   3. 迁移期同时承认两个事实（"Redis 有的都算数"）会把本表的作用稀释掉。
-- 存量会话统一失效这件事本身也是可接受的：它顺带验证了"登录会写新记录"这条路径。
--
-- 若未来要平滑迁移，正确做法是双写期（先上线双写，滚完再切只读 DB），
-- 而不是在一个迁移里把两个数据源强行对齐。

-- +goose Down
DROP TABLE IF EXISTS refresh_tokens;
