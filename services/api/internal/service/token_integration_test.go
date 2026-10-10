package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// newTokenTestService 建一个同时连真实 PG 与 Redis 的 Service。
// refresh 轮换这条路两头都要用，所以两个都必须真。
func newTokenTestService(t *testing.T) (*service.Service, *pgxpool.Pool) {
	t.Helper()

	pool := newTestPool(t)

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("连不上 Redis(%s)，跳过：%v", addr, err)
	}

	svc := service.New(service.Deps{
		Q:            repo.New(pool),
		Pool:         pool,
		Redis:        rdb,
		Issuer:       auth.NewTokenIssuer("test-secret", time.Minute),
		RefreshStore: auth.NewRefreshStore(rdb, time.Hour),
		Wechat:       auth.MockExchanger{},
		Payment:      payment.NewMock(),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		PendingTTL:   15 * time.Minute,
		RefreshTTL:   time.Hour,
	})
	return svc, pool
}

// seedMerchantWithSession 建一个商户并完成一次登录，返回它的 refresh token。
func seedMerchantWithSession(t *testing.T, svc *service.Service, pool *pgxpool.Pool) (userID string, sess *service.Session) {
	t.Helper()
	phone := uniquePhone()
	registerCleanup(t, pool, phone)

	s, err := svc.RegisterMerchant(context.Background(), service.MerchantRegisterInput{
		Phone: phone, Password: "merchant-secret-1", ShopName: "轮换测试店",
	})
	if err != nil {
		t.Fatalf("准备商户失败: %v", err)
	}
	return s.User.ID, s
}

// ---------- 基本轮换 ----------

// 正常轮换：旧 token 变 ROTATED，新 token 变 ACTIVE，且能连续轮换。
func TestRefresh_RotatesAndChainWorks(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	s2, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	if s2.RefreshToken == sess.RefreshToken {
		t.Fatal("刷新后应换一个新的 refresh token，实际与旧 token 相同")
	}
	if s2.AccessToken == "" {
		t.Fatal("刷新应签发新的 access token")
	}
	if s2.User.ID != sess.User.ID {
		t.Fatalf("刷新后用户变了：%s -> %s", sess.User.ID, s2.User.ID)
	}

	// 新 token 还能继续刷新（链式轮换）
	if _, err := svc.Refresh(ctx, s2.RefreshToken); err != nil {
		t.Fatalf("用新 token 继续刷新失败: %v", err)
	}

	// 旧 token 在库里必须已是 ROTATED
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashToken(sess.RefreshToken)).Scan(&status); err != nil {
		t.Fatalf("读取旧 token 记录失败: %v", err)
	}
	if status != "ROTATED" {
		t.Fatalf("旧 token 状态应为 ROTATED，实际 %q —— 它还能被再用一次", status)
	}
}

