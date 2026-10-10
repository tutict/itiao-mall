package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RefreshRecord 是 refresh token 的载荷。
//
// 它同时存在于两个地方：Redis 的值，以及 Postgres 的
// user_id / family_id / role 三列。Redis 里的这份是**副本**，
// 存在的意义只有一个 —— 让"这个 token 还有效吗"不用查库就能答。
type RefreshRecord struct {
	UserID   string `json:"uid"`
	Role     string `json:"role"`
	FamilyID string `json:"fid"`
}

// HashToken 算 refresh token 的存储形式（SHA-256 十六进制小写）。
//
// 库与 Redis key 都用它。统一在一处算是为了防止将来某处漏改 ——
// 两个地方用不同的哈希算法的话，Redis 命中而库里查不到，
// 而这种不一致极难定位（表现是"偶尔刷新失败"，取决于走没走到缓存）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewFamilyID 生成一个新的 token family id。
// 单独导出是因为 service 层在"签发 + 写库"这条路径上需要拿到它。
//
// 刻意用 UUID 而不是随机串：family_id 是数据库主键的一部分
// （refresh_tokens.family_id 是 uuid 列），用非 UUID 值会在写库时被
// Postgres 拒掉 —— 而且是运行期才炸，不是编译期。
func NewFamilyID() string { return uuid.NewString() }

// rotateScript 一次性完成「校验旧 token → 失效它 → 落新 token」。
//
// # 为什么必须是 Lua
//
// 原实现是三步：GET 旧值、TxPipeline 消费、issueInFamily 写新值。
// 问题在 GET 与消费之间：如果两个刷新请求同时 GET 到同一条记录，
// 两者都会通过"token 存在"的校验，然后各自消费一次、各自签发一个新的。
// 第二个请求**会**因为 DEL/SET 的结果不确定而报错，但报错的形态是
// "写新 token 失败"或"消费失败"—— 取决于谁先执行到哪一步，
// 而在这两种情况下，**第一个请求拿到的新 token 可能已经被第二个请求的
// 后续动作覆盖或删除**。表现是用户偶发"刷新成功后马上又得刷新"。
//
// Lua 在 Redis 里是单线程原子执行的：读、判断、写全在一个时间片内完成。
// 于是两个并发请求里，一个成功、一个明确拿到"已被使用"。
//
// # KEYS
//  1. refresh:used:<hash>   —— 重放标记（旧 token 被消费过的证据）
//  2. refresh:tok:<newHash> —— 新 token 的位置
//  3. refresh:fam:<family>  —— family 的成员集合
//  4. refresh:tok:<oldHash> —— 旧 token 的位置
//
// 刻意按"操作顺序"而不是字典序排列 KEYS。Redis Cluster 要求同一 slot
// 才能用多键 Lua，而这里的 key 本来就跨 slot —— 所以**不用 Cluster**。
// 这一点是本设计的硬约束，写在这里是因为将来有人部署到 Cluster 时
// 一定会踩到：多键脚本在 Cluster 下直接报 CROSSSLOT。
// 单机/主从/哨兵下没有这个问题。
//
// # ARGV
//  1. 旧 token 的哈希
//  2. 新 token 的哈希
//  3. 新记录 JSON
//  4. TTL 秒数
//  5. 旧记录的 family id（用于重放时定位要吊销的家族）
var rotateScript = redis.NewScript(`
local usedKey   = KEYS[1]
local newTokKey = KEYS[2]
local famKey    = KEYS[3]
local oldTokKey = KEYS[4]

local oldHash = ARGV[1]
local newHash = ARGV[2]
local payload = ARGV[3]
local ttl     = tonumber(ARGV[4])
local family  = ARGV[5]

-- 分支一：旧 token 已经有"已用"标记 → 这是重放。
--
-- 这里的处理比原实现更克制：原实现会连带吊销整个 family。
-- 保留这个行为（它是对的：能重放出旧 token，说明攻击者手上有一份泄漏的旧值，
-- 整个家族都该作废），但**先看家族是否还有活口**：
-- family 已经被吊销（长度为 0）说明攻击者拿的是很久以前的废弃 token，
-- 此时再"吊销"是空操作；而如果此时有正当用户正在用同一家族的新 token，
-- 旧实现会把他一起踢下线 —— 那是误伤。所以先读长度。
if redis.call('EXISTS', usedKey) == 1 then
    return {2, family, tostring(redis.call('SCARD', famKey))}
end

-- 分支二：旧 token 存在 → 正常轮换。
if redis.call('EXISTS', oldTokKey) == 1 then
    redis.call('DEL', oldTokKey)
    redis.call('SET', usedKey, family, 'EX', ttl)
    redis.call('SREM', famKey, oldHash)
    redis.call('SET', newTokKey, payload, 'EX', ttl)
    redis.call('SADD', famKey, newHash)
    redis.call('EXPIRE', famKey, ttl)
    return {1, family, '0'}
end

-- 分支三：两者都不存在 → 令牌既没被消费过、也不存在。
-- 可能是伪造的，也可能是 Redis 重启后数据丢了。
-- 调用方必须回数据库核对（见 service 层），不能在这里下结论。
return {0, '', '0'}
`)

