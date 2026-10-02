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
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/platform/postgres"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// newTestPool 打开一个连到 DATABASE_URL 的连接池；未配置则跳过用例。
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 DATABASE_URL，跳过需要数据库的集成测试")
	}

	pool, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestService 建一个接真实 PG 的 Service。Redis 客户端是惰性连接的，
// 而本文件测的路径（下单/库存）不碰 Redis，所以不需要 Redis 实际在线。
func newTestService(t *testing.T) (*service.Service, *pgxpool.Pool) {
	t.Helper()

	pool := newTestPool(t)

	svc := service.New(service.Deps{
		Q:            repo.New(pool),
		Pool:         pool,
		Redis:        redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), // 不连接，仅占位
		Issuer:       auth.NewTokenIssuer("test-secret", time.Minute),
		RefreshStore: auth.NewRefreshStore(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}), time.Hour),
		Wechat:       auth.MockExchanger{},
		Payment:      payment.NewMock(),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		PendingTTL:   15 * time.Minute,
	})
	return svc, pool
}

// seedStockFixture 建一个商户、一个用户、一个商品和一个指定库存的 SKU。
func seedStockFixture(t *testing.T, pool *pgxpool.Pool, stock int32) (skuID, userID string) {
	t.Helper()
	ctx := context.Background()

	merchantID := uuid.NewString()
	userID = uuid.NewString()
	productID := uuid.NewString()
	skuID = uuid.NewString()

	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("准备测试数据失败: %v\nSQL: %s", err, sql)
		}
	}

	exec(`INSERT INTO users (id, role, phone, password_hash, nickname)
	      VALUES ($1, 'merchant', $2, 'x', '压测守卫-商户')`,
		merchantID, "139"+merchantID[:8])
	exec(`INSERT INTO users (id, role, wechat_openid, nickname)
	      VALUES ($1, 'user', $2, '压测守卫-用户')`,
		userID, "guard_openid_"+userID[:8])
	exec(`INSERT INTO products (id, merchant_id, title, status)
	      VALUES ($1, $2, '并发守卫商品', 'ON_SALE')`,
		productID, merchantID)
	exec(`INSERT INTO skus (id, product_id, name, price_cents, stock)
	      VALUES ($1, $2, '守卫规格', 1234, $3)`,
		skuID, productID, stock)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// 顺序无关（外键级联已覆盖），显式删干净避免污染后续跑
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM order_items WHERE sku_id = $1`, skuID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM orders WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM skus WHERE id = $1`, skuID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM products WHERE id = $1`, productID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = ANY($1::uuid[])`,
			[]string{merchantID, userID})
	})

	return skuID, userID
}

func currentStock(t *testing.T, pool *pgxpool.Pool, skuID string) int32 {
	t.Helper()
	var stock int32
	if err := pool.QueryRow(context.Background(),
		`SELECT stock FROM skus WHERE id = $1`, skuID).Scan(&stock); err != nil {
		t.Fatalf("读取库存失败: %v", err)
	}
	return stock
}

// ★ 本文件的核心守卫：并发下单绝不超卖。
//
// 断言三条，互相独立：
//  1. 成功数**恰好**等于初始库存（少一个说明误判库存不足，多一个说明超卖）；
//  2. 最终库存恰好为 0（不为负）；
//  3. 失败的那些必须都是 ErrInsufficientStock，不能是别的错被静默吞掉。
func TestCreateOrder_NoOversellUnderConcurrency(t *testing.T) {
	const initialStock = 100
	const attackers = 300

	svc, pool := newTestService(t)
	skuID, userID := seedStockFixture(t, pool, initialStock)

	var (
		succeeded  atomic.Int64
		insuffient atomic.Int64
		otherErr   atomic.Int64
		otherMsg   atomic.Value
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attackers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时开跑，制造真实竞争
			_, err := svc.CreateOrder(context.Background(), userID, "", []service.OrderLineInput{
				{SkuID: skuID, Qty: 1},
			})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, domain.ErrInsufficientStock):
				insuffient.Add(1)
			default:
				otherErr.Add(1)
				otherMsg.Store(err.Error())
			}
		}()
	}
	close(start)
	wg.Wait()

	finalStock := currentStock(t, pool, skuID)

	if got := succeeded.Load(); got != initialStock {
		t.Errorf("成功下单数 = %d，期望恰好 %d（多了=超卖，少了=误判库存不足，两者都是 bug）",
			got, initialStock)
	}
	if finalStock != 0 {
		t.Errorf("最终库存 = %d，期望 0（负数即超卖）", finalStock)
	}
	if got := insuffient.Load(); got != attackers-initialStock {
		t.Errorf("库存不足失败数 = %d，期望 %d", got, attackers-initialStock)
	}
	if got := otherErr.Load(); got != 0 {
		t.Errorf("出现了 %d 个非库存原因的失败（消息: %v）—— 这些错被错误分类了", got, otherMsg.Load())
	}

	t.Logf("并发 %d 次抢 %d 库存 → 成功 %d，库存不足 %d，最终库存 %d",
		attackers, initialStock, succeeded.Load(), insuffient.Load(), finalStock)
}

