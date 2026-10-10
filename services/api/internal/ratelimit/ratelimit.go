// Package ratelimit 用 Redis 有序集合实现**滑动窗口**限流。
//
// # 为什么是滑动窗口，不是固定窗口
//
// 固定窗口（`INCR key; EXPIRE key 60`）有个众所周知但总被重犯的错误：
// 窗口边界两侧的请求会在**同一秒内**都通过。60 秒窗口下，攻击者可以在
// 第 59.9 秒发 10 个、第 60.1 秒再发 10 个 —— 平均速率等于 20 秒 10 个的
// 2 倍，而限流器显示"从未超过 10/分钟"。对登录接口来说，这恰好是最关键的那几秒：
// 它允许的正是「密码喷洒」需要的节奏。
//
// 换成滑动窗口后，边界那一秒的 20 个请求里只有 10 个能进，
// 因为计数依据是"过去 60 秒内实际发生过什么"，而不是"当前落在哪个桶里"。
//
// # 为什么用 ZSET 而不是两次 INCR
//
// 固定窗口可以用计数器，滑动窗口不行 —— 它需要知道每个请求的**发生时刻**。
// ZSET 的 score 存毫秒时间戳、member 存唯一 ID，于是"删掉窗口外的 + 统计窗口内的"
// 就是这个数据结构最自然的用法。
//
// # 为什么必须用 Lua
//
// 滑动窗口的核心步骤是三步：ZREMRANGEBYSCORE（剔除过期）→ ZCARD（计数）→ ZADD（记录）。
// 三步分开发就是三次往返，中间那个窗口里并发请求会读到同一个基数，
// 于是"最后一个请求是否放行"的判断出现竞态：N 个并发请求会全部通过。
// Lua 在 Redis 里是单线程原子执行的，把三步合成一步就消掉了这个窗口。
package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrBackendUnavailable 表示 Redis 侧不可用（连不上、超时、返回了非法响应）。
//
// 调用方**必须**区分它与"限流命中"：前者是基础设施故障，应当走降级策略；
// 后者是业务判定，必须回 429。把两者混为一谈的结果是 Redis 一抖动全站 503，
// 或者反过来 —— 限流形同虚设还查不出原因。
var ErrBackendUnavailable = errors.New("限流后端不可用")

// BackendError 包装"限流后端不可用"这一类失败。
//
// # 为什么需要一个类型，而不是 fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
//
// 这个错误有两个去向，而它们对信息量的要求正好相反：
//
//   - 日志与排查：需要底层原文（"dial tcp 10.0.3.11:6379: connect: connection refused"）；
//   - 任何**可能**回给客户端的路径：一个字都不能带（内网地址就是一张攻击地图）。
//
// 拼成一个字符串时，两个去向只能共用同一份文本 —— 想让日志有用，就得顺手把
// 内网地址交给每一个能触发后端故障的人。拆开之后，Error() 只输出哨兵那句话，
// Cause 由调用方**显式**取用（见 Cause 函数）。于是将来谁误把这个错误写进响应体，
// 拿到的也只是"限流后端不可用"，而 errors.Is(err, ErrBackendUnavailable) 照常成立。
type BackendError struct {
	// Cause 是底层原因。**只允许进日志**，不要拼进任何对外文本。
	Cause error
}

func (e *BackendError) Error() string { return ErrBackendUnavailable.Error() }

// Unwrap 返回底层原因，让 errors.Is(err, 某个底层哨兵) 仍可穿透。
func (e *BackendError) Unwrap() error { return e.Cause }

// Is 让 errors.Is(err, ErrBackendUnavailable) 成立。
//
// 必须单独实现：Unwrap 指向 Cause 之后，错误链上就没有哨兵了 ——
// 归因会静默失效，Decide 里的 fail-closed 分支不再成立，
// 结果是"配置为收紧的限流器在 Redis 故障时变成放行"。
func (e *BackendError) Is(target error) bool { return target == ErrBackendUnavailable }

// Cause 取出 BackendError 里的底层原因（不是 BackendError 就原样返回）。
//
// 存在的意义是把"取原文"变成一个**显式动作**：只该写日志的调用方调它，
// 而任何面向响应的代码路径都不会顺手拿到它。
func Cause(err error) error {
	var be *BackendError
	if errors.As(err, &be) && be.Cause != nil {
		return be.Cause
	}
	return err
}

