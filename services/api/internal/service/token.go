package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// tokenIOTimeout 是整条 token 流程的墙钟上限。
//
// 它必须存在：内部所有数据库与 Redis 访问都挂在这个 ctx 下。
// 没有它，一个卡住的下游（Redis 主从切换、连接池耗尽）会把 HTTP 请求
// 一直挂到网关超时 —— 表现为"服务变慢"而不是"服务报错"，
// 而后者是更容易被发现的故障。
const tokenIOTimeout = 5 * time.Second

// refresh token 记录的状态。与 0003 迁移的 refresh_tokens_status_check 一一对应。
//
// 为什么在这里声明常量而不用裸字符串：这些字面量会出现在判断里，
// 写错一个字母的后果是**静默走错分支**（比如把 REVOKED 写成 REVOKE，
// 于是已登出的会话被当成有效继续轮换）。而 sqlc 不会为 text 类型的
// CHECK 约束生成枚举常量 —— 也就是说数据库那层拦不住你，这里是唯一的防线。
const (
	tokenStatusActive  = "ACTIVE"
	tokenStatusRotated = "ROTATED"
	tokenStatusRevoked = "REVOKED"
)

// newOpaqueToken 生成一个不透明 refresh token。
//
// 抽成变量是为了可测试：并发测试需要构造"两个请求持有同一个 token"，
// 而真实实现依赖 crypto/rand，靠运气是撞不出来的。
var newOpaqueToken = func() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 refresh token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// issueSession 并发完成「签 access token」与「落 refresh token」。
//
// 两件事互不依赖：access token 的签发是纯计算（一次 HMAC），
// refresh token 的落库要一次数据库往返 + 一次 Redis 写。
// 串行做等于把两者的耗时相加，而它们之间没有任何数据依赖。
//
// # 并发度为什么固定为 2
//
// 这个函数里只有两个可并行的分支，errgroup.SetLimit(2) 是精确上界。
// 不用"可配置"的理由：签发流程内部有几步可并行是**代码结构**决定的，
// 不是运维决定的。把上限做成配置等于给了一个"调错了没人发现"的旋钮 ——
// 而真正的并发度调节（同时处理多少请求）属于 HTTP 层，由服务器与限流负责。
//
// # 为什么用 errgroup 而不是裸 WaitGroup + sync.Map
//
//  1. 它在第一个错误出现时**取消**共享 ctx，于是另一个分支的数据库往返
//     能被中断而不是跑完 —— 失败路径的延迟从"两段之和"降到"最慢的一段"。
//  2. 它自带结果写入的互斥保护。裸 WaitGroup + 手写锁容易漏锁，
//     而漏锁的表现是 race detector 偶发报错、线上偶发读到零值。
//  3. 错误按 errors.Join 语义收集：两个分支都失败时两个原因都在链上，
//     而不是只看到先返回的那个。
//
// # 超时与取消的方向
//
// errgroup.WithContext 派生的 ctx 取消**不会**向上传播到父 ctx，
// 所以父 ctx（带 tokenIOTimeout）是我们唯一的硬边界 —— 这正是想要的：
// 内部某一步出错不该拖垮整个 HTTP 请求。
func (s *Service) issueSession(ctx context.Context, user repo.User) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenIOTimeout)
	defer cancel()

	refreshToken, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	familyID := auth.NewFamilyID()
	hash := auth.HashToken(refreshToken)
	rec := auth.RefreshRecord{UserID: user.ID, Role: user.Role, FamilyID: familyID}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(2)

	var access string
	var accessExp time.Time

	g.Go(func() error {
		tok, exp, err := s.Issuer.IssueAccess(user.ID, user.Role)
		if err != nil {
			return fmt.Errorf("签发 access token 失败: %w", err)
		}
		access, accessExp = tok, exp
		return nil
	})

	g.Go(func() error {
		// 写库在前、写缓存在后。顺序反过来会造出"缓存里有、库里没有"的记录，
		// 而那种记录足以让刷新"看起来成功"，实际上查无此会话。
		if _, err := s.Q.InsertRefreshToken(gctx, repo.InsertRefreshTokenParams{
			UserID:    user.ID,
			FamilyID:  familyID,
			TokenHash: hash,
			Role:      user.Role,
			ExpiresAt: pgtypeTimestamptz(time.Now().Add(s.RefreshTTL)),
		}); err != nil {
			return fmt.Errorf("写入 refresh 记录失败: %w", err)
		}
		if err := s.RefreshStore.CacheToken(gctx, refreshToken, rec); err != nil {
			// 缓存写失败**不**回滚数据库记录。理由：数据库才是权威，
			// 缓存失败的最坏后果是"这次刷新下次走纯 DB 路径"（Redis miss
			// → 查库 → 库里是 ACTIVE → 成功）。反过来把库里的记录删掉，
			// 等于把一次可恢复的缓存故障升级成"用户必须重新登录"。
			s.Logger().Warn("refresh token 缓存写入失败，本次登录降级为纯数据库路径",
				"user_id", user.ID, "err", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		// 一个分支失败时另一个已被 errgroup 取消。
		// 注意：refresh 记录可能已经落库了（access 签发失败的那条路径）。
		// 这不是泄漏 —— 那个 token 没有返回给任何人，
		// 攻击者拿不到它的明文，用不了。它会在 TTL 到期后自然消失。
		return nil, err
	}

	return &Session{
		AccessToken:  access,
		RefreshToken: refreshToken,
		ExpiresAt:    accessExp,
		User:         user,
	}, nil
}

// Refresh 轮换 refresh token 并签发新的 access token。
//
// # 权威与加速的分工
//
// Postgres 是最终权威；Redis 是加速层 + 并发互斥层。执行顺序刻意是
// 「先查库 → 再原子消费 → 最后签发」：
//
//  1. **先查库**（GetRefreshTokenByHash）确认权威侧的状态。
//     必须先做：Redis 可能刚重启、可能被 flush，
//     而"库里说它有效"要先于"缓存里说它有效"。
//  2. **再原子消费**。两条路径：
//     - 快路径：Redis Lua 原子消费，随后 DB CAS 确认。
//     Lua 保证并发下只有一个请求能消费成功，正常情况下 CAS 必然命中。
//     - 降级路径：Redis 不可用或返回 miss → 直接用 DB CAS。
//     `UPDATE ... WHERE status='ACTIVE'` 配合唯一索引，
//     在 Postgres 的行锁下同样是原子的。慢一点，正确性不依赖 Redis。
//  3. **最后签发** access token（并发做取用户与 HMAC 签名）。
//
// # Redis 与 DB 不一致时的处置
//
// | Redis        | DB          | 处置                                     |
// |--------------|-------------|------------------------------------------|
// | 消费成功      | CAS 命中    | 正常返回                                 |
// | 消费成功      | CAS 未命中  | 补偿回滚 Redis，返回"令牌已被使用"       |
// | miss         | CAS 命中    | 正常返回，并补写缓存让下次走快路径       |
// | miss         | CAS 未命中  | 无效令牌（过期/伪造/已吊销）             |
// | 报重放        | —           | 吊销整个 family（Redis 与 DB 都做）      |
//
// 第二行是本函数最关键的一行：Redis 已经消费成功而 DB 说不行时，
// 必须在 Redis 里把这次消费**撤销**，否则会留下一条"库里查无此会话、
// 却能换到 access token"的凭证 —— 那是最难查的一类不一致。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	if refreshToken == "" {
		return nil, auth.ErrRefreshInvalid
	}

	ctx, cancel := context.WithTimeout(ctx, tokenIOTimeout)
	defer cancel()

	hash := auth.HashToken(refreshToken)

	// ---- 步骤 1：权威侧核对 ----
	existing, err := s.Q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 库里都没有：伪造的，或已过期。两者都归为"无效"。
			// 刻意**不**吊销 family：我们连它属于哪个 family 都不知道
			//（查不到行），贸然吊销等于把一个持有合法旧 token 的用户踢下线。
			return nil, auth.ErrRefreshInvalid
		}
		return nil, fmt.Errorf("查询 refresh 记录失败: %w", err)
	}
	if existing.Status == tokenStatusRevoked {
		// 已吊销（登出过 / 封号清理过）。这是用户自己的正常操作，不吊销 family。
		return nil, auth.ErrRefreshInvalid
	}
	if !existing.ExpiresAt.Valid || !existing.ExpiresAt.Time.After(s.clock()) {
		// 过期不是攻击特征，所以也不吊销 family ——
		// 同一设备的正常会话会被误伤，而攻击者本来也拿不到新 token。
		//
		// 注意这只是一道预检。真正的兜底是下面 SQL CAS 里的 `expires_at > now()`：
		// 预检与 CAS 之间还有一次 Redis 往返，行可能在那段时间里过期。
		// 两道都要有 —— 也正因此，SQL 那层需要单独的测试才可能被验证到
		//（见 TestConsumeRefreshToken_SQLGuards 与 Deps.Now 的注释）。
		return nil, auth.ErrRefreshInvalid
	}

	familyID := existing.FamilyID
	rec := auth.RefreshRecord{UserID: existing.UserID, Role: existing.Role, FamilyID: familyID}

	newToken, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}

	// ---- 步骤 2a：Redis 原子消费（快路径尝试）----
	rotateRes, rotateErr := s.RefreshStore.Rotate(ctx, refreshToken, newToken, rec)
	switch {
	case errors.Is(rotateErr, auth.ErrRefreshReused):
		return nil, s.handleReuse(ctx, existing, "redis")
	case rotateErr != nil:
		// Redis 侧故障 → 降级到纯 DB 路径。这是有意的可用性取舍：
		// 正确性由 DB 的 CAS 保证，Redis 只影响延迟。
		s.Logger().Warn("Redis 轮换失败，降级为纯数据库路径",
			"user_id", existing.UserID, "err", rotateErr)
	}
	redisConsumed := rotateRes == auth.RotateOK

	// ---- 步骤 2b：DB CAS（权威裁决）----
	consumed, err := s.Q.ConsumeRefreshToken(ctx, hash)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("消费 refresh 记录失败: %w", err)
		}
		// CAS 未命中，两种成因，处理不同。
		if existing.Status == tokenStatusRotated {
			// 库里已是 ROTATED → 有人抢先消费了，这就是重放。
			return nil, s.handleReuse(ctx, existing, "db")
		}
		// 库里还是 ACTIVE 但 CAS 没命中：只可能是并发窗口内状态刚被改。
		// 而 Redis 若已消费成功，这里就是"Redis 领先于 DB"，必须回滚。
		if redisConsumed {
			if rerr := s.RefreshStore.Compensate(ctx, refreshToken, newToken, rec); rerr != nil {
				// 回滚也失败：Redis 与 DB 已不一致且无法修复。
				// 此时**必须**明确失败而不是继续 —— 继续等于签发一个
				// 数据库里查无此会话的凭证。
				return nil, fmt.Errorf("refresh 轮换不一致且回滚失败: %w", rerr)
			}
		}
		return nil, auth.ErrRefreshInvalid
	}

	// ---- 步骤 2c：落新记录 ----
	newHash := auth.HashToken(newToken)
	if _, err := s.Q.InsertRefreshToken(ctx, repo.InsertRefreshTokenParams{
		UserID:    consumed.UserID,
		FamilyID:  familyID,
		TokenHash: newHash,
		Role:      consumed.Role,
		ExpiresAt: pgtypeTimestamptz(time.Now().Add(s.RefreshTTL)),
	}); err != nil {
		// 新记录没落库 → 刚签的 token 是"查无此会话"的凭证，必须作废。
		//
		// 补偿顺序刻意是「先删新记录，再把旧记录恢复成 ACTIVE」：
		// 反过来的话中途失败会留下"旧 token 已消费、新 token 不存在"，
		// 用户直接登出；正序的最坏情况是"两个 token 都不可用"，
		// 重新登录一次即可恢复。两相比较，偏向"用户多登录一次"。
		if _, rerr := s.Q.RevokeRefreshToken(ctx, newHash); rerr != nil && !errors.Is(rerr, pgx.ErrNoRows) {
			s.Logger().Error("新 refresh 记录写入失败后吊销新 token 也失败",
				"user_id", consumed.UserID, "err", rerr)
		}
		if rerr := s.RefreshStore.RevokeToken(ctx, newToken); rerr != nil {
			s.Logger().Error("吊销 Redis 里的新 token 失败", "err", rerr)
		}
		if rerr := s.RefreshStore.Compensate(ctx, refreshToken, newToken, rec); rerr != nil {
			s.Logger().Error("新 refresh 记录写入失败后回滚旧 token 也失败", "err", rerr)
		}
		return nil, fmt.Errorf("写入新 refresh 记录失败: %w", err)
	}

	// ---- 步骤 2d：补缓存 ----
	// 快路径下 Rotate 已经写过；降级路径下这里补上，让下次刷新回到快路径。
	// 失败只记日志（理由见 issueSession）。
	if !redisConsumed {
		if err := s.RefreshStore.CacheToken(ctx, newToken, rec); err != nil {
			s.Logger().Warn("refresh token 缓存写入失败，下次刷新继续走慢路径",
				"user_id", consumed.UserID, "err", err)
		}
	}

	// ---- 步骤 3：并发取用户 + 签 access token ----
	return s.finishRefresh(ctx, consumed.UserID, consumed.Role, newToken)
}

