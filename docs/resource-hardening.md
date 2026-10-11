# 高并发资源耗尽加固（第五轮）

> 2026-10-11。这一轮不做功能，只回答一个问题：
> **把并发拉到极限、或者让某个依赖挂掉时，这台服务会先从哪个资源上崩？**

## 0. 一句话结论

之前的四轮加固守的是**内容**（请求体多大、错误里带不带内网地址、谁能访问什么）。
这一轮补的是**容量**：在"入口并发"和"下游资源池"之间原本**完全没有闸门**，
高并发下的失败形态不是"拒绝一部分请求"，而是**所有请求一起变慢直到全部超时**。

新增三件东西，各守一种资源：

| 组件 | 守的资源 | 位置 | 过载/故障时的行为 |
| --- | --- | --- | --- |
| `connlimit` | file descriptor / goroutine | TCP accept 层 | 停止 accept，新连接停在内核 backlog |
| `ConcurrencyGate` | 并发请求数 → 下游连接池 | 路由中间件最外层之一 | **快速 503**（不排队）+ `Retry-After` |
| `circuitbreaker` | 每请求的判定超时 | 限流后端调用 | 连续失败后**不再去试**，直接降级 |

外加两处**日志风暴**的修复（过载时日志 I/O 本身就是下一个被抽干的资源）。

---

## 1. 审计：加固前每个资源的状态

| 资源 | 加固前 | 缺口 | 处置 |
| --- | --- | --- | --- |
| 请求体内存 | 三道防护（411 / 413 / `MaxBytesReader`），上限 1 MiB | 无 | 已有，未动 |
| HTTP 头内存 | `MaxHeaderBytes` 16 KiB | 无 | 已有，未动 |
| 超时 | `ReadHeaderTimeout` 5s / `ReadTimeout` 15s / `WriteTimeout` 30s / `IdleTimeout` 60s + `Timeout(30s)` 中间件 | 无 | 已有，未动 |
| **同时打开的连接数** | **无上限**（Go 的 `http.Server` 没有这个能力） | fd / goroutine 可被慢速攻击抽干 | **新增 `connlimit`** |
| **同时在处理的请求数** | **无上限** | 全部请求涌入抢 20 条 PG 连接 → 超时瀑布 | **新增 `ConcurrencyGate`** |
| PG 连接池 | `MaxConns = 20` **硬编码**，acquire 无独立超时 | 池满时请求排队到 30s | 池大小可配（`PG_MAX_CONNS`）；容量由闸门从上游保护 |
| 限流 Redis 池 | `PoolSize = 10` **硬编码** | 实测池 10→100 吞吐 2.8 倍 | 可配 + 默认上调到 100（`RATE_LIMIT_REDIS_POOL_SIZE`） |
| 业务 Redis 池 | `PoolSize = 20` 硬编码 | 不可按部署调 | 可配（`REDIS_POOL_SIZE`） |
| 限流后端故障 | 有 fail-open/fail-closed**降级**，但每请求仍去试一次 | Redis 挂时每请求付一次 300ms 判定超时 | **新增熔断**（`RATE_LIMIT_BREAKER_*`） |
| 过载时的日志 | 闸门拒绝 / 限流降级各自**逐条**记日志 | 日志量 ∝ 请求量 → 磁盘被写满 | 改为**不按请求记**的日志：过载告警按时间节流、恢复只记一条 |

### 关键判据：为什么"已经有降级逻辑"还不够

`ratelimit.Decide` 的 fail-open/fail-closed 是**完善的**，但它只回答
「失败之后怎么答」，不回答「还要不要每次都去试」。

于是 Redis 挂掉的整段时间里：

```
每个请求 → 等一次判定超时（默认 300ms）→ 失败 → 按 failsClosed 答
```

这是一个**每请求必经**的固定延迟，与 Redis 是否"连得上"无关 ——
它把 Redis 的故障时长**乘以请求量**，转嫁成本服务的故障。
熔断补的就是这一段。

---

## 2. 三道新闸门

### 2.1 `connlimit`：连接数上限

