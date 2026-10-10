package service_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// depsFor 返回一份与 newTokenTestService 相同、但可覆盖个别字段的 Deps。
func depsFor(pool *pgxpool.Pool, rdb *redis.Client) service.Deps {
	return service.Deps{
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
	}
}

// deadRedisClient 指向一个必然连不上的地址。
//
// MaxRetries: -1 是必需的：go-redis 默认重试 3 次并指数退避，
// 每次都要等 DialTimeout —— 那会让并发用例从几百毫秒变成好几秒。
func deadRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	dead := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond,
		ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond,
		MaxRetries: -1, // go-redis: -1 表示不重试
	})
	t.Cleanup(func() { _ = dead.Close() })
	return dead
}

// ★ 权威裁决点（SQL CAS）的两条条件必须有牙 —— 必须**绕过** svc.Refresh 直接调 repo。
//
// 为什么不能走 svc.Refresh：service 的过期预检排在 CAS 之前，过期行在那里就被拒了，
// SQL 里的 `expires_at > now()` 永远不会被执行到，删掉它也不会有任何测试变红。
// 这正是 M20/M21 长期"未证明"的原因 —— 不是守卫没用，而是**没有任何测试走到它**。
//
// 三条断言互相独立：过期要拒、未过期要放行、已消费要拒。
// "未过期要放行"这一条不能省：否则把条件写成恒假（一律拒绝）也能让另外两条通过。
func TestConsumeRefreshToken_SQLGuards(t *testing.T) {
	svc, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, svc, pool)

	hash := auth.HashToken(sess.RefreshToken)
	q := repo.New(pool)

	// 1) 已过期：即使 status 仍是 ACTIVE 也必须拒绝。
	if _, err := pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 hour' WHERE token_hash = $1`,
		hash); err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}
	if _, err := q.ConsumeRefreshToken(ctx, hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("已过期的行仍被 CAS 消费了（err=%v）—— 令牌有效期形同虚设", err)
	}

	// 2) 恢复未过期 → 必须能消费。
	if _, err := pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() + interval '1 hour' WHERE token_hash = $1`,
		hash); err != nil {
		t.Fatalf("恢复过期时间失败: %v", err)
	}
	if _, err := q.ConsumeRefreshToken(ctx, hash); err != nil {
		t.Fatalf("ACTIVE 且未过期的行应能被 CAS 消费，实际: %v", err)
	}

	// 3) 已被消费（ROTATED）→ 再也不能消费。这是"同一凭证只能用一次"的落点。
	if _, err := q.ConsumeRefreshToken(ctx, hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("同一 token 被消费了两次（err=%v）—— 一个凭证被重复使用", err)
	}
}

// ★ 快路径失效时，DB 的 CAS 必须独立兜住并发。
//
// 与 TestRefresh_ConcurrentOnlyOneSucceeds 的区别：那条走的是 Redis 的原子消费，
// DB 的 `status = 'ACTIVE'` 条件根本没被用到 —— 删掉它测试照样全绿。
// 这里把 Redis 换成不可达地址，于是 Lua 那层整体失效，**只剩 DB CAS 兜底**：
// 20 个并发刷同一个 token，成功必须恰好 1 次。
func TestRefresh_DBGuard_ConcurrentOnlyOneSucceedsWithoutRedis(t *testing.T) {
	normal, pool := newTokenTestService(t)
	_, sess := seedMerchantWithSession(t, normal, pool)

	broken := service.New(depsFor(pool, deadRedisClient(t)))

	const n = 20
	var success atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := broken.Refresh(context.Background(), sess.RefreshToken); err == nil {
				success.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Fatalf("Redis 不可用时并发刷新成功了 %d 次，期望恰好 1 次 —— DB CAS 没兜住", got)
	}

	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashToken(sess.RefreshToken)).Scan(&status); err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if status != "ROTATED" {
		t.Fatalf("并发结束后该 token 状态应为 ROTATED，实际 %q", status)
	}
}

// ★ Redis 消费成功但 DB CAS 失败时，必须把 Redis 回滚（Compensate）。
//
// 不回滚会留下一条"库里查无此会话、却能换到 access token"的凭证 ——
// access token 的签发不查库，所以持有者能一直用，而库里查不到任何痕迹。
//
// # 怎么让这条不可达的路径变得可达
//
// 正常流程里过期预检排在前面，过期行根本走不到 CAS。这里注入一个**回拨的时钟**
// （Deps.Now），让预检以为"还没到期"，于是请求落到 CAS 上并因 SQL 的 now() 失败 ——
// 这正是需要 Compensate 的那个时刻。
//
// 断言的是 Redis 里那个重放标记（refresh:used:<hash>）被清掉：
// Rotate 成功时会 SET 它，Compensate 会 DEL 它。它还在 ⇒ 回滚没执行。
func TestRefresh_CompensatesWhenDBCasFails(t *testing.T) {
	normal, pool := newTokenTestService(t)
	ctx := context.Background()
	_, sess := seedMerchantWithSession(t, normal, pool)

	// 库里那行改成已过期（SQL 的 now() 会说"过期"，预检的 s.clock() 却被我们回拨了）。
	if _, err := pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 hour' WHERE token_hash = $1`,
		auth.HashToken(sess.RefreshToken)); err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}

	d := depsFor(pool, normal.Redis)
	d.Now = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
	skewed := service.New(d)

	if _, err := skewed.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Fatal("DB 行已过期，刷新必须失败")
	}

	usedKey := "refresh:used:" + auth.HashToken(sess.RefreshToken)
	n, err := normal.Redis.Exists(ctx, usedKey).Result()
	if err != nil {
		t.Fatalf("读取 Redis 键失败: %v", err)
	}
	if n != 0 {
		t.Fatal("Redis 的重放标记没被清掉 —— Compensate 没有执行，" +
			"Redis 与数据库已经不一致（库里查无此会话、Redis 里却留着可用凭证）")
	}
}