// slidingWindowScript 是滑动窗口的原子实现。
//
// KEYS[1] = 窗口有序集合的 key
// ARGV[1] = 窗口大小（毫秒）
// ARGV[2] = 阈值（窗口内最多允许多少次）
// ARGV[3] = 本次请求的唯一 ID（作为 ZSET member）
// ARGV[4] = 当前时间（毫秒）
//
// 返回 {计数, 是否放行(1/0), 最早那条记录的时间戳}
//
// 刻意**不**在拒绝时 ZADD：超限的请求不该占额度，否则攻击者用远超阈值的
// 请求量把窗口塞满，会让正常用户也被挤掉（而且他自己的重试在帮倒忙）。
// 不记录 → 他就得等窗口自然滑走，这是限流该有的行为。
var slidingWindowScript = redis.NewScript(`
local key    = KEYS[1]
local window = tonumber(ARGV[1])
local limit  = tonumber(ARGV[2])
local member = ARGV[3]
local now    = tonumber(ARGV[4])

-- 只剔除**严格早于**窗口起点的记录。用 <= 会把恰好落在边界上的那条算作过期，
-- 于是窗口实际比配置短 1 毫秒。这种偏差单看测试发现不了，但窗口越短、
-- 阈值越低，放大效应越明显（1ms/100ms 的窗口上就是 1% 的额度凭空蒸发）。
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)

local count = redis.call('ZCARD', key)

if count >= limit then
    -- 取窗口内最早的一条，算出它什么时候滑出窗口 —— 那就是调用方最早能重试的时刻。
    -- 拿它而不是"现在 + 窗口"：后者会让一个刚被拒的调用方白白多等一整个窗口。
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local oldestAt = now
    if oldest[2] then
      oldestAt = tonumber(oldest[2])
    end
    return {count, 0, oldestAt}
end

redis.call('ZADD', key, now, member)
-- TTL 取窗口的两倍：这样即便某个请求在窗口末尾才进来，它自己那条记录
-- 也能在滑出统计范围之后再活一会儿，不会出现"key 提前消失导致计数骤降"。
redis.call('PEXPIRE', key, window * 2)
return {count + 1, 1, now}
`)

// Result 是一次限流判定的结论。
type Result struct {
	// Allowed 是否放行。
	Allowed bool
	// Count 是本次判定后窗口内的请求数（含被放行的这次）。
	Count int64
	// Limit 是该策略的阈值，响应头要用。
	Limit int
	// RetryAfter 是被拒时建议的等待时长；放行时为 0。
	//
	// 注意语义是"**至少**等这么久"而不是"等这么久就好"：Redis 里那条记录
	// 可能马上又被别人的请求推后。客户端应把它当作退避下界。
	RetryAfter time.Duration
}

// Limiter 是限流器。零值不可用，必须用 New 构造。
type Limiter struct {
	// rdb 刻意只声明成 Scripter（Eval/EvalSha/ScriptExists/ScriptLoad），
	// 不是 UniversalClient。限流器真正用到的只有"跑一段 Lua"这一件事，
	// 把依赖面收到最小有两个好处：意图更清楚，以及测试可以注入一个
	// 必然失败、且错误文本可控的假后端来验证"错误不外泄" ——
	// 靠真连不上的 Redis 测那条，结论会随 refused/timeout 形态漂移。
	rdb    redis.Scripter
	prefix string

	// 单次判定的超时。它独立于调用方的 ctx：调用方可能给了 30 秒的
	// HTTP 超时，而限流只需要几百毫秒就能给出结论 —— 不设上限的话，
	// Redis 卡住会把每个请求都拖到 30 秒后才失败。
	timeout time.Duration

	// failsClosed 决定后端故障时是收紧（拒绝）还是放开（放行）。
	failsClosed bool

	// now 可注入，便于测试里精确控制时间边界。
	now func() time.Time
}