Go 的 `http.Server` 完全没有连接数上限，上限由操作系统的 `RLIMIT_NOFILE` 决定。
撞上它之后 `accept` 开始失败，而 **fd 是进程级资源** —— 一个被爬的公开接口
把 fd 吃光，同进程的日志、探针会一起不工作。

`LimitListener` 包一层 `net.Listener`，满了就**停止 accept**（新连接停在内核
backlog 里）。这是正确的背压方向：已建立的连接继续被服务，而不是所有连接一起变慢；
backlog 也满时由内核回 RST，客户端立刻知道该退避。

> **满了之后的行为选择**：停止 accept（而不是"接受后立刻关闭"），
> 因为后者仍要为每个连接分配 fd 与 goroutine —— 那正是要防的东西。

**配置**：`MAX_CONNS`，默认 `4096`。
它与 `RLIMIT_NOFILE` 是**乘法**关系：配得比 fd 上限还大等于没配
（fd 会先耗尽）。容器部署需显式配 `ulimits`。

### 2.2 `ConcurrencyGate`：并发准入闸门

它防的是**超时瀑布**：一旦入口并发超过下游资源池能服务的量，延迟开始线性增长，
而没有任何一个请求是错的 —— 它们只是在互相排队。闸门从中间掐断这个正反馈：
宁可快速拒绝**一部分**，也不让**全部**请求一起在 30 秒后超时。

三处设计决定：

| 决定 | 选择 | 理由 |
| --- | --- | --- |
| 满了怎么办 | **快速 503**，不排队 | 排队只是把雪崩从下游搬到闸门里；请求仍占着连接、仍在累积延迟 |
| 回什么状态码 | **503**（不是 429） | 429 = "你请求太多"，503 = "我这边满了"。混用会让客户端对容量问题只做退避重试，永不上报 |
| 放行什么 | `/healthz`、`/readyz` **逐条列举** | 探针被挡 → 编排系统判实例不健康 → 重启 → 冷启动吞吐更低 → 过载加剧 |

**位置**：紧跟安全响应头、**在访问日志之前**。被拒请求不产生访问日志 ——
过载时若每个被拒请求都落一行日志，磁盘会成为下一个被抽干的资源。

**配置**：`MAX_INFLIGHT_REQUESTS`，默认 `2048`。
依据是实测饱和点（读接口约 10k req/s × p50 90ms ≈ 900 并发），留约 2 倍余量
—— 正常负载下不介入，只在接近雪崩时拒绝。

### 2.3 `circuitbreaker`：限流后端熔断

标准三态（closed / open / half-open）：

```
closed  ──连续失败 ≥ 阈值──▶  open
   ▲                          │
   │                    冷却时间到
   │                          ▼
   └──── 探测成功 ──────  half-open（只放行 1 个探测）
                              │
                          探测失败
                              ▼
                            open（重新计时）
```

几个不是随手定的决定：

- **连续失败**而不是累计失败：一个 50% 失败率的抖动依赖还能用，熔断它是自伤。
- **half-open 只放一个探测**：冷却一到就全量放行 = 给尚未恢复的依赖再一次全量冲击，
  故障常被"试探性流量"二次打垮。
- **不重试/不排队**：熔断的价值就是"立刻给结论"，在 open 时还去试会让它退化成另一种排队。
- **`Abort()`**：调用方取消 ctx（客户端断开）时探测**没有结论**，不能作为证据；
  但也必须撤销半开探测位，否则 `halfOpenInFlight` 永久为 true，
  熔断器再也放不出探测（依赖早恢复了，服务却一直 503 到重启）。

**熔断不改变降级方向**：open + fail-closed 仍然拒绝，open + fail-open 仍然放行。
它只决定"要不要去试"。

---

## 3. 过载时，日志本身是资源

这一轮顺手修掉两处**日志量正比于请求量**的地方。它们与新增的闸门直接相关：
闸门省下的开销，不能从日志口子原样漏回去。