// finishRefresh 并发「取用户」与「签 access token」，然后组装响应。
//
// 无依赖的关键：userID 与 role 在 CAS 那一步就都拿到了，
// 所以签 access token 那一支不必等用户查询返回 —— 这才是真并行。
//
// role 刻意用 refresh 记录里带的那份而**不**回查 users.role。
// 这看着只是省一次查询，实际有更重要的理由：access token 里的 role 是
// **签发那一刻的快照**。若回查 users.role，那么"降级"与"提权"两种变更
// 会因为 refresh token 是谁签的而表现不同。用 refresh 记录里的 role，
// 两个场景行为一致，语义也清楚：**role 在登录时确定，变更需重新登录生效**。
//
// 这与"商户审核状态每次查库"不矛盾：审核状态是**随时可变的经营资格**，
// 必须在每次操作前确认；role 是**身份**，本来就该在登录时固定。
func (s *Service) finishRefresh(ctx context.Context, userID, role, newToken string) (*Session, error) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(2)

	var user repo.User
	var access string
	var accessExp time.Time

	g.Go(func() error {
		u, err := s.Q.GetUserByID(gctx, userID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 会话还在但用户没了（被删号）。返回哨兵错误而不是
				// 原始 pgx 错误，否则会掉进 mapError 的 default 变成 500。
				return domain.ErrNotFound
			}
			return fmt.Errorf("查询用户失败: %w", err)
		}
		user = u
		return nil
	})

	g.Go(func() error {
		tok, exp, err := s.Issuer.IssueAccess(userID, role)
		if err != nil {
			return fmt.Errorf("签发 access token 失败: %w", err)
		}
		access, accessExp = tok, exp
		return nil
	})

	if err := g.Wait(); err != nil {
		// 新 refresh 记录已落库，但这次刷新返回失败。刻意**不**回滚：
		// 那个 token 客户端没拿到（响应没发出去），哈希无人知晓，无法被使用。
		// 反过来删库里的记录会引入更麻烦的状态：客户端可能因为网络原因
		// **收到了**响应却没拿到 body（多数 HTTP 客户端会自动重试），
		// 此时删记录等于让一个合法会话凭空失效。
		return nil, err
	}

	if err := ensureActive(user); err != nil {
		// 账号被停用。刻意**不**吊销 refresh 记录：封号是 users.status 的事，
		// 与会话是两件事。吊掉会话会让"解封后要重新登录"变成
		// "解封了但客户端还在自动重试刷新" —— 后者在 access token 到期后
		// 会自然走到 401，行为一样但更平滑。
		return nil, err
	}

	return &Session{
		AccessToken:  access,
		RefreshToken: newToken,
		ExpiresAt:    accessExp,
		User:         user,
	}, nil
}