// ★ 库里只存哈希，不存明文。
//
// 这是 refresh token 表的地基：拿到 refresh token 等同于拿到凭证。
// 数据库被读走（备份泄露、注入读表、日志里有人打了 select *）时，
// 存明文等于所有在线用户的会话同时失窃。
func TestRefresh_DatabaseStoresHashNotPlaintext(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM refresh_tokens WHERE user_id = $1`, sess.User.ID).Scan(&stored); err != nil {
		t.Fatalf("读取 token 记录失败: %v", err)
	}
	if stored == sess.RefreshToken {
		t.Fatal("库里存了 refresh token 明文 —— 库泄露即全部会话失窃")
	}
	if stored != auth.HashToken(sess.RefreshToken) {
		t.Fatalf("库里存的哈希与 HashToken() 的结果不一致：%q", stored)
	}
}

// 伪造的 token 必须被拒，且不得写入任何记录。
func TestRefresh_RejectsUnknownToken(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	before := countRefreshTokens(t, pool)
	_, err := svc.Refresh(ctx, "totally-made-up-token")
	if err == nil {
		t.Fatal("伪造 token 应被拒绝")
	}
	if after := countRefreshTokens(t, pool); after != before {
		t.Fatalf("拒绝伪造 token 却改了库：%d -> %d", before, after)
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("正常 token 应仍可用（伪造请求不应牵连合法会话）: %v", err)
	}
}

// ---------- 并发 ----------

// ★★ 核心断言：并发刷新同一个旧 token，成功次数必须**恰好为 1**。
//
// 这是整套 refresh 设计的牙齿。原来 GET+DEL 非原子时，两个并发请求
// 都能通过"token 存在"的校验并各自签发一个新的；用户在 App 里就会遇到
// "刚刷新成功，下一次刷新又说令牌无效"。
//
// 为什么用 20 个并发而不是 2 个：2 个并发时判定恰好卡在临界点上才有
// 机会复现，而概率随并发度迅速上升。20 个是"一次就撞上"的量级，
// 而代价只是几十毫秒。
//
// 失败的请求应当报"重放"（因为确实有第二个请求撞上了同一个 token）。
// 但**不要求**每个失败都是重放 —— 也可能是"令牌无效"。两种都算通过条件，
// 关键断言是成功恰好一次。
func TestRefresh_ConcurrentOnlyOneSucceeds(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	const n = 20
	var success atomic.Int64
	var reused atomic.Int64
	var other atomic.Int64
	var issuedMu sync.Mutex
	issued := make([]string, 0, n)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := svc.Refresh(ctx, sess.RefreshToken)
			switch {
			case err == nil:
				success.Add(1)
				issuedMu.Lock()
				issued = append(issued, s.RefreshToken)
				issuedMu.Unlock()
			case errors.Is(err, auth.ErrRefreshReused):
				reused.Add(1)
			default:
				other.Add(1)
				t.Logf("并发刷新失败（可接受）: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Fatalf("并发刷新同一个 token 成功了 %d 次，期望恰好 1 次。重放=%d 其他=%d",
			got, reused.Load(), other.Load())
	}
	// 成功的那些必须给出**互不相同**的新 token。
	// 两次成功签出同一个新 token 意味着"同一个凭证被发出去两次"。
	seen := make(map[string]bool, len(issued))
	for _, tok := range issued {
		if seen[tok] {
			t.Fatalf("并发刷新签出了重复的新 token %q —— 同一个凭证发出去两次", tok)
		}
		seen[tok] = true
	}
	if len(seen) != int(success.Load()) {
		t.Fatalf("成功次数 %d 但新 token 只有 %d 个 —— 有重复签发",
			success.Load(), len(seen))
	}
	t.Logf("并发 %d：成功 1，重放 %d，其他 %d", n, reused.Load(), other.Load())
}

// ★ 重放必须吊销整个会话家族。
//
// 这是"检测到重放"这个动作的全部价值：攻击者拿着一份泄漏的旧 token
// 反复尝试，而正当用户手里的新 token 也在同一个家族里 —— 不吊销的话，
// 攻击者每次失败但用户也被拖在"不知道该重登几次"的状态里。
func TestRefresh_ReplayRevokesWholeFamily(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	s2, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}

	// 重放那个已经消费过的旧 token。
	_, err = svc.Refresh(ctx, sess.RefreshToken)
	if !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("重放旧 token 应返回 ErrRefreshReused，实际: %v", err)
	}

	// 家族被吊销 ⇒ 当前有效的那个 token 也必须失效。
	_, err = svc.Refresh(ctx, s2.RefreshToken)
	if err == nil {
		t.Fatal("重放后同家族的另一个 token 仍能刷新 —— 家族没被吊销")
	}

	// 库里应能看到 REVOKED 记录。
	var revoked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND status = 'REVOKED'`,
		sess.User.ID).Scan(&revoked); err != nil {
		t.Fatalf("统计已吊销记录失败: %v", err)
	}
	if revoked == 0 {
		t.Fatal("库里没有 REVOKED 记录 —— 权威侧没有吊销")
	}
}

// ---------- Redis / DB 不一致 ----------