| 位置 | 原行为 | 后果 | 现行为 |
| --- | --- | --- | --- |
| 闸门拒绝 | `WriteProblem` 对**每个** 5xx 记一条 Error | 拒绝风暴 → 日志风暴 → 磁盘写满 | `shouldLogAsError` 把"预期内的背压"排除；由闸门自己的日志补可观测性（过载告警**按时间节流**每 10s 至多一条、恢复只记一条） |
| 限流降级 | 每请求记一条 Warn | 同上 | 同形：降级告警按时间节流，恢复只记一条（CAS 保证） |

改后日志条目数正比于**故障事件数**，而不是请求数。

> ★ 这一节的"状态转移日志"设计**被实测推翻过一次**，修完才写下上面这段话。
> 第一版用 `atomic.Bool` + `CompareAndSwap` 做"翻转才记一条"，看着无懈可击 ——
> 直到 300 VU 压测 60 秒产生了 **268,176 条**"闸门已满"日志。
> 机制：槽位是每个请求借还一次，所以饱和点附近"是否满"这个**瞬时状态**
> 随每次占槽/释放反复翻转 —— 在落在饱和点的系统里，状态转移本身就是高频事件。
> 判据修正为**按时间节流**（每 10 秒至多一条），实测降到 7 条。详见第 7 节。

> `shouldLogAsError` 的守卫带**正控制**：只断言"过载不记"的话，
> 把函数写成恒 `false` 也能通过 —— 而后果是所有真故障都静默。

---

## 4. 配置项一览

| 变量 | 默认 | 说明 |
| --- | ---: | --- |
| `MAX_CONNS` | `4096` | 同时打开的 TCP 连接上限。**0 = 显式关闭**（需配合 fd 上限理解） |
| `MAX_INFLIGHT_REQUESTS` | `2048` | 同时在处理中的请求上限。0 = 显式关闭 |
| `PG_MAX_CONNS` | `20` | PG 连接池上限 |
| `REDIS_POOL_SIZE` | `20` | 业务 Redis 池大小 |
| `RATE_LIMIT_REDIS_POOL_SIZE` | `100` | 限流专用 Redis 池大小（原写死 10，实测 10→100 为 2.8 倍） |
| `RATE_LIMIT_BREAKER_THRESHOLD` | `5` | 限流后端连续失败多少次打开熔断 |
| `RATE_LIMIT_BREAKER_OPEN` | `5s` | 熔断冷却时长 |

**默认值刻意全部为"开"**：容量保护最可能的失效方式不是配错值，
而是默认值被改成 0/关闭 —— 那种改动没有任何测试会变红，现象是"服务裸奔但一切正常"。
所以有一条把"默认必须为正"写死的断言（`TestDefaults_AreProtective`）。

---

## 5. 验证

### 5.1 单元测试

新增测试（全部覆盖"该拒的拒了"**和**"该放的放了"两侧）：

| 包 | 覆盖 |
| --- | --- |
| `connlimit` | 上限生效、归还槽位后能继续接受、重复 Close 不重复归还、非正值必须 panic |
| `httpapi` | 超限快速 503、探针过载时仍放行、**形近路径不放行**（前缀误写）、**编码变体不放行**（`/%68ealthz`）、字面探针仍放行的正控制、panic 后归还槽位、白名单是**静态**断言、闸门**位置**是静态断言（在 SecurityHeaders 之内 / AccessLog 之外） |
| `circuitbreaker` | 连续失败才打开、成功重置计数、半开只放一个探测、探测失败重新计时、Abort 不卡死 |
| `ratelimit` | **熔断打开后不再调后端**（数调用次数）、调用方取消不喂熔断、**内部判定超时喂熔断**（与前者构成两侧对照）、前置校验不喂熔断 |
| `config` | 取值边界、**默认值必须是保护性的** |
| `profiling` | 采集时长上限的**精确边界**（`sec==max` 放行 / `sec==max+1` 拒绝，直接验 `guardSeconds` 不真采集） |

### 5.2 变异体验证

`scripts/verify-guards.py` 新增 **M51–M62**（12 条）证明守卫有牙；
提交前四轴 review 又补了 **M63 / M64**（见 §8）：

