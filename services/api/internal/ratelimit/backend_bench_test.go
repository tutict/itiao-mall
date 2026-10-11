package ratelimit

// miniredis 与真 Redis 的对照基准。
//
// # 为什么需要这个文件
//
// `middleware_bench_test.go` 里量到 `Decide` 约 96µs/请求、845 次分配。
// 这个数字**很可能被测试后端放大了**，而不是真实成本。
//
// 机制：`Script.Run` 先试 `EVALSHA`，收到 `NOSCRIPT` 才回退到 `EVAL`
// （go-redis v9.16 script.go:65-70）。若 miniredis 不缓存已加载的脚本，
// 每次调用都要把**整段 Lua 源码**（本文件下方那段，约 1.3KB）当作命令参数发过去，
// 序列化 + 传输 + 解析的成本与真实 Redis 上的 EVALSHA 差一个量级。
//
// 于是「96µs 里有多少是限流器自己的 CPU、多少是测试后端的假象」这个问题，
// 直接决定优化方案该往哪走：
//   - 若真Redis 也是几十微秒 →瓶颈是网络往返，要减少往返次数（本地预筛/放宽）；
//   - 若真 Redis 只有几微秒 → 瓶颈是分配与编码，要就地优化（复用缓冲、少派生 context）。
//
// # 判据口径
//
// 真 Redis 的读数天然带网络抖动，所以用 `-benchtime` 多次取稳态，
// 并且**同时报 allocs**：分配数是确定的、不随网络抖动的，
// 它是判断"是不是客户端侧开销"的可靠信号。

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func benchPolicy() Policy {
	return Policy{Scope: ScopeRead, Action: Action("product.read"),
		Window: time.Minute, Limit: 1000000000}
}

// BenchmarkDecide_Miniredis 复现 middleware_bench 里的那个读数。
func BenchmarkDecide_Miniredis(b *testing.B) {
	mr := miniredis.RunT(b)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	b.Cleanup(func() { _ = cli.Close() })
	l := New(cli, "bench", 200*time.Millisecond, false)

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := l.Decide(ctx, benchPolicy(), "bench-subject", Defaults{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecide_RealRedis 是对照组。
//
// ★ 用环境变量给连接串，不写死 localhost:6379 ——
//
//	本机的 Redis 端口由 compose 映射决定（可能是 15433 之类），
//	写死会让这条基准在别的机器上连不上，表现为"忘了看 skip"。
//	连不上时**直接 Fatal 而不是 Skip**：Skip 在退出码上与通过无法区分，
//	而这条基准的价值恰恰在于它给出的数字必须是真的。
func BenchmarkDecide_RealRedis(b *testing.B) {
	addr := os.Getenv("BENCH_REDIS_ADDR")
	if addr == "" {
		b.Fatal("需要 BENCH_REDIS_ADDR（例如 127.0.0.1:6379）。" +
			"刻意不做成跳过：跳过的基准等于没有基准")
	}
	cli := redis.NewClient(&redis.Options{
		Addr: addr,
		// 与生产一致：不重试。理由见 NewDedicatedRedis 的注释 ——
		// 重试会把一次局部故障放大成全站变慢。
		MaxRetries: -1,
		PoolSize:   50,
	})
	b.Cleanup(func() { _ = cli.Close() })

	ctx := context.Background()
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := cli.Ping(pingCtx).Err(); err != nil {
		b.Fatalf("连不上 %s: %v", addr, err)
	}

	l := New(cli, "bench", 200*time.Millisecond, false)
	// 预热：第一次调用会 NOSCRIPT → EVAL，之后才是真实路径。
	if _, _, err := l.Decide(ctx, benchPolicy(), "warmup", Defaults{}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := l.Decide(ctx, benchPolicy(), "bench-subject", Defaults{}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	// 收尾：别把基准键留在共享 Redis 里。
	keys, _ := cli.Keys(context.Background(), "bench:*").Result()
	if len(keys) > 0 {
		_ = cli.Del(context.Background(), keys...).Err()
	}
}

// BenchmarkRoundTrip_Ping 是**纯网络往返**的成本基线。
//
// 它不含 Lua、不含编解码、不含业务逻辑，就是一个 PING 往返。
// 拿它与 BenchmarkDecide_RealRedis 相减，就得到"限流器为了做限流这件事
// 额外花了多少"，而两者的**比值**直接决定优化方向：
//
//   - 若两者接近 → 成本几乎全在往返 → 优化手段是**减少往返次数**，
//     就地省 CPU 没有意义；
//   - 若 Decide 只是 Ping 的几倍 → 成本在编解码 / Lua 求值 → 该就地优化。
//
// 这个区分是本轮所有结论的支点，所以值得单独一条。
//
// ★ 本机实测（2026-10-11，Redis 跑在 Docker/WSL2）：
//
//	Ping            ~350µs
//	Decide          ~370µs    ← 与 Ping 几乎相同
//	Pipeline/判定    ~9.5µs
//
// Decide ≈ Ping 说明限流器的成本**几乎全部是一次网络往返**，
// Lua 与编解码加起来只占个位数百分比。
func BenchmarkRoundTrip_Ping(b *testing.B) {
	addr := os.Getenv("BENCH_REDIS_ADDR")
	if addr == "" {
		b.Fatal("需要 BENCH_REDIS_ADDR")
	}
	cli := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, PoolSize: 50})
	b.Cleanup(func() { _ = cli.Close() })

	ctx := context.Background()
	if err := cli.Ping(ctx).Err(); err != nil {
		b.Fatalf("连不上 %s", addr)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cli.Ping(ctx).Err(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecide_PipelineUpperBound 用 pipeline 把 N 次判定合并成一次往返。
//
// 它回答的是"减少往返次数能省多少"：若单发60µs、pipeline 后每判定降到个位数µs，
// 那"少往返"就是正确方向；若 pipeline 收益很小，说明成本在别处。
//
// 意义是给优化方案定界：这是**上限测量**——
// 它把 Redis 的网络往返降到最低，剩下的就是纯客户端成本。
func BenchmarkDecide_PipelineUpperBound(b *testing.B) {
	addr := os.Getenv("BENCH_REDIS_ADDR")
	if addr == "" {
		b.Fatal("需要 BENCH_REDIS_ADDR")
	}
	cli := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, PoolSize: 50})
	b.Cleanup(func() { _ = cli.Close() })

	ctx := context.Background()
	if err := cli.Ping(ctx).Err(); err != nil {
		b.Fatalf("连不上 %s", addr)
	}

	const batch = 100
	script := slidingWindowScript
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += batch {
		pipe := cli.Pipeline()
		for j := 0; j < batch; j++ {
			pipe.EvalSha(ctx, script.Hash(), []string{"bench:pipe:" + string(rune('a'+j%26))},
				60000, 1000000000, "member", time.Now().UnixMilli())
		}
		if _, err := pipe.Exec(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	keys, _ := cli.Keys(context.Background(), "bench:*").Result()
	if len(keys) > 0 {
		_ = cli.Del(context.Background(), keys...).Err()
	}
}