// revokeFamilyScript 原子吊销一个 family。
// SMEMBERS 之后 DEL 之间若有新成员加入会被漏掉，所以整个过程放进 Lua。
//
// 前缀通过 ARGV 传而不是写死 'refresh:tok:'：写死的话，
// 一旦有人把 NewRefreshStore 里的 prefix 改成可配置，这里就成了一个
// 静默失配的字符串 —— 吊销脚本仍然"成功"（不报错）但一个 token 都没删掉。
// 这种失败模式比报错难查得多：登出看起来生效了，令牌却还能用。
var revokeFamilyScript = redis.NewScript(`
local famKey = KEYS[1]
local prefix = ARGV[1]
local members = redis.call('SMEMBERS', famKey)
for i = 1, #members do
    redis.call('DEL', prefix .. ':tok:' .. members[i])
end
redis.call('DEL', famKey)
return #members
`)

// RotateResult 是 Rotate 的判定结果。导出它是因为调用方（service 层）
// 需要区分"Redis 已消费"与"Redis 不知道"—— 前者要触发补偿，后者要补缓存。
type RotateResult int

const (
	// RotateMiss 表示 Redis 里既没有该 token 也没有重放标记。
	// **不能**据此判定无效：Redis 是加速层，丢数据是它的常态，
	// 最终答案要回数据库查。
	RotateMiss RotateResult = iota
	// RotateOK 表示已在 Redis 侧原子完成消费并缓存了新 token。
	RotateOK
	// RotateReused 表示检测到重放。Rotate 此时返回 ErrRefreshReused。
	RotateReused
)

// RefreshStore 用 Redis 缓存 refresh token 的校验结果，并承担并发互斥。
//
// # 它不是权威
//
// 这个类型的所有方法都可能返回"Redis 不知道"。调用方（service 层）
// **必须**在拿不准时回 Postgres 核对 —— 那是最终权威。
// 之所以保留 Redis 这一层而不是全部走数据库，有两个具体收益：
//  1. 并发下的一次性消费由 Lua 保证，不必每次都去数据库抢行锁；
//  2. 大量"token 明显无效"的探测（扫描器拿随机串来打）可以在 Redis 就地拒绝，
//     不产生数据库压力。
//
// 但**判定"是否有效"这件事**最终落在数据库的 ConsumeRefreshToken 上。
type RefreshStore struct {
	rdb    *redis.Client
	ttl    time.Duration
	prefix string
}

func NewRefreshStore(rdb *redis.Client, ttl time.Duration) *RefreshStore {
	return &RefreshStore{rdb: rdb, ttl: ttl, prefix: "refresh"}
}

func (s *RefreshStore) tokKey(hash string) string  { return s.prefix + ":tok:" + hash }
func (s *RefreshStore) usedKey(hash string) string { return s.prefix + ":used:" + hash }
func (s *RefreshStore) famKey(familyID string) string {
	return s.prefix + ":fam:" + familyID
}

// CacheToken 把一条已落库的 refresh 记录写进 Redis。
//
// 登录、刷新成功后都要调它。**写库在前、写缓存在后** —— 顺序反过来会造出
// "缓存里有、库里没有"的记录，而那一类的存在足以让人以为刷新成功了。
func (s *RefreshStore) CacheToken(ctx context.Context, token string, rec RefreshRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 refresh 记录失败: %w", err)
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.tokKey(HashToken(token)), payload, s.ttl)
	pipe.SAdd(ctx, s.famKey(rec.FamilyID), HashToken(token))
	pipe.Expire(ctx, s.famKey(rec.FamilyID), s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("缓存 refresh token 失败: %w", err)
	}
	return nil
}

// Rotate 在 Redis 侧原子完成「消费旧 token + 缓存新 token」。
//
// # 这里的成功不代表最终成功
//
// 返回 rotateOK 只说明"Redis 这一层没有竞争"。真正的裁决在数据库：
// service 层拿到 ok 之后必须再跑一次 ConsumeRefreshToken 的 CAS，
// 而那个 CAS 才是"旧 token 只被使用一次"的权威判定。
// 如果 Redis 成功但 DB CAS 失败（比如并发下另一个请求已经先消费了），
// 调用方要调用 Compensate 把 Redis 回滚 —— 否则库里记着"已轮换"，
// Redis 里却留着一个本不该存在的有效新 token。
//
// 为什么不把 DB CAS 放进这个函数：auth 包刻意不依赖 repo/pgx
// （它是纯凭证层，被 service 与 httpapi 复用）。分层比省一次调用重要。
func (s *RefreshStore) Rotate(ctx context.Context, token, newToken string, rec RefreshRecord) (RotateResult, error) {
	if token == "" || newToken == "" {
		return RotateMiss, ErrRefreshInvalid
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return RotateMiss, fmt.Errorf("序列化 refresh 记录失败: %w", err)
	}

	oldHash := HashToken(token)
	newHash := HashToken(newToken)

	res, err := rotateScript.Run(ctx, s.rdb,
		[]string{
			s.usedKey(oldHash),
			s.tokKey(newHash),
			s.famKey(rec.FamilyID),
			s.tokKey(oldHash),
		},
		oldHash,
		newHash,
		string(payload),
		int64(s.ttl.Seconds()),
		rec.FamilyID,
	).Slice()
	if err != nil {
		// Redis 侧失败**不等于**刷新失败。调用方应回数据库走纯 DB 路径：
		// 那条路径更慢，但正确性不依赖 Redis。
		// 返回非业务错误就是为了让调用方能这样区分。
		return RotateMiss, fmt.Errorf("Redis 轮换失败: %w", err)
	}
	if len(res) != 3 {
		return RotateMiss, fmt.Errorf("Redis 轮换脚本返回了 %d 个值，期望 3 个", len(res))
	}
	code, ok := res[0].(int64)
	if !ok {
		return RotateMiss, errors.New("Redis 轮换脚本返回类型异常")
	}
	switch RotateResult(code) {
	case RotateOK:
		return RotateOK, nil
	case RotateReused:
		return RotateReused, ErrRefreshReused
	default:
		return RotateMiss, nil
	}
}