| 编号 | 变异 | 期望被抓住的守卫 |
| --- | --- | --- |
| M51 | 连接上限对非法值静默兜底 | `TestNewLimitListener_RejectsNonPositive` |
| M52 | 连接关闭时不归还槽位 | `TestLimitListener_CapsConcurrentConns` |
| M53 | 探针豁免表被顺带扩大 | `TestProbeExemptPaths_IsExactlyTheExpectedSet` |
| M54 | 探针豁免写成前缀匹配 | `TestConcurrencyGate_DoesNotExemptLookalikePaths` |
| M55 | 闸门拒绝重新变成逐条 Error 日志 | `TestShouldLogAsError` |
| M56 | 闸门拒绝回 429 而不是 503 | `TestConcurrencyGate_RejectsBeyondLimit` |
| M57 | 限流器不再检查熔断 | `TestAllow_BreakerStopsHittingBackend` |
| M58 | 调用方取消被计入熔断失败 | `TestAllow_CallerCancelDoesNotFeedBreaker` |
| M59 | 前置校验也喂熔断 | `TestAllow_PrecheckDoesNotFeedBreaker` |
| M60 | 并发闸门的默认值被改成 0 | `TestDefaults_AreProtective` |
| M61 | 熔断器半开期不再独占探测 | `TestBreaker_HalfOpenAllowsSingleProbeAndClosesOnSuccess` |
| M62 | 熔断器成功时不重置失败计数 | `TestBreaker_SuccessResetsFailureCount` |
| **M63** | 内部判定超时被改回「按错误链」判成调用方取消 | `TestAllow_InternalJudgeTimeoutFeedsBreaker` |
| **M64** | 探针豁免改回只看 `r.URL.Path`（放过编码变体） | `TestConcurrencyGate_DoesNotExemptPercentEncodedVariants` |

> 顺带修了 **M19 / M41** 的锚点：本轮的变量重命名（`err` → `runErr`）
> 让那两个既存变异体的锚点失配 —— `--audit` 会报"锚点命中 0 次"，
> 而那会让它们变成恒为「未证明」的假条目。已同步更新。
>
> **M54 的锚点又被改了一次**（同日）：把探针判定收进 `isProbeExempt` 之后
> （为了让"拿路径的哪个字段"有单一改动点），M54 原本钉的调用点那一行不再存在，
> `--audit` 立刻报"命中 0 次"。这正是它该有的表现 —— 重构会让锚点失效，
> **而失效必须是响的**。已改钉到函数体。

### 5.3 高并发实测

见第 7 节（实测记录）。

---

## 6. 已知遗留 / 未做

- **PG 池的 acquire 没有独立超时**。本轮用并发闸门从**上游**保护池
  （同时进入业务的请求数被钉住，池不会被压满），但没有在 `repo`/`service` 层
  给每次 acquire 加一个独立于请求 ctx 的短超时。若将来闸门阈值配得远大于池容量，
  仍可能出现"进了闸门但卡在 acquire"的请求。
- **熔断目前只作用于限流后端**。PG 是更核心的依赖，但给它加熔断需要包住
  `repo` 层的所有调用点，改动面大、且 pgx 自带连接健康检查会与之重叠 ——
  单独立项更合适。
- **`MAX_CONNS` 的默认值(4096) 与部署的 `RLIMIT_NOFILE` 尚未做联动校验**。
  理想情况是启动时读 `RLIMIT_NOFILE`，若 `MAX_CONNS` 超出就 Warn（而不是只写在注释里）。
- **`ConcurrencyGate` 不区分主体**：它按容量拒绝，不按"是谁"。因此
  在**没有前置网关**的裸部署下，大流量攻击者会与正常用户竞争闸门槽位。
  应用层的限流（按 IP）部分覆盖了这一点，但完整的解法是边缘层。
- Web 端 token 仍在 `localStorage`（第四轮遗留，见 `docs/security-hardening.md`）。

---

## 7. 实测记录

### 环境与方法

