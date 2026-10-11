# 性能剖析（pprof）

这份文档回答：**服务变慢时，怎么知道时间花在哪。**

它不讲"怎么变快"—— 优化方案要等 profile 出来之后才谈得上。
现在有的是一件工具：把进程的 CPU 使用拆开看。

---

## 1. 为什么它默认不存在

pprof 不是普通的只读端点。已实测它能给出什么：

| 端点 | 会吐出什么 |
| --- | --- |
| `/debug/pprof/heap` | 存活对象，按分配点归类。**参数内容可能出现在里面** |
| `/debug/pprof/goroutine?debug=2` | 每个协程的**完整栈**，含栈帧上的参数值 |
| `/debug/pprof/allocs` | 同heap，但看累计分配 |
| `/debug/pprof/mutex` / `block` | 锁竞争 / 阻塞时间（**默认不采样**，见第 4 节） |

而 Go 标准 pprof 还提供 `cmdline`—— 它会把整个进程命令行原样吐出。
本服务的命令行与启动环境里可能有 `DATABASE_URL`（含数据库密码）与 `JWT_SECRET`。

所以：**一个可访问的 pprof ≈ 一份进程内存快照。**

于是这里的默认状态是"关"。开启必须显式配 `PPROF_ADDR`，
而不是"默认开、想安全就记得关"。

---

## 2. 怎么开

### 开发机（推荐：走 dev-up.sh）

```bash
PROFILE_API=1 bash scripts/dev-up.sh --restart
```

之后：

- 剖析端点：`http://127.0.0.1:6060/debug/pprof/`
- 抓一份压测期间的 CPU profile：`bash scripts/profile-api.sh`

`--restart` 是必需的：环境变量在进程启动时读取，已在跑的实例不会因为
你设了变量就多出端口。脚本会单独确认剖析端口真的在应答 ——
**启动日志说开了不等于端口在听**。

### 手动起

```bash
PPROF_ADDR=127.0.0.1:6060 bash scripts/api.sh
```

### 生产

生产环境**必须绑回环**：

```bash
APP_ENV=prod PPROF_ADDR=127.0.0.1:6060 ./bin/api.exe serve
```

绑非回环地址（容器编排里让 sidecar 抓 profile）需要再显式声明：

```bash
ALLOW_PUBLIC_PPROF_IN_PROD=true PPROF_ADDR=0.0.0.0:6060
```

这个逃生门的存在理由与 `ALLOW_MOCK_PROVIDERS_IN_PROD` 一致：
真实场景需要它，但**必须让"我正在降低安全等级"这件事显式发生一次**。

---