// ★ Redis 丢数据（重启 / flush）后，刷新必须**仍然可用**。
//
// 这是"Redis 只是加速层、Postgres 才是权威"的直接验证。
// 删掉 Redis 里的键之后，同一个 refresh token 还能刷新成功 ——
// 因为服务回数据库查到了 ACTIVE 行。
//
// 反过来（不查库、只信 Redis）的话，用户会被莫名登出，而日志里什么都没有。
func TestRefresh_SurvivesRedisDataLoss(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	// 模拟 Redis 重启：删掉这个用户相关的所有键。
	keys, err := svc.Redis.Keys(ctx, "refresh:*").Result()
	if err != nil {
		t.Fatalf("枚举 Redis 键失败: %v", err)
	}
	if len(keys) > 0 {
		if err := svc.Redis.Del(ctx, keys...).Err(); err != nil {
			t.Fatalf("清空 Redis 失败: %v", err)
		}
	}
	// 顺便把整个测试库的 refresh 缓存都清掉，避免残留影响其他用例。
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if ks, err := svc.Redis.Keys(c, "refresh:*").Result(); err == nil && len(ks) > 0 {
			_ = svc.Redis.Del(c, ks...).Err()
		}
	})

	s2, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatalf("Redis 数据丢失后刷新应仍成功（DB 是权威），实际: %v", err)
	}
	if s2.RefreshToken == "" {
		t.Fatal("应签发新的 refresh token")
	}
	// 再刷一次，验证它把缓存补回来了（第二次走快路径）。
	if _, err := svc.Refresh(ctx, s2.RefreshToken); err != nil {
		t.Fatalf("补缓存后再次刷新失败: %v", err)
	}
}

// Redis 里有一条"幽灵记录"（库里没有对应行）时，刷新必须失败。
//
// 这是反方向的测试：Redis 说了不算，库说了才算。
// 造法是直接往 Redis 里塞一条 —— 模拟"数据曾经不一致"或
// "有人手工往缓存里塞了东西"。
func TestRefresh_DatabaseOverridesRedisGhost(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	ghost := "ghost-token-" + uuid.NewString()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = svc.Redis.Del(c, "refresh:tok:"+auth.HashToken(ghost),
			"refresh:fam:"+sess.User.ID).Err()
	})
	// 让 Redis 侧看起来完全合法
	rec := auth.RefreshRecord{UserID: sess.User.ID, Role: "merchant", FamilyID: sess.User.ID}
	if err := svc.RefreshStore.CacheToken(ctx, ghost, rec); err != nil {
		t.Fatalf("写入幽灵缓存失败: %v", err)
	}

	_, err := svc.Refresh(ctx, ghost)
	if err == nil {
		t.Fatal("库里不存在的 token 必须被拒 —— Redis 不是权威")
	}
	// 幽灵记录不该在库里留下任何东西。
	var ghostRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashToken(ghost)).Scan(&ghostRows); err != nil {
		t.Fatalf("统计幽灵记录失败: %v", err)
	}
	if ghostRows != 0 {
		t.Fatalf("幽灵 token 被写进了库（%d 行）—— Redis 的说法压过了数据库", ghostRows)
	}
}

// Redis 不可用时刷新必须仍然可用（降级到纯 DB 路径）。
//
// 为什么要单独测：Rotate 报错时 RotateResult 是 RotateMiss，
// 而 miss 与"Redis 不知道"共用同一个值 —— 如果这个区分写错，
// Redis 挂掉会直接变成"所有刷新失败"，也就是全站登出。
func TestRefresh_FallsBackWhenRedisDown(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	// 登录成功后立刻把 Redis 换成不可达的地址。
	//
	// MaxRetries: 0 是必需的：go-redis 默认重试 3 次并指数退避，
	// 每次都等 DialTimeout —— 那会让这个用例从 0.1 秒变成好几秒。
	// 生产代码里**不能**这么设（重试对瞬时故障有用），
	// 但测试要的就是"立刻失败"这个语义。
	dead := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond,
		ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond,
		MaxRetries: -1, // go-redis: -1 表示不重试
	})
	t.Cleanup(func() { _ = dead.Close() })
	broken := service.New(service.Deps{
		Q:            repo.New(pool),
		Pool:         pool,
		Redis:        dead,
		Issuer:       auth.NewTokenIssuer("test-secret", time.Minute),
		RefreshStore: auth.NewRefreshStore(dead, time.Hour),
		Wechat:       auth.MockExchanger{},
		Payment:      payment.NewMock(),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		PendingTTL:   15 * time.Minute,
		RefreshTTL:   time.Hour,
	})

	s2, err := broken.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatalf("Redis 挂掉时刷新应降级到纯 DB 路径并成功，实际: %v", err)
	}
	if s2.RefreshToken == "" {
		t.Fatal("应签发新的 refresh token")
	}
	// 降级路径下再刷一次（此时 Redis 依旧不通）
	if _, err := broken.Refresh(ctx, s2.RefreshToken); err != nil {
		t.Fatalf("降级路径下第二次刷新失败: %v", err)
	}
}