// 库存为 0 时下单必须干净地失败，且不留下订单痕迹。
func TestCreateOrder_ZeroStockRejects(t *testing.T) {
	svc, pool := newTestService(t)
	skuID, userID := seedStockFixture(t, pool, 0)

	_, err := svc.CreateOrder(context.Background(), userID, "", []service.OrderLineInput{
		{SkuID: skuID, Qty: 1},
	})
	if !errors.Is(err, domain.ErrInsufficientStock) {
		t.Fatalf("零库存下单应返回 ErrInsufficientStock，实际: %v", err)
	}

	var orderCount int
	if qerr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders WHERE user_id = $1`, userID).Scan(&orderCount); qerr != nil {
		t.Fatalf("统计订单失败: %v", qerr)
	}
	if orderCount != 0 {
		t.Errorf("下单失败却留下了 %d 条订单记录 —— 事务没回滚干净", orderCount)
	}
	if stock := currentStock(t, pool, skuID); stock != 0 {
		t.Errorf("失败的下单不该改动库存，实际 stock=%d", stock)
	}
}

// 同一 Idempotency-Key 并发重复提交，只能生成一单。
func TestCreateOrder_IdempotencyKeyUnderConcurrency(t *testing.T) {
	const concurrency = 20

	svc, pool := newTestService(t)
	skuID, userID := seedStockFixture(t, pool, 1000)
	key := "guard-idem-" + uuid.NewString()

	var wg sync.WaitGroup
	orderNos := make([]string, concurrency)
	errs := make([]error, concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			view, err := svc.CreateOrder(context.Background(), userID, key, []service.OrderLineInput{
				{SkuID: skuID, Qty: 1},
			})
			if err != nil {
				errs[idx] = err
				return
			}
			orderNos[idx] = view.Order.OrderNo
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[string]struct{}{}
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发请求返回错误: %v", i, err)
			continue
		}
		seen[orderNos[i]] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("同一幂等键并发生成了 %d 个不同订单，期望 1 个: %v", len(seen), seen)
	}

	var orderCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM orders WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key).Scan(&orderCount); err != nil {
		t.Fatalf("统计订单失败: %v", err)
	}
	if orderCount != 1 {
		t.Errorf("库中该幂等键有 %d 单，期望 1 单", orderCount)
	}
	// 幂等生效时，库存只应被扣一次
	if stock := currentStock(t, pool, skuID); stock != 999 {
		t.Errorf("幂等命中却扣了多次库存：stock=%d，期望 999", stock)
	}
}

// 支付回调重复投递只生效一次：第二次必须 handled=false，且订单仍为 PAID。
func TestHandleNotify_DuplicateCallbackIsIdempotent(t *testing.T) {
	svc, pool := newTestService(t)
	skuID, userID := seedStockFixture(t, pool, 100)

	view, err := svc.CreateOrder(context.Background(), userID, "", []service.OrderLineInput{
		{SkuID: skuID, Qty: 1},
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	orderNo := view.Order.OrderNo

	body := []byte(fmt.Sprintf(`{"order_no":%q,"amount_cents":1234}`, orderNo))

	first, err := svc.HandleNotify(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("首次回调失败: %v", err)
	}
	if !first.Handled || first.Status != string(domain.OrderPaid) {
		t.Fatalf("首次回调应把订单改为 PAID，实际 handled=%v status=%s", first.Handled, first.Status)
	}

	second, err := svc.HandleNotify(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("重复回调不应报错，实际: %v", err)
	}
	if second.Handled {
		t.Error("重复回调被当成新事件处理了 —— 幂等失效，会导致重复发货/重复记账")
	}
	if second.Status != string(domain.OrderPaid) {
		t.Errorf("重复回调后状态变成 %s，仍应为 PAID", second.Status)
	}

	var eventCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_events WHERE out_trade_no = $1`, orderNo).Scan(&eventCount); err != nil {
		t.Fatalf("统计支付事件失败: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("同一笔回调落了 %d 条支付事件，期望 1 条", eventCount)
	}
}
