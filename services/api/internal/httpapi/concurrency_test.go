package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
)

// testGateHandler 是测并发闸门用的桩 handler：
//   - 探针路径立刻返回 200，不占住槽位（真实的探针 handler 同样不做业务）；
//   - 其余路径先往 entered 发一个信号，然后挂住直到 release 被关闭。
//
// 探针路径在这里**写死**而不是查 probeExemptPaths：桩的职责是模拟真实下游，
// 若它复用被测实现的那张表，两边会一起错、而测试照样通过。
func testGateHandler() (http.HandlerFunc, chan string, chan struct{}) {
	entered := make(chan string, 32)
	release := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			// 兜底：正常路径下超限请求根本进不来这个 handler；但若某个坏实现
			// 放行了它，这里最多等 5 秒而不是永久挂住 —— 永久挂住会把
			// "本该报红的断言"变成整个包超时，反而看不出是哪条守卫失守。
		}
		w.WriteHeader(http.StatusOK)
	}
	return h, entered, release
}

// TestLogThrottled 守住"日志按时间节流"这条 —— 它是 268,176 条日志那个
// 实测缺陷的修复。
//
// 为什么不测"日志条目数"而是测这个函数：条目数依赖真实并发与时序，
// 写出来的断言会又慢又脆。把"是否该记"抽成纯函数之后，
// 它可以被确定性验证，而"调用点用没用它"由变异体 M55 那一类覆盖。
func TestLogThrottled(t *testing.T) {
	var last atomic.Int64

	// 首次总是放行：last 的零值是 0，而当前 unix 纳秒远大于 0。
	if !logThrottled(&last, time.Hour) {
		t.Fatal("首次应放行")
	}
	// 同一窗口内必须抑制 —— 这正是把 268,176 条压回个位数的机制。
	for i := 0; i < 100; i++ {
		if logThrottled(&last, time.Hour) {
			t.Fatal("同一节流窗口内不该再放行 —— 过载日志会退化成每请求一条")
		}
	}

	// 窗口过去之后应恢复放行。
	var last2 atomic.Int64
	if !logThrottled(&last2, time.Millisecond) {
		t.Fatal("首次应放行")
	}
	time.Sleep(5 * time.Millisecond)
	if !logThrottled(&last2, time.Millisecond) {
		t.Fatal("过了节流窗口应恢复放行（否则过载期间就彻底静默了）")
	}
}

func TestConcurrencyGate_RejectsBeyondLimit(t *testing.T) {
	inner, entered, release := testGateHandler()
	h := ConcurrencyGate(2, time.Second)(inner)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
		}()
	}

	// 确认两个槽位都被占住，再做超限判定 —— 否则下面测的可能是"还没进来"。
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("前两个请求应进入 handler")
		}
	}

	// 第 3 个请求：必须**快速失败**，而不是排队等槽位。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("超过上限应回 503，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("503 响应必须带 Retry-After（否则客户端不知道要退避）")
	}
	// 拒绝路径不该把请求放进 handler。
	select {
	case p := <-entered:
		t.Errorf("被闸门拒绝的请求不该进入 handler（path=%s）", p)
	default:
	}

	close(release)
	wg.Wait()
}

// TestConcurrencyGate_ExemptsProbesEvenWhenFull 守住"过载时探针仍可达"。
//
// 这条不是锦上添花：探针被挡 → 编排系统判实例不健康 → 重启 →
// 冷启动期间吞吐更低 → 更多实例被判不健康。闸门守的正是容量，
// 而它自己触发一场"过载 → 重启"雪崩是最讽刺的失败方式。
func TestConcurrencyGate_ExemptsProbesEvenWhenFull(t *testing.T) {
	inner, entered, release := testGateHandler()
	h := ConcurrencyGate(1, time.Second)(inner)

	// 占满唯一的槽位。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求应进入 handler 并占住槽位")
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("过载时探针 %s 应仍放行（200），实际 %d", path, rec.Code)
		}
	}

	close(release)
	wg.Wait()
}

// TestConcurrencyGate_DoesNotExemptLookalikePaths 守住豁免表的**外侧边界**。
//
// 只测"/healthz 被放行"抓不到"豁免写成前缀匹配"这类错误：写成
// `strings.HasPrefix(path, "/health")` 时上面那条测试照样绿，而任何
// `/health*` 新路径都会静默绕过闸门。所以这里逐条测"形近但不等"的路径，
// 它们**必须**被拒。
func TestConcurrencyGate_DoesNotExemptLookalikePaths(t *testing.T) {
	inner, entered, release := testGateHandler()
	h := ConcurrencyGate(1, time.Second)(inner)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求应进入 handler 并占住槽位")
	}

	for _, path := range []string{"/healthzz", "/healthz/extra", "/readyz/admin", "/health"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("形近路径 %s 不该豁免（应 503），实际 %d —— 豁免表被写宽了", path, rec.Code)
		}
	}

	close(release)
	wg.Wait()
}