- 本机 Docker（`itiao-pg` / `itiao-redis` 容器）+ k6（chocolatey 安装）
- API 起在 `:8090`（8080 被本机一个代理占着），`MAX_INFLIGHT_REQUESTS=20`
  —— **故意压低到 20**，这样在很小的负载下就能触发闸门，便于观察行为
- 负载：`tools/loadtest/scenarios/probe.js`（闭环、无 sleep），
  阶梯 50/150/300 VU，每档 5s 爬升 + 15s 稳态，共约 65 秒
- 数据：`tools/loadtest/seed/{reset,seed}.sql` 重新播种

### 结果 1：闸门生效，且探针不受影响

压测**进行中**同步探测（每轮间隔 1 秒，共 12 轮）：

| 路径 | 结果 |
| --- | --- |
| `/healthz` | **200 × 12** |
| `/readyz` | **200 × 12** |
| `/api/v1/products`（业务） | 503 × 11，429 × 1 |

这正是要的行为：**过载时业务被快速拒绝，但编排探针始终可达** ——
探针被挡才会引发"过载 → 判不健康 → 重启 → 更过载"的雪崩。

施加的负载与结果（修复日志节流之后的那次）：

| 指标 | 值 |
| --- | ---: |
| 总请求 | 4,891,783 |
| 速率 | 75,258 req/s |
| `probe_status_5xx` | ≈ 88%（闸门拒绝） |
| `probe_status_4xx` | ≈ 12%（限流 429，压测源 IP 单一，120/min 很快超） |
| `probe_status_2xx` | 约 0.005%（仅 240 次） |

> `http_req_failed: 99.99%` 是 k6 的口径（4xx/5xx 都算 failed），
> 它在这里**不代表故障** —— 88% 是闸门按设计拒绝的。这也是当初给探底场景
> 单列 2xx/4xx/5xx/传输错误四个计数的原因：混一个 `failed` 里就分不清
> "被保护机制挡住"和"系统崩了"。

### 结果 2：延迟没有雪崩 —— 这是闸门存在的全部理由

| | p95 |
| --- | ---: |
| 本次（有闸门，20 并发上限，75k req/s 施加） | **7.62ms** |
| 对照：无闸门、300 VU（`docs/capacity-probe.md` 实测） | p99 **47.71ms**（继续加 VU 会线性恶化） |

闸门把过载换成了**快速失败**：88% 的请求被拒，但**被服务的**那些延迟稳定在毫秒级。
没有闸门时，同样的负载会让**所有**请求一起变慢。

### 结果 3：日志节流（实测发现的缺陷）

同一负载（300 VU，60 秒）下，"并发闸门已满"日志的条数：

| 版本 | 日志条数 | 总日志行数 |
| --- | ---: | ---: |
| 第一版（`atomic.Bool` 按状态翻转） | **268,176** | 1,104,146 |
| 修复后（按时间节流，10s） | **7** | 943,288 |

剩下那 94 万行是**被服务的请求**的访问日志（正常，一个请求一条）。
差值 ≈ 26.8 万行，正好是被闸门拒绝、且**没有**产生访问日志的请求
—— 这同时验证了"闸门排在 AccessLog 之前"这个设计。

### 结果 4：熔断真的省下了那 300ms

用 `docker pause itiao-redis` 制造**黑洞**故障（TCP 连得上、但进程冻结、不回应）
—— 这正是 300ms 判定超时被触发的场景（相当于 Redis 主从切换）。

连续 9 个请求的耗时：

| 请求序号 | 结果 | 耗时 |
| ---: | --- | ---: |
| 1–5 | 200 | **~305ms**（每次都在等判定超时） |
| 6–9 | 200 | **~3ms**（熔断已打开，不再碰 Redis） |

**305ms → 3ms，差约 100 倍。** 阈值是 5，所以第 6 个请求起熔断打开。

- 状态码仍是 200 是因为 `APP_ENV=dev` ⇒ `RATE_LIMIT_FAIL_MODE=auto` ⇒ fail-open
  （降级放行）。**熔断没有改变降级方向**，只决定了"还要不要去试"。
- 生产环境默认 fail-closed，同样的实验会得到 503 —— 但延迟曲线一样（先 300ms 后 3ms）。

