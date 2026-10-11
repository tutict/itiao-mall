package connlimit_test

import (
	"net"
	"testing"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/platform/connlimit"
)

// acceptInBackground 在后台跑 accept 循环，把接受的连接送进返回的 channel。
//
// 必须后台跑：Accept 在上限满时是**阻塞**的，直接在测试 goroutine 里调用它
// 会让"检查第 3 个连接有没有被接受"的那段代码永远执行不到。
func acceptInBackground(l net.Listener) <-chan net.Conn {
	ch := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(ch)
				return
			}
			ch <- c
		}
	}()
	return ch
}

func TestLimitListener_CapsConcurrentConns(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ll := connlimit.NewLimitListener(base, 2)
	defer ll.Close()

	accepted := acceptInBackground(ll)
	addr := base.Addr().String()

	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial 失败: %v", err)
		}
		return c
	}

	// 建 3 个连接。三次 Dial **都会成功** —— TCP 三次握手在内核完成，
	// 连接落进 backlog，与应用的 Accept 循环是否跟得上无关。
	// 这正是连接上限起作用的位置：它让第 3 个连接停在 backlog 里，
	// 而不是让应用多发一个 goroutine 去服务它。
	c1, c2, c3 := dial(), dial(), dial()
	defer c3.Close()
	defer c1.Close()
	defer c2.Close()

	var served []net.Conn
	for i := 0; i < 2; i++ {
		select {
		case c, ok := <-accepted:
			if !ok {
				t.Fatal("listener 意外关闭")
			}
			served = append(served, c)
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 个连接应在上限内被接受", i+1)
		}
	}

	// 第 3 个连接不该被接受。
	select {
	case _, ok := <-accepted:
		if ok {
			t.Fatal("超过上限的连接不该被接受")
		}
		t.Fatal("listener 意外关闭")
	case <-time.After(300 * time.Millisecond):
		// 正确：第 3 个连接卡在 backlog 里，没有占到一个服务端 goroutine。
	}

	// 关掉一个**服务端**已接受的连接 → 归还槽位 → 第 3 个连接应当进来。
	// 这一条是整个组件最容易写错的地方：忘了在连接关闭时归还槽位，
	// 上限就会在第一次达到之后永久卡死（表现为"服务活着但再也不接受连接"）。
	if err := served[0].Close(); err != nil {
		t.Fatalf("关闭服务端连接失败: %v", err)
	}
	select {
	case _, ok := <-accepted:
		if !ok {
			t.Fatal("listener 意外关闭")
		}
		// 正确：归还的槽位让等待中的第 3 个连接被接受。
	case <-time.After(3 * time.Second):
		t.Fatal("关闭一个连接后，等待中的连接应被接受（槽位没归还？）")
	}
}

// TestLimitListener_RepeatedCloseReleasesOnce 守住"槽位只能归还一次"。
//
// 为什么这条值得单独测：release 的实现是 `<-l.sem`（从带缓冲 chan 读一格）。
// 若 Close 被调用两次而归还也执行两次，**第二次会阻塞** —— sem 已经空了，
// 读一个空 channel 会永久挂住。后果不是"上限变松"，而是 Close 本身死锁。
// 而 net/http 的某些路径确实会重复 Close 一个连接。
func TestLimitListener_RepeatedCloseReleasesOnce(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ll := connlimit.NewLimitListener(base, 1)
	defer ll.Close()

	accepted := acceptInBackground(ll)
	addr := base.Addr().String()

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 失败: %v", err)
	}
	defer client.Close()

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("连接未被接受")
	}

	done := make(chan struct{})
	go func() {
		_ = serverConn.Close()
		_ = serverConn.Close() // 第二次：必须立刻返回，不能阻塞在归还槽位上
		close(done)
	}()

	select {
	case <-done:
		// 正确。
	case <-time.After(2 * time.Second):
		t.Fatal("重复 Close 阻塞了：槽位被归还了两次（releaseOnce 没生效）")
	}
}

// TestNewLimitListener_RejectsNonPositive 守住"无效上限必须响"。
//
// 传 0 或负数时静默当作"不限"是最坏的实现：调用点通常是从配置里读到了空值，
// 而"以为设了上限、实际裸奔"正是这个组件要防的事。
func TestNewLimitListener_RejectsNonPositive(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer base.Close()

	for _, n := range []int{0, -1, -100} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("上限为 %d 时应 panic，实际静默返回了", n)
				}
			}()
			connlimit.NewLimitListener(base, n)
		}()
	}
}