// TestConcurrencyGate_DoesNotExemptPercentEncodedVariants 守住豁免判定的**第二个**
// 外侧边界：编码变体。
//
// ★ 这是 2026-10-11 四轴 review 挖出来的 fail-open，形态值得记下来：
// net/http 对 `/%68ealthz` 会给出 `Path="/healthz"` + `RawPath="/%68ealthz"`，
// 而 chi 路由用的是 **RawPath**（非空时优先）⇒ 它找不到 `/healthz` 这条路由、回 404。
// 第一版闸门只查 `r.URL.Path`，于是这类请求**拿到了闸门豁免却并没有走到探针**：
// 它们绕过容量闸门，还落进被刻意排在闸门内层的 AccessLog ——
// "过载时日志是下一个被抽干的资源"这条防线被重新打开。
//
// 判据：RawPath 非空即说明请求用了编码形式，而探针打的是字面路径 → 一律不豁免。
// 注意这条与 LookalikePaths 那条**互补**：那条管"路径名不同"，这条管"名字其实相同、
// 只是编码了" —— 只测前者的话，一个只看 Path 的实现照样全绿。
func TestConcurrencyGate_DoesNotExemptPercentEncodedVariants(t *testing.T) {
	inner, entered, release := testGateHandler()
	h := ConcurrencyGate(1, time.Second)(inner)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求应进入 handler 并占住槽位")
	}

	// 先做前置断言：这些请求的 Path 确实等于探针路径（否则本用例测的东西不成立，
	// 会变成"因为名字对不上才被拒"的假绿）。
	variants := []string{"/%68ealthz", "/health%7A", "/%72eadyz", "/ready%7A"}
	for _, raw := range variants {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		if req.URL.RawPath == "" {
			t.Fatalf("%s 的 RawPath 为空 —— 这条用例的前提（net/http 会保留编码形式）不成立", raw)
		}
		if _, ok := probeExemptPaths[req.URL.Path]; !ok {
			t.Fatalf("%s 解析出的 Path=%q 不在豁免表里 —— 前提不成立，用例测不到目标",
				raw, req.URL.Path)
		}
	}

	for _, raw := range variants {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, raw, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("编码变体 %s 不该豁免（应 503），实际 %d —— "+
				"闸门只看了 Path，攻击者用百分号编码即可绕过（且请求会落进闸门内层的访问日志）",
				raw, rec.Code)
		}
	}

	close(release)
	wg.Wait()
}

// TestConcurrencyGate_LiteralProbeStillExempt 是上一条的**正控制**。
//
// 没有它的话，把 isProbeExempt 写成恒 false（一律不豁免）能让上面那条全绿 ——
// 而那会让探针在过载时被 503，直接触发"过载 → 判不健康 → 重启 → 更过载"的雪崩。
func TestConcurrencyGate_LiteralProbeStillExempt(t *testing.T) {
	inner, entered, release := testGateHandler()
	h := ConcurrencyGate(1, time.Second)(inner)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求应进入 handler 并占住槽位")
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("字面探针路径 %s 必须豁免（过载时仍 200），实际 %d", path, rec.Code)
		}
	}

	close(release)
	wg.Wait()
}