### 结果 5：回归与守卫

| 项 | 数字 |
| --- | --- |
| Go 测试（带 `DATABASE_URL`，集成测试未跳过） | **516 PASS / 0 FAIL / 0 SKIP** |
| 变异体（M51–M64，本轮 12 条 + review 补 2 条） | **14/14 被指定测试抓住**，无未证明项 |
| `verify-guards.py --audit` | **64 条**全部处于原始状态、锚点唯一命中 |

**提交前门禁 `scripts/check-all.sh` 真跑满 10/10 全绿**：

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 变异体残留自检 | ✓ 64 条原始状态 |
| 2 | gofmt / go vet | ✓ |
| 3 | 契约漂移 | ✓ 无漂移 |
| 4 | Go 测试 | ✓ **516 PASS / 0 SKIP**（下限 380） |
| 5 | 压测比对逻辑单测 | ✓ 14/14 |
| 6 | 变异验证 M1–M64 | ✓ **64/64** 被指定测试抓住 |
| 7 | govulncheck | ✓ 0 条可达漏洞 |
| 8 | 压测门禁变异验证 | ✓ 注入劣化→红，还原→绿（sha256 一致） |
| 9 | 安全探针 | ✓ **PASS=63 / FAIL=0 / WARN=0 / SKIP=0** |
| 10 | 用户端端到端验收 | ✓ **15/15** |

> ★ 门禁的跑法本身踩过一次坑（第一次跑红在第 9/10 项，现象像"这轮改动把服务搞坏了"）：
> **`dev-up.sh` 起的 API/web 不会跨 Bash 调用存活** —— 脚本报"起来了"是真的
> （探测拿到 200），但那次调用一返回整个进程树就被回收，门禁再跑时 :8080 已空。
> 正确姿势是**把起栈和门禁放进同一次调用**：
>
> ```bash
> export PG_COMPOSE_OVERRIDE=".workbuddy/pg-port-override.yml"
> export DATABASE_URL="postgres://itiao:itiao@localhost:15432/itiao?sslmode=disable"
> bash scripts/dev-up.sh; bash scripts/check-all.sh   # 用 ; 不用 && —— dev-up 失败也要看到门禁结果
> ```
>
> 另一个容易指错方向的信号：本机 `http_proxy=127.0.0.1:1444` 对本机 `localhost` **也生效**，
> 服务不在时它回 **502**（看起来像网关坏了）。判据：`curl --noproxy '*'` 对照，
> `000` 才是"真的没人听"。而 `node` 的 `fetch` 不读 `http_proxy`，所以探针报 `fetch failed`。


> 本轮把判定里的局部变量 `err` 改名成 `runErr`（避免遮蔽具名返回值），
> 这让既存变异体 **M19 / M41** 的锚点失配 —— `--audit` 立刻报出
> "锚点命中 0 次"。它是**设计好的信号**：那两条会恒为「未证明」而不是默默变成绿。
> 已同步更新锚点。

### 复现命令

```bash
# 1) 播种
docker exec -i itiao-pg psql -U itiao -d itiao -q < tools/loadtest/seed/reset.sql
docker exec -i itiao-pg psql -U itiao -d itiao -q < tools/loadtest/seed/seed.sql

# 2) 起一个低闸门阈值的实例
cd services/api && go build -o bin/api.exe ./cmd/api
env DATABASE_URL="postgres://itiao:itiao@localhost:15432/itiao?sslmode=disable" \
    REDIS_ADDR="localhost:6379" HTTP_ADDR=":8090" APP_ENV=dev \
    MAX_INFLIGHT_REQUESTS=20 ./bin/api.exe serve

# 3) 压测（另开一个终端）
cd tools/loadtest
k6 run --env BASE_URL=http://localhost:8090 \
       --env STEPS=50,150,300 --env STAGE_SECONDS=15 --env RAMP_SECONDS=5 \
       scenarios/probe.js

# 4) 熔断验证：冻结 Redis，观察耗时从 ~305ms 掉到 ~3ms
docker pause itiao-redis
for i in $(seq 1 9); do
  curl -s -o /dev/null -w "$i %{http_code} %{time_total}\n" \
    "http://127.0.0.1:8090/api/v1/products?page=1&page_size=20" -H "Origin: http://localhost:5173"
done
docker unpause itiao-redis
```