// handleReuse 处理重放：吊销整个 family（数据库与 Redis 都做）。
//
// detectedBy 只进日志：排查重放时需要知道是 Redis 先发现还是数据库先发现，
// 两者的时间差能反映攻击发生在哪一层（Redis 侧说明攻击者打的是缓存键，
// 数据库侧说明他打的是完整链路）。
func (s *Service) handleReuse(ctx context.Context, tok repo.RefreshToken, detectedBy string) error {
	s.Logger().Warn("检测到 refresh token 重放，吊销整个会话家族",
		"user_id", tok.UserID,
		"family_id", tok.FamilyID,
		"detected_by", detectedBy,
	)
	if _, err := s.Q.RevokeRefreshFamily(ctx, tok.FamilyID); err != nil {
		// 数据库侧吊销失败：这是安全问题（该家族本应失效），
		// 必须让请求失败并留下日志，不能静默继续。
		return fmt.Errorf("重放后吊销会话家族失败: %w", err)
	}
	if _, err := s.RefreshStore.RevokeFamily(ctx, tok.FamilyID); err != nil {
		// Redis 侧失败只记日志：数据库已吊销，权威侧已经安全。
		// Redis 里的残留在 TTL 到期后消失，且下次校验会走数据库。
		s.Logger().Error("重放后吊销 Redis 缓存失败（数据库已吊销，不影响安全）", "err", err)
	}
	return auth.ErrRefreshReused
}