// TestRouter_ConcurrencyGatePlacement 钉住闸门在中间件链里的**位置**。
//
// ★ 为什么这条是静态断言（读源码找 index）而不是行为测试
//
// 这个位置约定只在**过载时**才显形：被闸门拒绝的请求不写访问日志、闸门回的 503
// 也要带安全响应头。要行为验证它得配齐 Deps（logger / PG 池 / Redis）并制造过载，
// 代价远大于收益 —— 而它一旦被挪动，后果恰好是"日志量正比于请求数"这种
// 只在过载时才致命、平时完全看不出来的问题。
//
// 断言用 index 顺序而不是行号：只是重排中间件就会红，改缩进/加注释不会误伤。
// 锚点找不到时**报失败而不是跳过** —— 重命名了中间件却让守卫静默失效，
// 比没有这条守卫更坏。
func TestRouter_ConcurrencyGatePlacement(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("读不到 router.go：%v（go test 的工作目录就是本包目录）", err)
	}
	text := string(src)

	idx := func(needle string) int {
		i := strings.Index(text, needle)
		if i < 0 {
			t.Fatalf("router.go 里找不到 %q —— 守卫的锚点已失效，请同步更新本测试"+
				"（锚点失效必须是响的，不能静默变成「查不到就当通过」）", needle)
		}
		return i
	}

	security := idx("r.Use(SecurityHeaders)")
	gate := idx("ConcurrencyGate(d.MaxInflightRequests")
	access := idx("r.Use(AccessLog(")

	if security >= gate {
		t.Error("ConcurrencyGate 必须在 SecurityHeaders **之内**（注册在其后）：" +
			"否则闸门回的 503 不带安全响应头，过载时的响应形状与正常响应不一致")
	}
	if gate >= access {
		t.Error("ConcurrencyGate 必须在 AccessLog **之外**（注册在其前）：" +
			"否则被拒请求会逐条写访问日志，过载时日志（磁盘）就是下一个被抽干的资源 —— " +
			"一场拒绝风暴能把服务从「降级但可用」打成「彻底不可用」")
	}
}

// TestConcurrencyGate_ReleasesSlotOnPanic 守住"panic 也要归还槽位"。
//
// 漏归还的后果不是"上限变松"，而是闸门被永久吃掉一格：几十次 panic
// （而 panic 恰好是 Recoverer 兜住的常规事件）之后闸门就永久卡死了。
func TestConcurrencyGate_ReleasesSlotOnPanic(t *testing.T) {
	h := ConcurrencyGate(1, time.Second)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { panic("boom") },
	))

	call := func() (panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
		return false
	}

	if !call() {
		t.Fatal("第一次调用应 panic（本测试故意不包 Recoverer）")
	}
	// 若 defer 没归还槽位，第二次会被闸门拒（503，不 panic）。
	if !call() {
		t.Fatal("第二次调用仍应 panic —— 说明 panic 后槽位没被归还")
	}
}

func TestConcurrencyGate_PanicsOnNonPositiveLimit(t *testing.T) {
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("上限为 %d 时应 panic，实际静默退化成'不限制'", n)
				}
			}()
			ConcurrencyGate(n, time.Second)
		}()
	}
}

// TestProbeExemptPaths_IsExactlyTheExpectedSet 是一条**静态**断言：
// 名单必须写死在测试里，不能遍历实现自己的切片/映射。
//
// 为什么：遍历 probeExemptPaths 来验证 probeExemptPaths，等于让白名单
// 给自己作证 —— 将来有人顺手往表里加一条（哪怕是内部的 /metrics），
// 遍历式断言会照样通过。只有把期望值写死，扩容才会让测试变红。
func TestProbeExemptPaths_IsExactlyTheExpectedSet(t *testing.T) {
	want := []string{"/healthz", "/readyz"}
	if len(probeExemptPaths) != len(want) {
		t.Fatalf("探针豁免表条目数为 %d，期望 %d：%v",
			len(probeExemptPaths), len(want), probeExemptPaths)
	}
	for _, p := range want {
		if _, ok := probeExemptPaths[p]; !ok {
			t.Errorf("探针豁免表缺少 %q", p)
		}
	}
}

// TestMapError_ServiceOverloadedIs503 钉住"容量信号"与"频率信号"的分野。
//
// 两者若都映射成 429，客户端会把容量问题当成自己的问题去退避重试，
// 而这个只有扩容才能解决的问题永远不会被上报。
func TestMapError_ServiceOverloadedIs503(t *testing.T) {
	status, _ := mapError(domain.ErrServiceOverloaded)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("ErrServiceOverloaded 应映射 503，实际 %d", status)
	}
}

// TestShouldLogAsError 守住"闸门拒绝不逐条记 Error"这条例外，
// 同时用**正控制**保证它没有退化成"一律不记"。
//
// 只测"过载被排除"是不够的：把 shouldLogAsError 写成恒 false 也能让那条绿，
// 而后果是所有 5xx 都不再留痕（真故障变成静默）。所以必须有"其它错误仍要记"的反向断言。
func TestShouldLogAsError(t *testing.T) {
	if shouldLogAsError(domain.ErrServiceOverloaded) {
		t.Error("闸门拒绝是预期内的背压，不该逐条记 Error（会造成日志风暴）")
	}
	// 正控制：普通 5xx 仍要留痕。
	for _, err := range []error{errors.New("boom"), domain.ErrRateLimitUnavailable} {
		if !shouldLogAsError(err) {
			t.Errorf("%v 应逐条记 Error 日志（否则真故障会静默）", err)
		}
	}
}