// 已过期的 token 必须被拒，且**不吊销家族**。
//
// 不吊销是刻意的：过期不是攻击特征。同一设备上正常轮换出来的
// 其他 token 不该被一个过期的兄弟连累。
func TestRefresh_ExpiredTokenRejectedWithoutFamilyRevoke(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	// 把这条记录改成已过期。
	if _, err := pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 minute'
		 WHERE token_hash = $1`, auth.HashToken(sess.RefreshToken)); err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}

	_, err := svc.Refresh(ctx, sess.RefreshToken)
	if err == nil {
		t.Fatal("过期 token 应被拒")
	}

	var revoked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND status = 'REVOKED'`,
		sess.User.ID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 {
		t.Fatal("过期不应吊销家族 —— 那会误伤同设备的正常会话")
	}
}

// ---------- 登出 ----------

// 登出必须让整个家族在**数据库**里失效。
//
// 只删 Redis 是不够的：Redis 会过期、会被 flush，权威侧留着就等于没登出。
func TestLogout_RevokesInDatabase(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("登出失败: %v", err)
	}

	if _, err := svc.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Fatal("登出后该 token 仍能刷新")
	}

	var revoked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND status = 'REVOKED'`,
		sess.User.ID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked == 0 {
		t.Fatal("登出未在库里吊销 —— 权威侧还留着有效会话")
	}
}

// 登出是幂等的：重复登出不报错，未知 token 也不报错。
func TestLogout_Idempotent(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("首次登出失败: %v", err)
	}
	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("重复登出应成功（幂等），实际: %v", err)
	}
	if err := svc.Logout(ctx, "never-existed"); err != nil {
		t.Fatalf("登出未知 token 应成功（幂等），实际: %v", err)
	}
	if err := svc.Logout(ctx, ""); err != nil {
		t.Fatalf("空 token 登出应成功，实际: %v", err)
	}
}

// 登出一个不存在的 token 不得影响该用户的其他会话。
func TestLogout_UnknownTokenDoesNotRevokeOthers(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	s2, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if err := svc.Logout(ctx, "never-existed"); err != nil {
		t.Fatalf("登出未知 token 失败: %v", err)
	}
	if _, err := svc.Refresh(ctx, s2.RefreshToken); err != nil {
		t.Fatalf("登出未知 token 牵连了合法会话: %v", err)
	}
}

func countRefreshTokens(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens`).Scan(&n); err != nil {
		t.Fatalf("统计 refresh 记录失败: %v", err)
	}
	return n
}

// 家族成员数在轮换后必须保持为 1（而不是随轮换次数增长）。
//
// 这是"旧 token 从集合里被移除"的直接断言。不移除的话，
// 一次会话轮换 30 天会有 720 个成员，SREM 被漏掉的表现是
// 登出时 Redis 侧要删 720 个键而每次都超时。
func TestRefresh_FamilySetDoesNotGrow(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	tok := sess.RefreshToken
	for i := 0; i < 5; i++ {
		s, err := svc.Refresh(ctx, tok)
		if err != nil {
			t.Fatalf("第 %d 次刷新失败: %v", i, err)
		}
		tok = s.RefreshToken
	}

	var familyID string
	if err := pool.QueryRow(ctx,
		`SELECT family_id FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashToken(tok)).Scan(&familyID); err != nil {
		t.Fatalf("读取 family_id 失败: %v", err)
	}
	n, err := svc.RefreshStore.FamilySize(ctx, familyID)
	if err != nil {
		t.Fatalf("读取家族成员数失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("轮换 5 次后家族成员应为 1，实际 %d —— 旧 token 没被 SREM 移除", n)
	}
	fmt.Fprintf(os.Stderr, "family=%s size=%d\n", familyID, n)
}