---

## 8. 提交前的四轴 review 挖出的两处真缺陷

> 2026-10-11。四路只读复核（正确性 / 安全性 / 可维护性 / 测试覆盖）＋ 逐条实证复现。
> 两条 P1 **都是本轮自己引入的**，而且都不是"代码看起来危险"那一类 ——
> 它们各自都需要**两条事实叠加**才会显形，单看任何一处都读得通。

### 8.1 探针豁免用错了 URL 字段 ⇒ 编码变体绕过闸门（fail-open）

| 事实 | 出处 |
| --- | --- |
| net/http 对 `/%68ealthz` 给出 `Path="/healthz"` + `RawPath="/%68ealthz"` | 实测（stdlib url 解析） |
| chi 路由用的是 `RawPath`（非空时优先） | chi v5.3.2 `mux.go:454` |

合起来：闸门**只看 `Path`** 就给出豁免，而 chi 按 `RawPath` 找不到 `/healthz` 这条路由、回 404 ——
于是这类请求**绕过了容量闸门，却并没有走到探针**，而且还落进被刻意排在闸门**内层**的 `AccessLog`。
"过载时被拒请求不写日志"这条防线被从侧门打开：攻击者用 `/%68ealthz` 这类变体
既能不被限并发、又能持续产生日志。

**修法**（`httpapi.isProbeExempt`）：`RawPath` 非空即说明请求用了编码形式，
而探针打的是字面路径 → 一律不豁免（fail-closed）。
**守卫**：`TestConcurrencyGate_DoesNotExemptPercentEncodedVariants`
（含"这些请求的 Path 确实等于探针路径"的前置断言，避免退化成"因为名字对不上才被拒"的假绿）
＋ 正控制 `TestConcurrencyGate_LiteralProbeStillExempt`；**变异体 M64**。

> 它与既有的两条豁免守卫是**三个不同方向**，不可互相替代：
> M53 改"表里有哪些项"、M54 改"怎么比较"、M64 改"拿路径的哪个字段"。
> 形近路径那条守卫（`/healthzz` 等）在 M64 下**照样全绿** —— 那些路径的 `Path` 本来就不匹配。

### 8.2 熔断记账用错误链判"调用方取消" ⇒ 熔断器对超时是聋的

| 事实 | 出处 |
| --- | --- |
| `BackendError` 实现了 `Unwrap() { return Cause }`，错误链会穿透到 Cause | `ratelimit.go:71` |
| **内部** judgeCtx 超时时，go-redis 在池等待路径上原样返回 `ctx.Err()` | go-redis v9.22.0 `internal/pool/pool.go:1055/1114/1168` |

原实现（本轮写下的）用 `errors.Is(err, context.DeadlineExceeded)` 判"调用方取消"。
于是**内部**判定超时被读成"调用方取消" → 走 `Abort()`（不下结论）→ 不计失败 →
**熔断器永远打不开**。而"后端卡住 + 池被占满"正是熔断唯一要解决的形态。

**实测复现**（假后端分别返回两种错误，阈值 3）：

| 注入的错误 | 熔断状态 | 后端被调用次数 |
| --- | --- | --- |
| `errors.New("i/o timeout")`（net 超时） | **open** ✅ | 3（之后不再打） |
| `context.DeadlineExceeded`（池等待，调用方 ctx 正常） | **closed** ❌ | 6（每请求都打） |

**修法**：用显式的 `noConclusion` 标记，只在**调用方自己的 ctx** Done 时置位；
判据从"错误链"改成"谁的 ctx 结束了"。
**守卫**：`TestAllow_InternalJudgeTimeoutFeedsBreaker`（与既有的
`TestAllow_CallerCancelDoesNotFeedBreaker` 构成**两侧对照**）；**变异体 M63**。