// New 构造限流器。prefix 用于隔离环境（dev / prod 共用一个 Redis 时不至于串味）。
//
// failsClosed 传 true 时后端不可用会拒绝所有请求；传 false 则放行。
// 这个决定见 config.RateLimitFailsClosed 的注释 —— 它是一笔安全与可用性的取舍，
// 不该由调用方随手决定，所以本包只接受一个已定的布尔值。
//
// # rdb 必须是"只给限流用"的专用客户端
//
// 这不是洁癖，是**正确性要求**。实测（2026-10-03，go-redis v9.22）：
//
//	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
//	cli.Eval(ctx, ...)   // 后端"连得上但不回话"
//	→ 实际耗时 5.0006826s，返回 "i/o timeout"
//
// **ctx 的 deadline 不会中断一个已经阻塞在 socket 读上的调用。**
// go-redis 在这种情形下等的是它自己的 ReadTimeout（默认 5s）。
// 于是那个 200ms 的 ctx 只是装饰 —— 每个请求仍然要挂满 5 秒。
//
// 后果对限流来说是致命的：Redis 主从切换期间，全站每个请求都被限流器
// 拖住 5 秒，而限流器本该是那个"几百毫秒内给出结论、不拖累任何人"的组件。
//
// 修法是让限流器有一个 ReadTimeout 与自身 timeout 匹配的专用客户端
// （见 main.go 的 newLimiterRedis）。**不要**图省事共用业务 Redis 客户端：
// 把它的 ReadTimeout 调到几百毫秒会让 refresh 轮换在跨机房 Redis 上误超时。
//
// 参数类型是 redis.Scripter 而不是 UniversalClient：限流只用得到"执行 Lua"，
// 收窄之后测试能注入假后端（见 Limiter.rdb 的注释）。*redis.Client 与
// *redis.ClusterClient 都满足这个接口，调用方无需改动。
func New(rdb redis.Scripter, prefix string, timeout time.Duration, failsClosed bool) *Limiter {
	if prefix == "" {
		prefix = "rl"
	}
	if timeout <= 0 {
		// 显式失败而不是退回某个默认值：默认超时的选择是一个部署决定，
		// 而这里能发现"有人传了 0"这件事本身比默认值重要。
		panic("ratelimit: timeout 必须为正数")
	}
	return &Limiter{
		rdb:         rdb,
		prefix:      prefix,
		timeout:     timeout,
		failsClosed: failsClosed,
		now:         time.Now,
	}
}

// policy 是一个已解析的限流策略：走哪个 key 维度、用哪个窗口与阈值。
type policy struct {
	window time.Duration
	limit  int
}

// Scope 是限流维度的名字。它同时也是 Redis key 的第二段，出现在监控里。
//
// key 维度被明确收成这三类，是为了让"限流键爆炸"这件事不可能发生：
// 键的来源只能是「主体标识 + 固定动作名」，两者都不来自用户输入。
type Scope string

const (
	// ScopeAuth 按主体（IP 或账号）限制认证类动作：登录、注册、刷新、登出。
	ScopeAuth Scope = "auth"
	// ScopeWrite 按用户限制写操作：下单、改购物车、商品维护。
	ScopeWrite Scope = "write"
	// ScopeRead 按 IP 限制读操作：浏览商品、搜索。
	ScopeRead Scope = "read"
)

// Action 是一次具体动作的稳定标识（如 "login"）。
// 它是常量而不是路径 —— 路径会随路由重命名而变，用它当 key 的一部分
// 会导致改个路由名就把限流计数清零，攻击窗口无声地重新打开。
type Action string

// Policy 描述一个动作的限流规则。
type Policy struct {
	// Scope 决定 key 的主体维度。
	Scope Scope
	// Action 决定 key 的动作段。
	Action Action
	// Window 与 Limit 为零时从 Limiter 的默认值取。
	Window time.Duration
	Limit  int
}

// DefaultPolicy 给出该 Limiter 的兜底阈值。
type Defaults struct {
	Window time.Duration
	Limit  int
}