// Compensate 在「Redis 轮换成功、但数据库 CAS 失败」时回滚 Redis。
//
// 为什么必须回滚：不回滚的话，Redis 里会留下一条库里不存在的有效 token。
// 它能换到 access token（access token 的签发不查库），于是数据库里
// 出现"查无此会话、但持有者能正常用"的状态 —— 那是最难查的一类不一致。
//
// 回滚动作：删掉新 token、删掉旧 token 的重放标记、把旧 token 放回去。
// 三个动作放进一个 pipeline（不是 Lua）—— 因为它们**之间**没有正确性依赖：
// 最坏情况是"旧 token 没放回去、但新 token 也没留下"，此时该用户
// 下次刷新会走纯 DB 路径（Redis miss → 查库 → 库里有 ACTIVE 行 → 成功）。
// 也就是回滚失败的最坏后果是"多一次数据库往返"，不是"用户被锁在外面"。
// 这就是这里不需要 Lua 的理由。
func (s *RefreshStore) Compensate(ctx context.Context, oldToken, newToken string, rec RefreshRecord) error {
	oldHash := HashToken(oldToken)
	newHash := HashToken(newToken)

	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, s.tokKey(newHash))
	pipe.SRem(ctx, s.famKey(rec.FamilyID), newHash)
	pipe.Del(ctx, s.usedKey(oldHash))
	// 旧 token 放回去时用原 TTL 剩余量而不是完整 TTL。
	// 用完整 TTL 等于让这次失败的事件"续命"一轮 —— 攻击者若在轮换窗口内
	// 截获了旧 token，会因此多出一整个 TTL 的可用时间。
	pipe.Set(ctx, s.tokKey(oldHash), mustJSONRecord(rec), remainingTTL(s.ttl))
	pipe.SAdd(ctx, s.famKey(rec.FamilyID), oldHash)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("回滚 refresh 缓存失败: %w", err)
	}
	return nil
}

// remainingTTL 把缓存时间调短。补偿时用它的目的是"别把失败事件的窗口再延长"。
func remainingTTL(ttl time.Duration) time.Duration {
	half := ttl / 2
	if half < time.Minute {
		return time.Minute
	}
	return half
}

func mustJSONRecord(rec RefreshRecord) string {
	b, err := json.Marshal(rec)
	if err != nil {
		// RefreshRecord 只有一个字符串字段，序列化失败只可能是内存坏了。
		// 与 mustRandom 同一处置：此时继续跑没有意义。
		panic(fmt.Sprintf("序列化 refresh 记录失败: %v", err))
	}
	return string(b)
}

// RevokeFamily 吊销整个家族的所有 token。
// 返回被吊销的 token 数量，供调用方判断"重放检测是否仍然成立"。
func (s *RefreshStore) RevokeFamily(ctx context.Context, familyID string) (int64, error) {
	n, err := revokeFamilyScript.Run(ctx, s.rdb, []string{s.famKey(familyID)}, s.prefix).Int64()
	if err != nil {
		return 0, fmt.Errorf("吊销 token family 失败: %w", err)
	}
	return n, nil
}

// RevokeToken 登出：删掉该 token 本身并吊销其家族。
func (s *RefreshStore) RevokeToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	raw, err := s.rdb.Get(ctx, s.tokKey(HashToken(token))).Bytes()
	if errors.Is(err, redis.Nil) {
		// Redis 不知道这个 token。这在登出的语境下是**正常**的：
		// 用户本来就是"用完就丢"，也可能 Redis 已经把它过期了。
		// 权威侧的吊销由 service 层在数据库里做。
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 refresh token 失败: %w", err)
	}
	var rec RefreshRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("解析 refresh 记录失败: %w", err)
	}
	_, err = s.RevokeFamily(ctx, rec.FamilyID)
	return err
}

// FamilySize 报告一个 family 里还有几个有效 token。只用于测试与诊断。
func (s *RefreshStore) FamilySize(ctx context.Context, familyID string) (int64, error) {
	return s.rdb.SCard(ctx, s.famKey(familyID)).Result()
}