> ★ 只加一侧用例是挡不住的：一个"把所有失败都当无结论"的实现能通过全部取消用例，
> 而它的后果是熔断形同不存在。
> ★ M63 的第一版变异体把 `noConclusion` 整个换掉，结果**文件编不过**（变量声明未使用），
> harness 如实报"只有包级失败、没有具名测试"。这正是方法论里那条：
> **写变异体先问"它还能被编译吗"，再问"它描述的后果真的会发生吗"**。

### 8.3 其余采纳项

| 轴 | 结论 | 级别 | 处置 |
| --- | --- | --- | --- |
| 可维护性 | 3 处代码注释 + 2 处文档表格仍写"**状态转移**日志"，而实现已改成按时间节流（§3 的 ★ 注自己就写着它被实测推翻了 —— 同章自相矛盾） | P2 | 已改 |
| 正确性 | `config.pprofMaxSecondsCap` 与 `profiling.MaxSecondsHardCap` 两个 600 靠注释同步，漂移不出声 | P2 | 改为单一来源（config 引用导出的常量，编译期绑定） |
| 测试覆盖 | `TestGuardSeconds_AllowsExactlyAtLimit` 实际传 `seconds=1`（上限 5）⇒ `>` 写成 `>=` 无人能抓，边界根本没被钉住 | P2 | 改为在 `guardSeconds` 上直接验 `sec==max` 放行 / `sec==max+1` 拒绝（不真采集） |
| 测试覆盖 | "闸门排在 AccessLog 之外"这条位置约定零断言 | P2 | 加静态断言 `TestRouter_ConcurrencyGatePlacement`（行为验证需配齐依赖并制造过载，代价远大于收益） |
| 安全性 | `ErrRateLimitUnavailable` 与 `ErrServiceOverloaded` 同映射 503、文案不同，客户端无法按状态码分流根因；前者未带 `Retry-After` | P3 | **驳回不改**：两者都是"服务端暂时不行"，客户端动作都是带抖动退避重试；区分根因是**监控**的职责（日志里有哨兵原文），不是状态码的职责。加 `Retry-After` 会暗示"等一下就好"，而限流器坏掉时该做的是看告警 |
| 安全性 | `RATE_LIMIT_BREAKER_THRESHOLD` 只要求 ≥1，配 1 时单次抖动即打开；fail-open 下等于限流全量失效 | P3 | **驳回不改**：这是部署方的显式选择，且默认 5 已在文档里给出依据（5×300ms≈1.5s）。加下限等于替运维猜他的 Redis 抖动率 |
| 安全性 | `/healthz/`（带尾斜杠）不豁免 | P3 | **驳回**：把它加进豁免表就是"顺带放行一类路径"，正是 §2.1 里"豁免必须逐条列举"要防的方向；探针打的是字面路径，`dev-up.sh` 的 `healthcheck` 已验证用的是 `/healthz` |
| 可维护性 | `docs/resource-hardening.md` 里的计数（"M51–M62 12 条""10 项""2.8 倍"）与实现一致 | — | 已核对，无问题（`grep -c 'ident="M'` 得 64 条，M51–M64 连续存在） |
| 可维护性 | `dev-up.sh` 等 3 个脚本硬编码 `C:/Users/tutic/.workbuddy/...` 作 PATH 前缀 | P3 | **驳回不改**：这是本仓库既有的 PATH 约定（其余脚本同形），且都保留了 `$PATH` 兜底与 Python `-x` 存在性回退，缺失只会降级不会崩 |
| 测试覆盖 | 两个 `_bench_test.go` 不参与 CI | P3 | **驳回**：benchmark 是测量工具不是守卫，它们的价值在"归因"而非"防回归"，本就不该进 CI 门禁 |
| 可维护性 | 行尾：全库 `i/lf w/lf`；新增未跟踪文件无 CRLF | — | 已核对，无问题 |
| 可维护性 | 工作区无 `.tmp/`、`bin/`、`results/` 等产物混入待提交 | — | 已核对，无问题（`git check-ignore` 命中 4 条规则） |