## 3. 配置项

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PPROF_ADDR` | 空（关） | 监听地址。空 = 不起端口 |
| `PPROF_MAX_SECONDS` | 60 | 单次采集的秒数上限 |
| `ALLOW_PUBLIC_PPROF_IN_PROD` | false | 生产绑非回环的逃生门 |

### 为什么有 `PPROF_MAX_SECONDS`

Go 的 `net/http/pprof.Profile` 对 `seconds` 参数**没有上界**：
它是 `strconv.ParseInt` 之后直接拿去 `sleep` 的，
所以 `?seconds=99999` 会让一个 handler 线程占用 27 小时。

而 profiling 期间运行时持续采样，进程比平时慢一截——
一条无上限的端点等于**自我施加的 DoS**。

上限超限时返回 400 并在响应体里写明上限值，**不静默钳制**：
静默钳制会让运维请求 10 分钟、实际采到 1 分钟，
而报告里写的是"10 分钟 CPU 采样"—— 结论与请求不一致。

上限本身也有上界（600 秒）：配成 86400（"录一整天"）会让 profiling
几乎永久生效，而它的表现（服务变慢）不像错误、像负载问题，
排障方向会从一开始就错。要长时间采样就分多次抓。

---

## 4. 挂载了哪些 profile，以及那些"有洞"的

白名单是**逐条列举**的（`profiling.profiles`）：

```
profile  allocs  heap  goroutine  mutex  block  threadcreate
```

**刻意不挂**：`cmdline` / `trace` / `symbol`。它们都是 Go 标准五件套之一，
所以"顺手也挂上"是个极自然的错误——而挂上之后端点完全正常，
不会有任何报错。`cmdline` 泄露敏感信息，`trace` 开销大且能带出参数值。

### mutex 与 block 默认返回空数据

`runtime.SetMutexProfileFraction` 与 `runtime.SetBlockProfileRate`
的默认值都是 0，即**不采样**。不打开这两个开关时，这两个端点会返回
200 和一份空 profile。

这比 404 更坏：404 会让人去查，200 + 空数据会让人得出"没有锁竞争"的结论。
启动日志里会列出这两个名字及其原因，`profiling.BlockedProfiles()` 也能取到。

---

## 5. 抓一份压测期间的 profile

```bash
PROFILE_API=1 bash scripts/dev-up.sh --restart   # 起环境（含剖析端口）
bash scripts/profile-api.sh                       # 抓 30 秒 CPU profile
```

脚本会：起 k6 压测 → 等 3 秒进入稳态 → 抓 profile → 顺手抓一份 goroutine 全栈
→ 用 `go tool pprof -top` 解读。

产物在 `tools/loadtest/results/`：

| 文件 | 内容 |
| --- | --- |
| `cpu-<时间戳>.pb.gz` | CPU profile（`go tool pprof -http <file>` 看火焰图） |
| `goroutine-<时间戳>.txt` | goroutine 全栈 |
| `profile-load.log` | k6 压测日志 |

### 限流必须放宽

```bash
RATE_LIMIT_MAX=1000000 RATE_LIMIT_AUTH_MAX=1000000 bash scripts/api.sh
```

未登录的商品读取按 **IP** 建桶，默认 `120 次/分钟`。
压测一分钟就能打出几千次读，于是实测 ≈98% 的请求吃 429——
而 profile 里量到的是**限流器**，不是系统。

---

## 6. 已知的三个坑（都是实测踩的）

### 坑一：`pprof.Handler("profile")` 是 404

直觉写法是"遍历白名单，统一用 `pprof.Handler(name)` 注册"。
这个写法的失败形态极隐蔽：

| 端点 | 结果 |
| --- | --- |
| heap / goroutine / allocs / mutex / block | 200，且有数据 |
| **profile（CPU）** | **404 "Unknown profile"** |

原因：`pprof.Handler` 内部是 `pprof.Lookup(name)`，
而 `"profile"` 不是 `runtime/pprof` 里的伪 profile（`Lookup` 返回 nil）。
CPU profile 的实现在 `net/http/pprof.Profile` 里，必须单独挂。

而404 在一次仓促的排障里很容易被读成"这个端点没开"，
结论就变成"我们没有 CPU profiling 能力"。

**因此 `TestHandler_CPUProfileActuallyWorks` 的判据是"能否解出 CPU profile
的语义标记"（`samples` / `count` / `cpu` / `nanoseconds`），不是"状态码是否 200"。**

（第一版那条断言写的是"首字节 == 0x0A"，那是我猜的 protobuf魔数，
实测是 `0x48`。**写看起来很专业的魔数断言比不写更坏** —— 它会让人以为格式核对过了。）

### 坑二：`DefaultServeMux` 上**一定**有 pprof

不是只有 `import _ "net/http/pprof"` 才会注册。Go 1.27 的
`net/http/pprof` 有这么一段**无条件 init**：

```go
func init() {
    http.HandleFunc("GET /debug/pprof/", Index)
    http.HandleFunc("GET /debug/pprof/cmdline", Cmdline)
    http.HandleFunc("GET /debug/pprof/profile", Profile)
    http.HandleFunc("GET /debug/pprof/symbol", Symbol)
    http.HandleFunc("GET /debug/pprof/trace", Trace)
}
```

**具名导入同样执行 init。** 本包需要 `pprof.Profile` / `pprof.Handler` /
`pprof.Index`，所以只要编译进这个二进制，`DefaultServeMux` 上就一定有这五条。

写成"不该存在"的守卫只会得到一个永久红的测试。真正的防线是
"这些注册永远不会被服务出去"—— 唯一的暴露路径是某处
`ListenAndServe(addr, nil)`（`nil` 的字面含义就是"用 DefaultServeMux"），
而那行代码看起来与安全毫无关系。

### 坑三：不带 `seconds` 的 profile 会真的采 30 秒

Go 的默认值是 30 秒，而 **CPU profile 是全进程唯一的**
（`StartCPUProfile` 同时只能有一个）。所以：

- 测试里打一次 `/debug/pprof/profile` 不带 `seconds` ⇒ 真的睡 30 秒
- 第一次跑这个包的测试时直接 **SIGTERM**（反复叠加）
- 修正后从 40 秒降到 10 秒

而且"只读状态码不读 body 就不会采集"是**错的**：
采集发生在 handler 内部，与调用方读不读 body 完全无关。

---

## 7. 怎么读一份 profile

1. 先看 flat top 里 **`net/http` 之外的项**。HTTP 栈的开销是固定的，
   它高不代表有问题—— 要看的是**谁在 HTTP 栈之下**。
2. 重点怀疑三个（本项目的具体候选）：
   - `encoding/json`：oapi-codegen 的编解码 + 响应组装
   - Redis 往返：限流器的 Lua 脚本每个请求跑一次
   - 日志写盘：`AccessLog` 每个请求写一行
3. flat 里pgx/pgxpool 占大头 ⇒ DB 侧；
   goroutine 大量停在 chan/select ⇒ 限流器的 Redis 往返在等。
4. **不要**拿这份 profile 去论证"该上分库分表"——
   10k req/s 的量级下分库是零收益，先找到那 100µs 的归属。

---

## 8. 与容量探底的关系

`docs/capacity-probe.md` 测出闭环读接口饱和在~10,000 req/s，
即每请求约 **100µs** 的独占资源时间。已排除连接池大小、PG 饱和、压测机。

**那 100µs 归属何处，在接上 pprof 之前是不知道的**——
而不知道构成就动手优化，会改到那个占大头的地方，也可能什么都改不到。

pprof 接上之后的第一件事就是抓一份压测期间的 profile，
把那100µs 拆成"JSON 编解码 / Redis 往返 / 中间件链 / PG 查询"四段的占比。

---

## 9. 实测结果：那 100µs 归属何处（2026-10-11）

**结论：限流器的 Redis Lua 往返是主要瓶颈，占 CPU 累计的 35.6%。**

### 采集条件

| 项 | 值 |
| --- | --- |
| 压测 | 300 VU 闭环无 sleep，k6 |
| 采样 | 30 秒 CPU profile（`tools/loadtest/results/cpu-20261011-133155.pb.gz`） |
| 该轮实测吞吐 | **9,879 req/s** —— 与探底测出的 10k 吻合，采到的正是饱和状态 |
| 环境 | 24 逻辑核 / 16GB，PG 与 Redis 在同机 Docker |

### CPU 分布（`go tool pprof -top -cum`）

```
57.08%  net/http.(*conn).serve          ← HTTP 栈本身（固定开销，不算问题）
42.78%  chi.(*Mux).ServeHTTP            ← 中间件链整体
35.63%  httpapi.rateLimitGate           ← ★ 限流门禁
22.99%  chi.(*Mux).routeHTTP            ← 真正的业务 handler
```

拆开限流门禁内部：

```
35.63%  rateLimitGate.func1.1
11.70%  ratelimit.(*Limiter).Decide
10.54%  redis.(*Script).Run
10.52%  redis.(*Script).EvalSha         ← 就是这次 Lua EVAL
```

而业务侧（`repo.*` + `service.*`）合计只有 **19.45%**：

```
12.55%  service.ListProducts
 6.47%  repo.ListProducts
 6.03%  repo.CountProducts
 3.50%  repo.GetProductByID
 3.38%  repo.ListSkusByProduct