// Allow 对一次请求做限流判定。
//
// subject 是主体标识：已登录请求传 user id，匿名请求传客户端 IP。
// **不要**传原始 IP 之外的用户输入（手机号、token 等）：它们直接进 Redis key，
// 可被用来在 key 空间里制造大量一次性条目。手机号这类标识要先哈希。
func (l *Limiter) Allow(ctx context.Context, p Policy, subject string, def Defaults) (Result, error) {
	window, limit := p.Window, p.Limit
	if window <= 0 {
		window = def.Window
	}
	if limit <= 0 {
		limit = def.Limit
	}
	if window <= 0 || limit <= 0 {
		// 走到这里说明配置缺了窗口或阈值。与其"当作不限流"，
		// 不如显式失败：不限流是**安全侧的失效**，它不会自己暴露。
		return Result{}, fmt.Errorf("%w: 窗口与阈值必须为正数", ErrBackendUnavailable)
	}
	if subject == "" {
		return Result{}, errors.New("限流主体不能为空：空 key 会让所有匿名请求共用一个计数器")
	}

	// 判定自身的超时独立于调用方，见 Limiter.timeout。
	//
	// 注意它**不是**唯一防线：go-redis 在"连得上但不回话"时等的是
	// 客户端自己的 ReadTimeout，ctx deadline 拦不住（见 New 的注释）。
	// 所以这一层是"尽快放弃"，真正保证上限的是那个专用客户端的 ReadTimeout。
	// 两层都要有：ctx 负责"调用方已经放弃了就别再纠缠"，
	// ReadTimeout 负责"Redis 卡住时别把请求拖满 5 秒"。
	judgeCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	now := l.now()
	res, err := slidingWindowScript.Run(judgeCtx, l.rdb,
		[]string{l.key(p, subject)},
		window.Milliseconds(),
		limit,
		nextMember(now),
		now.UnixMilli(),
	).Slice()

	if err != nil {
		// 刻意**不**把 err 原样上抛给 HTTP 层：Redis 的错误原文含内网地址与
		// 拓扑信息（"dial tcp 10.0.3.11:6379: connect: connection refused"），
		// 外泄等于给攻击者画地图。
		//
		// 注意这里**不能**写成 `fmt.Errorf("%w: %v", ErrBackendUnavailable, err)` ——
		// 那正是这次要修掉的形态：拼成字符串之后，Error() 里必然带着原文，
		// 任何把它写进响应体的路径都会泄露。改用 BackendError：哨兵在 Error() 里，
		// 原文挂在 Cause 上只进日志。
		if ctx.Err() != nil {
			// 调用方自己取消了（客户端已断开 / HTTP 超时）。这不算后端故障，
			// 也不该被算成"限流失败"而触发降级 —— 直接上抛调用方的 ctx 错误。
			return Result{}, ctx.Err()
		}
		return Result{}, &BackendError{Cause: err}
	}
	if len(res) != 3 {
		return Result{}, fmt.Errorf("%w: Lua 返回了 %d 个值，期望 3 个", ErrBackendUnavailable, len(res))
	}

	count, ok1 := res[0].(int64)
	allowed, ok2 := res[1].(int64)
	oldestAt, ok3 := res[2].(int64)
	if !ok1 || !ok2 || !ok3 {
		return Result{}, fmt.Errorf("%w: Lua 返回值类型异常", ErrBackendUnavailable)
	}

	out := Result{
		Allowed: allowed == 1,
		Count:   count,
		Limit:   limit,
	}
	if !out.Allowed {
		// 最早那条记录滑出窗口的时刻 = 最早可重试时刻。
		wait := time.Duration(oldestAt+window.Milliseconds()-now.UnixMilli()) * time.Millisecond
		if wait < time.Millisecond {
			// 至少给 1ms。回 0 会让客户端立即重试，
			// 而 Retry-After: 0 本身就是一个"无脑快刷"的邀请。
			wait = time.Millisecond
		}
		out.RetryAfter = wait
	}
	return out, nil
}

// key 拼出 Redis key：rl:{环境}:{scope}:{action}:{subject}。
//
// 刻意不写 hash tag（{...}）：限流的键天然是分散的，而同一用户的多个动作
// 如果强行落在同一个 slot，会让单次 Lua 无法跨键 —— 现在不需要跨键，
// 也就没必要付那个"把热点压到一个分片"的代价。
func (l *Limiter) key(p Policy, subject string) string {
	return l.prefix + ":" + string(p.Scope) + ":" + string(p.Action) + ":" + subject
}

