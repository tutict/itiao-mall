// Package connlimit 给 net.Listener 加"同时打开的连接数上限"。
//
// # 为什么 Go 的 http.Server 需要一个外挂的上限
//
// net/http 的 Server **没有**连接数上限。它每接受一个 TCP 连接就起一个 goroutine，
// 上限由操作系统决定 —— 而这个"上限"通常是 file descriptor 的 RLIMIT_NOFILE。
// 撞上之后的表现是 accept 开始失败，而 fd 是**进程级**资源：一个被爬的公开接口
// 把 fd 吃光，同进程里的日志、指标、健康探针会一起不工作。
//
// 对外的 JSON API 必须自己钉一个上限，理由有三条：
//
//  1. **慢速攻击的成本极低。** 攻击者开 N 个连接、每个只发一部分 header 然后挂住，
//     服务端就要为每个连接保持一个 goroutine 与一个 fd。ReadHeaderTimeout(5s)
//     只能限制**单个**连接的存活时间，限制不了**同时**有多少个 ——
//     攻击者只要让"打开速率 > 超时回收速率"，并发连接数就能一直堆上去。
//     实测里的判别信号是 `dial tcp: ... actively refused`：那不是应用拒绝的，
//     是内核 backlog 打满后回的 RST。
//
//  2. 上限把过载变成一个**看得见、可恢复**的错误（新连接被挡得更早），
//     而不是一次静默的资源泄漏（fd 慢慢耗尽，直到无关组件先崩）。
//
//  3. 满了之后的行为是**停止 accept**，新连接停在**内核 backlog** 里排队。
//     这是正确的背压方向：已经建立的连接继续被服务，而不是所有连接一起变慢。
//     backlog 也满时由内核回 RST —— 客户端立刻知道该退避重试，
//     好过在应用层排队到超时（那时连接已经建上、资源已经付出去了）。
//
// # 为什么不用 golang.org/x/net/netutil
//
// 那个实现与下面这份在行为上等价，引它进来只会多一个依赖面。这份实现
// 只处理 netutil 也处理的同一件事（占槽 → accept → 连接关闭时释放），
// 所以自己维护的成本很低。（若将来发现行为差异，优先切回 netutil。）
package connlimit

import (
	"net"
	"sync"
)

// LimitListener 限制同时在服务中的连接数。
//
// 零值不可用，必须用 NewLimitListener 构造。
type LimitListener struct {
	net.Listener

	// sem 是槽位。每接受一个连接占一个，连接 Close 时归还。
	//
	// 用带缓冲 chan 而不是 atomic 计数，是为了让 accept 在满的时候**阻塞**
	// 而不是自旋 —— 阻塞能让出 P，不浪费 CPU。
	sem chan struct{}

	// done 在 listener 关闭时被关闭。没有它，Accept 会永久阻塞在满槽的 sem 上：
	// 关闭时没有任何连接会 Close，也就没有任何东西会归还槽位。
	done      chan struct{}
	closeOnce sync.Once
}

// NewLimitListener 包一层 listener，最多同时服务 n 个连接。
//
// n ≤ 0 时 panic 而不是"当作不限"：一个传 0 或负数的调用点，
// 意图几乎一定是从配置里读到了空值，而"以为设了上限、实际没有"
// 正是这个组件要防的事 —— 它必须响，不能静默失效。
func NewLimitListener(l net.Listener, n int) *LimitListener {
	if n <= 0 {
		panic("connlimit: 连接上限必须为正数（0/负数会静默变成'不限'）")
	}
	return &LimitListener{
		Listener: l,
		sem:      make(chan struct{}, n),
		done:     make(chan struct{}),
	}
}

// acquire 尝试占一个槽位。成功返回 true；listener 已关闭返回 false。
//
// 两个 select 分支缺一不可：只等 sem 的话，Close 会让下一条 Accept
// 永久卡在这里（下面注释里的那句"expect accept won't block"就是这个意思）。
func (l *LimitListener) acquire() bool {
	select {
	case <-l.done:
		return false
	case l.sem <- struct{}{}:
		return true
	}
}

func (l *LimitListener) release() { <-l.sem }

// Accept 在有空余槽位时接受连接，否则阻塞（把新连接留在内核 backlog）。
func (l *LimitListener) Accept() (net.Conn, error) {
	acquired := l.acquire()

	// 若因为 listener 已关闭而没拿到槽位，下面的 Accept 不会阻塞，
	// 会立刻回一个错误 —— 所以这里不需要额外的超时。
	c, err := l.Listener.Accept()
	if err != nil {
		if acquired {
			l.release()
		}
		return nil, err
	}
	return &limitConn{Conn: c, release: l.release}, nil
}

// Close 关闭底层 listener，并唤醒所有阻塞在 acquire 的 Accept。
func (l *LimitListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

// limitConn 在关闭时归还槽位。
type limitConn struct {
	net.Conn

	releaseOnce sync.Once
	release     func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	// 用 Once 兜住：Close 被调用两次（某些 http.Server 路径会）
	// 不能归还两个槽位，否则上限会被慢慢放大，最后等于没有。
	c.releaseOnce.Do(c.release)
	return err
}