```

**限流器一个中间件的开销（35.6%）比整个业务逻辑（19.5%）还大。**

goroutine 快照给出同样的答案 —— 大批协程停在
`ratelimit.go:294` → `redis/scripting_commands.go` → `EVAL`。

### 对照实验：关掉限流重测

profile 说明"限流占 35.6% CPU"，但**归因不等于因果**。
所以起一个 `RATE_LIMIT_ENABLED=false` 的实例跑同样负载：

| 场景 | 吞吐 | p99 | 错误率 |
| --- | ---: | ---: | ---: |
| 限流开（300 VU） | 9,879 req/s | 47.71ms\* | 0% |
| **限流关（300 VU）** | **13,171 req/s** | **36.98ms** | 0% |
| 差异 | **+33.3%** | **−22.5%** | — |

\* 取探底同档位数据；本轮 profile 那次的 p99 见 `profile-load.log`。

**吞吐涨三成、尾延迟降两成，而负载完全相同** ——
这坐实了：那 100µs 的主要去向就是限流器每个请求一次 Redis Lua 往返。

### 这意味着什么

**先别动手。** 已排除的选项与新出现的选项：

| 方向 | 判断 |
| --- | --- |
| 分库分表 | **仍然零收益**。10k→13k 的量级离需要它差着数量级 |
| 加机器 | **有效**。瓶颈在每个请求的串行往返上，横向扩展线性有效 |
| 限流器本身 | **值得优化**，但要小心 —— 它是准入控制，不是可以随手摘掉的中间件 |
| 读路径缓存 | **仍然成立且现在更有依据**：缓存命中同时省掉限流往返与 DB 查询 |

限流器的优化方向（**都还没做，需另行评估**）：

1. **本地预筛**：`RATE_LIMIT_MAX` 很大时，绝大多数请求其实远未触限。
   在进程内先用一个滑动窗口计数，只在**接近上限**时才问 Redis ——
   200 个客户端共享 1 万配额时，几乎所有请求都能本地判定。
   代价：多实例下每个实例各判一次，实际阈值是 N×配置值（需要可接受）。
2. **放宽读接口的限流**：匿名读按 IP 建桶，默认 120/min 本身就与
   正常浏览行为不匹配（这是压测一直要放宽限流的原因）。
3. **只对写/认证类走 Redis**，读路径靠缓存而不是靠限流器。

三条都涉及安全语义，**不是纯性能改动**，需要单独设计与验证。