// Decide 是带降级的判定入口，HTTP 中间件只用这一个。
//
// 后端不可用时按 failsClosed 决定拒绝还是放行。返回值里的 Degraded 让调用方
// 能把"因为后端挂了才放行"这件事写进日志 —— 不然事后复盘时，
// 限流完全失效与"这段时间没人触发限流"在监控上长得一模一样。
//
// 顺带说明为什么**不**在这里做重试：Redis 故障通常不是瞬时的，
// 重试只会把每个请求的延迟拉长到 timeout 的倍数，把局部故障放大成全局超时。
// 该由客户端带抖动重试，而不是由准入层闷头重试。
func (l *Limiter) Decide(ctx context.Context, p Policy, subject string, def Defaults) (res Result, degraded bool, err error) {
	res, err = l.Allow(ctx, p, subject, def)
	if err == nil {
		return res, false, nil
	}
	if errors.Is(err, ErrBackendUnavailable) && l.failsClosed {
		return Result{Allowed: false, Limit: res.Limit}, true, err
	}
	// 放行两种情况：显式配置 open，或调用方自己取消了 ctx（不是后端故障）。
	return Result{Allowed: true, Limit: res.Limit}, errors.Is(err, ErrBackendUnavailable), nil
}

// FailsClosed 暴露降级方向，供中间件决定被拒时回 503 还是 429。
func (l *Limiter) FailsClosed() bool { return l.failsClosed }

// NewDedicatedRedis 建一个**只给限流用**的 Redis 客户端。
//
// 存在的理由见 New 的注释：go-redis 的 ctx deadline 拦不住阻塞中的 socket 读，
// 真正把单次判定钉在 timeout 上的是这里的 ReadTimeout / WriteTimeout。
//
// 为什么不复用业务 Redis 客户端（cache.New 建的那个）：它的 ReadTimeout 是 3s，
// 那是为 refresh 轮换定的（那个操作要跑 Lua 且不能轻易超时）。
// 把它调到 300ms 会让跨机房 Redis 上的 refresh 轮换误超时 ——
// 那是一个会被误判成"用户被登出"的故障。
//
// 代价是多一个连接池（默认 20 条连接）。这个数字对本组件是合适的：
// 它只需要承载准入判定的并发，而准入判定本身已经被限流阈值卡住了。
func NewDedicatedRedis(addr, password string, db int, timeout time.Duration) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
		// 池子刻意小：这是准入通道，不需要吞吐。连接多只会让后端故障时
		// 更多请求一起卡在同一个不可用的对端上。
		PoolSize:     10,
		MinIdleConns: 2,

		// 这三个是真正把单次判定钉在 timeout 上的参数。
		DialTimeout:  timeout,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,

		// 不重试。理由：Redis 故障通常不是瞬时的，而重试会把每个请求的
		// 延迟拉长到 timeout 的整数倍 —— 把一次局部故障放大成全站变慢。
		// 该由客户端带抖动退避重试，而不是由准入层闷头重试。
		MaxRetries: -1,
	})
}

// ---- member 唯一性 ----
//
// ZSET 的 member 相同就等于同一条记录，后写的覆盖先写的，计数凭空少 1。
// 所以 member **必须**唯一，而"用时间戳就好"这个直觉在 Windows 上是错的：
// 那边的 time.Now() 精度只到毫秒级，同一毫秒里的多次调用拿到完全相同的
// 纳秒值。实测（2026-10-02，Windows 11）：7 次连续判定里有 2 次计数丢失，
// 表现为"阈值 5 的限流器放行了 7 次"，而 ZSET 里只有 4 条。
// 而且这类故障不会报错、不会 panic，只表现为限流比配置更松 ——
// 属于最难发现的一类。
//
// 所以 member = 随机实例前缀 + 进程内原子序号 + 毫秒时间戳：
//   - 原子序号保证单进程内唯一（这是主要作用）；
//   - 随机前缀保证多实例之间不撞（同一个 Redis 被多个 API 进程共用是部署常态，
//     没有前缀的话每个实例的序号都从 1 开始，member 会跨实例重名）；
//   - 时间戳只是为了让 key 空间不至于被单一前缀垄断，删掉也不影响正确性。
var memberSeq atomic.Uint64

var memberPrefix = func() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// 熵源不可用时不能退化成"纯序号"：多实例会互相覆盖对方的记录，
		// 表现为限流在有多个实例时莫名失效。用时间兜底，宁可撞也不静默。
		return strconv.FormatInt(time.Now().Unix(), 36)
	}
	return hex.EncodeToString(b)
}()

func nextMember(now time.Time) string {
	return fmt.Sprintf("%s-%d-%d", memberPrefix, memberSeq.Add(1), now.UnixMilli())
}

// ---- 供测试与工具使用的解析辅助 ----

// ParseIntDefault 把字符串解析成 int，失败时返回默认值。
// 用于把可能为空的 query/header 归一成配置默认值。
func ParseIntDefault(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