// ★ 非 ErrNoRows 的查询错误不能被伪装成"令牌不存在"。
//
// 把数据库故障当成用户凭证问题，后果是故障在监控上表现成
// "一堆用户拿着无效令牌"，运维会去查客户端与令牌，而不是查数据库。
//
// 用假 DBTX 注入而不是关掉连接池：关池会连带影响 t.Cleanup 里的清理语句，
// 让用例因为"清理失败"变红 —— 那种红是噪音，会教人学会忽略失败。
func TestRefresh_PropagatesNonRowError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	svc := service.New(service.Deps{
		// 这条路径在预检阶段就失败，所以 Pool / Redis 可以是 nil ——
		// "能为 nil" 本身就是"没有走到它们"的证据。
		Q:          repo.New(failingDBTX{err: boom}),
		Issuer:     auth.NewTokenIssuer("test-secret", time.Minute),
		Wechat:     auth.MockExchanger{},
		Payment:    payment.NewMock(),
		Logger:     slog.New(slog.DiscardHandler),
		RefreshTTL: time.Hour,
	})

	_, err := svc.Refresh(context.Background(), "some-token")
	if err == nil {
		t.Fatal("数据库查询失败时刷新必须报错")
	}
	if errors.Is(err, auth.ErrRefreshInvalid) {
		t.Fatal("非 ErrNoRows 的错误被当成了「令牌不存在」—— 真实故障被伪装成用户凭证问题")
	}
}

// failingDBTX 让所有数据库操作都返回同一个错误（模拟连接被重置之类的故障）。
type failingDBTX struct{ err error }

func (f failingDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.err
}

func (f failingDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, f.err
}

func (f failingDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return failingRow{f.err}
}

type failingRow struct{ err error }

func (r failingRow) Scan(...interface{}) error { return r.err }
