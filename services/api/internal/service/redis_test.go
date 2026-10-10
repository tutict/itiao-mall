package service_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newRedisForTest 给 service 包的用例一个干净可用的 Redis。
//
// ★ 默认走 miniredis（进程内、永远能跑）。
//
// 原来这里是"连不上真 Redis 就 t.Skipf"，后果有两层，第二层更贵：
//
//  1. 无 Redis 的机器上，refresh 相关的用例**静默跳过**，而 go test 退出码仍是 0 ——
//     门禁绿着，但轮换、重放、家族吊销、并发唯一性这些核心逻辑一次都没执行过。
//  2. 它污染变异验证：施加变异后测试"仍然全绿"，看起来像"守卫是假的"，
//     实际是守卫根本没运行。这类假结论比没有结论更坏。
//
// 所以规则与 ratelimit / httpapi 两个包保持一致：默认 miniredis；
// 设 RATELIMIT_TEST_REAL_REDIS=1 时连真机（用于比对 miniredis 与真 Redis 在
// Lua 上的行为差异）；**连接失败一律 t.Fatal**，不再跳过。
//
// ★ 这是 service 包内唯一一份 Redis 测试客户端构造。
//
// 历史上 ratelimit 包改过一处、漏了 httpapi 包那份副本 —— 同一个辅助函数
// 在多个包里各有一份时，改一处不等于改完。所以这里刻意只留一个实现，
// token / merchant 两个 helper 都调它。
func newRedisForTest(t *testing.T) *redis.Client {
	t.Helper()

	if os.Getenv("RATELIMIT_TEST_REAL_REDIS") == "1" {
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			addr = "localhost:6379"
		}
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = rdb.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			t.Fatalf("指定了 RATELIMIT_TEST_REAL_REDIS=1 但连不上 Redis(%s): %v", addr, err)
		}
		return rdb
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}
