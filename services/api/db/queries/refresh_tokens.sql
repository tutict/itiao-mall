-- refresh token 记录。Postgres 是最终权威来源，Redis 只是加速与互斥层
-- （理由见 db/migrations/0003_refresh_tokens.sql 的顶部注释）。

-- name: InsertRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_hash, role, expires_at)
VALUES (
    sqlc.arg('user_id')::uuid,
    sqlc.arg('family_id')::uuid,
    sqlc.arg('token_hash'),
    sqlc.arg('role'),
    sqlc.arg('expires_at')::timestamptz
)
RETURNING *;

-- 按哈希取一条。刷新时先查它 —— 这是"这个 token 到底存不存在"的权威答案。
--
-- ★ 刻意不把 expires_at > now() 写进 WHERE：
-- 那样"过期"与"不存在"就都表现为 ErrNoRows，调用方无法区分，
-- 而这两者该走的处理完全不同（过期 → 让客户端重新登录即可；
-- 不存在 → 可能是重放，要吊销整个 family）。过期判断留给 Go 侧做，
-- 因为它需要把"过期"这个事实传出去，而 SQL 只能用一个空结果表达。
--
-- 也刻意不加 status 过滤：要拿到行才能知道它已经是 ROTATED/REVOKED，
-- 而"这行存在但已轮换"正是重放检测的输入。
-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens
WHERE token_hash = sqlc.arg('token_hash');

-- ★ 轮换的 CAS：只有仍处于 ACTIVE 的那一条能被消费。
--
-- 这条语句是整套设计的**权威裁决点**。Redis 的 Lua 能让并发下的
-- "消费+签发"变成一次原子操作，但它拦不住这种情况：
--   进程在 Redis 里消费成功、还没写库就挂了 → Redis 说"已消费"，
--   库里还是 ACTIVE。下一次同一个 token 进来时，Redis 说"已用过（重放）"，
--   而库里说"有效"。两个来源打架时以谁为准？
--
-- 答案是**以本条语句的结果为准**：Redis 只是加速层，它让常规路径不必
-- 每秒都去数据库抢行锁；而这条 UPDATE ... WHERE status='ACTIVE'
-- 配合 token_hash 上的唯一约束，在 Postgres 的行锁下是真正的一次性消费。
--
-- 影响行数为 0（ErrNoRows）意味着：不存在、已过期、已轮换、已吊销 —— 四种情况。
-- 调用方据此判定"这个 token 不能用"，再按需区分是重放（吊销 family）还是普通的无效。
--
-- 为什么 CAS 而不是"先 SELECT 再 UPDATE"：两个并发刷新都读到 ACTIVE，
-- 然后都去 UPDATE，后写的那个会把前一个的 rotated_at 覆盖掉，
-- 于是同一个 token 被消费了两次，签出两批有效的新 token。
-- 单条带条件的 UPDATE 由行锁把这件事挡在数据库内部。
-- name: ConsumeRefreshToken :one
UPDATE refresh_tokens
SET status     = 'ROTATED',
    rotated_at = now()
WHERE token_hash = sqlc.arg('token_hash')
  AND status = 'ACTIVE'
  AND expires_at > now()
RETURNING *;

-- 吊销一个 family 下的全部 token。
-- 场景：登出、检测到重放、账号被封。
--
-- 返回受影响行数不是为了给用户看，而是给调用方一个"我确实吊销了东西"的信号：
-- 返回 0 意味着这个 family 早就空了，调用方应据此判断"重放检测"这件事
-- 是否还成立（family 已空 = 攻击者拿的是早就用过的旧 token，
-- 此时**不该**再吊销任何东西，否则会误伤当前持有合法 token 的用户）。
-- name: RevokeRefreshFamily :one
WITH updated AS (
    UPDATE refresh_tokens
    SET status     = 'REVOKED',
        revoked_at = now()
    WHERE family_id = sqlc.arg('family_id')::uuid
      AND status <> 'REVOKED'
    RETURNING id
)
SELECT COUNT(*)::bigint FROM updated;

-- 按哈希吊销单条（登出时用；只知道 token，不知道 family 的兜底路径）。
-- name: RevokeRefreshToken :one
UPDATE refresh_tokens
SET status     = 'REVOKED',
    revoked_at = now()
WHERE token_hash = sqlc.arg('token_hash')
  AND status <> 'REVOKED'
RETURNING *;

-- 清理过期记录。
-- 刻意**不**靠 expires_at 上的索引做自动清理（那需要 pg_cron 之类的额外依赖），
-- 而是提供这条语句给运维手动/定时调用。理由：引入定时任务意味着
-- 多一个"它没在跑"的静默失效模式，而这张表很小（每个在线会话一行），
-- 即便半年不清理也就是几十万行 —— 不值得为它换一个新故障模式。
-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens WHERE expires_at < sqlc.arg('now')::timestamptz;

-- 某用户当前是否还有活跃会话。用于"封号时立刻断掉所有会话"这类判断，
-- 也给运营一个"这个账号还有几个设备在线"的入口。
-- name: CountActiveRefreshTokens :one
SELECT COUNT(*)::bigint
FROM refresh_tokens
WHERE user_id = sqlc.arg('user_id')::uuid
  AND status = 'ACTIVE'
  AND expires_at > now();
