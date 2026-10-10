# itiao-mall

一个商城应用的最小闭环：**用户端 App + Web（Expo / React Native）、商户端 App + Web、管理员端 App + Web、Go 后端 + k6 性能门禁**。

当前阶段：**后端已成型，前端只做了用户端**。

- **已落地，且有测试与变异守卫**：三角色权限模型（user / merchant / admin）· 商户入驻与平台审核 ·
  商户商品管理与审核（5 状态机 + 审核 CAS）· Redis 滑动窗口限流 · refresh token 一次性轮换
  （PostgreSQL 为权威、Redis 为加速层）· 防超卖与幂等 · k6 两层性能门禁。
- **尚未开工**：商户端 / 管理员端的**前端**（`apps/merchant`、`apps/admin` 目前是空目录）。
  对应的**后端接口已就绪**（见 `docs/openapi/openapi.yaml`），缺的只是界面。
- **仍是 mock**：支付与微信登录走 mock 网关（还没有微信支付商户号与开放平台资质）。
  生产环境**默认禁止** mock provider，要开必须显式设逃生门 —— 见 [安全加固报告](docs/security-hardening.md)。

## 技术选型

| 层 | 选型 | 为什么 |
| --- | --- | --- |
| 用户端 | Expo SDK 57 + Expo Router | 一套代码出 Android / iOS。Metro 用 Babel 剥类型，**不依赖 tsc 的编程式 API**，所以能跟上 TS 7 |
| 商户端 | 同上（独立 Expo 代码库） | P1，尚未开工 |
| 后端 | Go 1.26 + chi v5 | chi 与 oapi-codegen 的 chi-server 生成器零摩擦（gin/echo 得包一层自定义 Context）。工具链漏洞用 `govulncheck` 卡，见「安全」一节 |
| 契约 | OpenAPI 3.0.3 | **前后端唯一真源**。3.0（不是 3.1）是为了 oapi-codegen 的兼容性 |
| 代码生成 | oapi-codegen → Go 严格接口；openapi-typescript → TS 类型 | 生成物提交进仓库，用 `git diff --exit-code` 卡漂移 |
| 数据访问 | sqlc + pgx/v5 | 编译期类型安全的 SQL，不用反射、不引入 ORM |
| 迁移 | goose（`go:embed` 内嵌） | 迁移随二进制走，部署不依赖外部文件 |
| 存储 | PostgreSQL 17 + Redis 7 | 库存靠行锁、幂等靠唯一索引；refresh token 与家族吊销放 Redis |
| 压测 | k6 v1.7 | 门禁要能进 CI，脚本即代码 |

## 目录结构

```
apps/app/                 Expo 用户端 App（App Router 文件式路由）
packages/api-contract/    由契约生成的 TS 类型（全部是生成物，不要手改）
services/api/             Go 后端
  cmd/api/                serve / migrate / version / healthcheck
  cmd/hashpw/             生成 bcrypt 口令（给商户账号用）
  db/migrations/          goose 迁移
  db/queries/             sqlc 查询
  internal/domain/        状态机唯一真源（订单流转 + 商品 5 状态）
  internal/config/        环境变量解析 + fail-fast 校验（生产禁 mock provider、体积上限、限流阈值）
  internal/auth/          JWT 签发/校验、refresh 轮换与家族吊销、微信 exchanger
  internal/ratelimit/     Redis 滑动窗口限流（ZSET + Lua 原子脚本 + 降级决策）
  internal/payment/       支付网关抽象 + mock 实现
  internal/service/       业务事务边界（token 轮换编排、商品审核）
  internal/httpapi/       接口实现 + 中间件链（CORS / 认证 / 限流 / 体积 / 安全头）+ 统一错误映射
docs/openapi/openapi.yaml  契约真源
deploy/                   docker-compose（dev / perf）
scripts/                  入口脚本，详见下方「脚本清单」
tools/codegen/            TS 类型生成包装脚本
tools/loadtest/           k6 场景、种子数据、基线、比对脚本
```

### 脚本清单

| 脚本 | 作用 | 必跑 |
| --- | --- | --- |
| `check-all.sh` | **总入口**：一次跑完下面所有守门项 | ✅ 先跑这个 |
| `dev.sh` | 起 PG + Redis 并等健康（比手敲 `docker compose` 多一步健康等待） | |
| `gen.sh` | 由契约生成 Go 接口与 TS 类型 | |
| `api.sh` | 起后端（默认 `:8080`，providers=mock） | |
| `perf.sh` | k6 压测 + 与基线比对（会 reset 并 seed 数据库） | |
| `verify-perf-gate.sh` | 变异验证：证明性能门禁真的能拦住回归 | |
| `verify-guards.py` | 变异验证：证明权限 / 审核 / 错误泄露 / 分页 / 限流 / refresh / 商品 / 安全加固的守卫真的有牙（38 条变异体）。`--audit` 只做残留自检，`--from/--to` 分批跑，`--packages` 收窄到不依赖 DB 的包 | |
| `verify-onboarding-flow.py` | 对活服务器跑一遍商户入驻全生命周期（35 步状态码断言） | |
| `acceptance-app.mjs` | 用户端端到端验收（真实 Chromium 驱动，15 项） | |
| `preview-web.mjs` | Expo web 产物的静态预览服务器（验收脚本的前置） | |

`check-all.sh` 会断言 Go 测试的**条数下限**。这不是形式主义：不设 `DATABASE_URL` 时
集成测试整体 skip，退出码依然是 0 —— 实测 27 PASS / 12 SKIP 与 39 PASS / 0 SKIP
的退出码完全相同，而丢掉的 12 条恰好是超卖、幂等、回调幂等、审核 CAS。
`MIN_GO_TESTS` 环境变量可覆盖下限。

## 快速开始

```bash
pnpm install                                   # 依赖
docker compose -f deploy/docker-compose.dev.yml up -d   # PG + Redis
bash scripts/gen.sh                            # 由契约生成 Go 接口与 TS 类型
bash scripts/api.sh                            # 起后端（默认 :8080，providers=mock）
bash scripts/perf.sh                           # 跑性能门禁
```

App：

```bash
cd apps/app && pnpm start        # 或 pnpm android / pnpm web
```

Android 模拟器里 `localhost` 指向模拟器自己，宿主机要用 `10.0.2.2`。App 已按平台设好默认值，
真机调试用 `EXPO_PUBLIC_API_BASE_URL=http://<你的局域网IP>:8080` 覆盖。

## 跨域（CORS）

商户 Web（P1）与 Expo web 都要从浏览器打后端，所以 CORS 是必需品，不是可选项。

| 环境变量 | 默认 | 作用 |
| --- | --- | --- |
| `CORS_ALLOWED_ORIGINS` | 空 | 逗号分隔的**精确** origin，如 `https://shop.example.com,https://m.example.com` |
| —（无变量） | dev 自动开 | dev 环境额外放行任意 `localhost` / `127.0.0.1` / `[::1]` 端口 |

两条踩过的坑，都写进了 `internal/httpapi/cors.go` 的注释与 `cors_test.go`：

1. **CORS 必须排在 `authGate` 之前。** 预检 `OPTIONS` 按规范**不带** `Authorization` 头，
   而 `authGate` 是默认拒绝的。顺序反了，每个带 token 的跨域请求都会先吃一个 401 ——
   这个 bug 用 curl 逐个测接口**测不出来**，只有真实浏览器会炸。所以 `isPublicRoute` 里
   另外放行了 `OPTIONS` 作纵深防御。
2. **`Vary: Origin` 必须无条件加**，不能只加在"放行"分支。未放行来源拿到的是同一 URL 上
   "没有 ACAO"的另一个响应体；若被共享缓存（CDN / 反代）缓存，之后合法来源会取到那份副本，
   浏览器照样拦。实测确认过这两条：预检 204 + 回显 origin，恶意 origin 无 ACAO 但仍带 `Vary`。

响应头**刻意不设** `Access-Control-Allow-Credentials`：本项目走 Bearer token，不用 Cookie。

## 本地预览与 P0 验收

```bash
node scripts/preview-web.mjs --port 8081        # 预览 apps/app 的 web 导出
node scripts/acceptance-app.mjs                 # 用真实 Chromium 跑一遍闭环并逐步截图
```

`preview-web.mjs` 不是可有可无的糖：Expo 静态导出把**动态路由**落成带方括号的占位文件
（`product/[id].html`、`order/[id].html`），通用静态服务器只会把它当普通文件，
请求 `/product/<uuid>` 一律 404。这个脚本补上了干净 URL 与动态路由解析，并挡住路径穿越。

`acceptance-app.mjs` 走完「登录 → 商品列表 → 详情 → 加购 → 结算 → mock 支付 → 订单列表」，
每一步截图并断言关键文案，最后写 `report.json`。它**同时是 CORS 的端到端验证**：
App 在 `:8081`、API 在 `:8080`，浏览器里的每个请求都是跨域的。退出码即结论。

两个脚本都零依赖：本机 `npm install` 在沙箱里会崩（`Cannot read properties of null
(reading 'edgesOut')`），而 ms-playwright 缓存里已经有 Chromium，所以 `acceptance-app.mjs`
直接讲 CDP（Node 22 自带全局 WebSocket）。找不到浏览器时用 `CHROME_PATH` 指定。

## 性能门禁：两层，分工不同

**这是本项目最该先读的一段。**

| 层 | 在哪 | 判据 | 作用 |
| --- | --- | --- | --- |
| 灾难线 | `tools/loadtest/scenarios/*.js` 的 `thresholds` | 绝对值 SLO（如浏览 p95 < 300ms）、错误率上限、`checks` 通过率 | 没有基线也能跑。抓"服务已经不可用" |
| 回归线 | `tools/loadtest/baselines/load.json` + `tools/loadtest/scripts/compare.mjs` | 同机基线中位数比对：p95 +20%、p99 +25%、吞吐 −10%、错误率绝对 0.5%，**且延迟类必须同时超过绝对下限** | **真正抓"后端引入的性能问题"** |

三个关键设计：

1. **灾难线刻意很松，回归线才是主力。** 实测：往商品列表注入 25ms 延迟后，浏览 p95 从 1.70ms 涨到 26.90ms，
   灾难线照样通过（26.90ms 远低于 300ms），是回归线把它拦下来的。绝对值在换机器后不可比，
   所以绝对值只当"离谱线"用，真正的判定依据是同机基线。
2. **读不到指标一律判失败。** `compare.mjs` 不会把 `undefined` 当成 0 放过 —— 真空通过的门禁比没有门禁更危险。
3. **延迟类判据要有绝对最小效应量。** 见下，这条是量出来的，不是拍脑袋加的。

基线用 3 次运行取**中位数**，不用单次：单次受 GC、磁盘缓存影响很大，拿一次数当标准等于拿噪声当标准，
之后每次都会误报，门禁最后会被人为关掉。

```bash
RECORD_BASELINE=1 RUNS=3 bash scripts/perf.sh   # 重建基线（跨机器不可比，换机要重建）
bash scripts/perf.sh                            # 正常运行 + 与基线比对，退出码即结论
bash scripts/verify-perf-gate.sh                # 证明门禁不是真空的（见下）
node --test tools/loadtest/scripts/compare.test.mjs   # 门禁自身的单元测试
```

### 为什么要给延迟加「绝对最小效应量」

这是实测暴露出来的问题，不是预防性设计。本机 3 次单跑的离散度：

| 指标 | run1 | run2 | run3 | 中位 | **单跑极差/中位** | 门禁容忍度 |
| --- | --- | --- | --- | --- | --- | --- |
| 总 p95 | 2.923 | 2.846 | 2.598 | 2.846 | **11.4%** | +20% |
| 总 p99 | 3.711 | 3.323 | 3.027 | 3.323 | **20.6%** | +25% |
| 下单 p95 | 3.518 | 3.255 | 2.978 | 3.255 | **16.6%** | +20% |
| 吞吐 | 174.117 | 174.136 | 174.089 | 174.117 | 0.0% | −10% |

**p99 自身的噪声（20.6%）几乎顶到容忍度（25%）**，这样下去门禁会开始随机报红，
然后被人为关掉 —— 正是上面第 3 段警告的那个失败模式。

根因是延迟绝对值只有 2~4ms，GC / 调度抖动占比过大，**百分比判据在亚毫秒尺度上没有意义**。

真实回归不会这么小：注入 25ms 后，总 p95 的绝对变化是 +23.97ms。所以 `compare.mjs` 给每条
延迟判据加了 `minAbs`（p95 1.5ms / p99 2ms），**相对容忍度和绝对下限都超了才判失败**。
被抑制的项会在输出里**显式标注**「视为噪声」，不做无声放过。

注意：这只影响**相对**判据，k6 原生 `thresholds`（灾难线）不受影响，仍是硬上限。
加上它之后重跑 `verify-perf-gate.sh`，注入 25ms 仍报红（总 p95 +842%、总 p99 +737%、浏览 p95 +1486%），
且失败只落在被注入的读路径上（下单 +13.9% / 登录 +15.5% 照常 PASS）—— 说明判据没有被削弱，也没变成一刀切。

### 门禁本身也要被验证

`bash scripts/verify-perf-gate.sh` 会往被测处理函数注入一次固定延迟、起在另一个端口、
先自检「注入确实生效」、再确认门禁**报红**，然后按 sha256 还原源码、确认门禁**转绿**。

一个从不报红的门禁和没有门禁是同一种东西。所以这个脚本是必须跑一遍的，不是可选项。
`compare.mjs` 的比对逻辑抽成了可导入的纯函数 `compareReports()`，配 14 个单测覆盖
「缺指标必须判失败」「噪声抑制」「真回归报红」三条路径 —— 守卫自己也要有测试。

### 压测请求带 `Origin`（否则压不到 CORS）

`tools/loadtest/lib/auth.js` 在唯一的 http 出口处给**每个**请求补 `Origin` 头。
服务端的 CORS 中间件只在 `Origin != ""` 时才真的执行设响应头那段代码；不带 Origin 的请求
走的是早返回分支。浏览器发的跨域请求一定带 Origin，所以不补的话，CORS 中间件等于没被压到。
`scenarios/smoke.js` 里另有 5 条断言直接钉住 CORS 行为（含预检 204 与 `Vary: Origin`）。

> 踩坑记录：`k6` 的 `http.options` 签名是 `(url, body, params)` 而非 `(url, params)`。
> 少写一个 body 位，`params` 会被当成 body 传出去，请求就**不带 Origin**，拿到 405 ——
> 症状看起来像"CORS 坏了"，其实是压测脚本参数写错。七个方法逐一探过，只有 `options` 是特例。

## 反超卖与幂等

- **库存**：`UPDATE skus SET stock = stock - n WHERE id = ? AND stock >= n`，靠 Postgres 行锁保证原子性。
  影响行数为 0 即库存不足 → 409。不需要分布式锁。
- **重复下单**：`Idempotency-Key` 头 + `orders(user_id, idempotency_key)` 部分唯一索引。撞唯一键就回滚并回读原单。
- **重复支付回调**：`payment_events(provider, transaction_id)` 唯一约束 + 订单状态 CAS。
  重复回调返回 `handled=false`，订单状态**不会**被改第二次。
- **令牌**：access 用 JWT（HS256，15 分钟）；refresh 是 Redis 里的不透明 token，**一次性轮换**。
  同一个已消费的 refresh token 再次出现即视为重放，整个 token family 被吊销。

## 安全

一轮完整的攻击面审计与加固，结论见 [docs/security-hardening.md](docs/security-hardening.md)。要点：

- **生产禁 mock**：`PAYMENT_PROVIDER` / `AUTH_PROVIDER` 默认都是 `mock`，而 mock 实现完全不设防
  （任意 `order_no` 可标记已支付、任意 `code` 可登录）。生产环境**默认拒绝启动**，除非显式设
  `ALLOW_MOCK_PROVIDERS_IN_PROD=true`（此时启动打 Warn）。这与 `JWT_SECRET` 的生产校验同构。
- **请求体 / 请求头上限**：`MAX_BODY_BYTES`（默认 1 MiB）+ `MAX_HEADER_BYTES`（默认 16 KiB）。
  有 body 的方法必须带 `Content-Length`（否则 411）；超限 413，且**一个字节都不读**。
- **安全响应头**：`nosniff` / `X-Frame-Options` / CSP / `Referrer-Policy` / `CORP`，
  在写响应的**那一刻**强制覆盖 —— 下游任何同名写入都盖不掉它。
- **依赖漏洞**：`govulncheck ./...` 会卡标准库与依赖漏洞。当前基线 Go 1.26.0 还挂着 28 条
  标准库漏洞（`net/http` HTTP/2 那批是远程可达的 DoS），升到 1.26.1+ 才能清掉 —— **待办**。

**刻意没加 HSTS**：它属于域名级 TLS 终止点，而本服务跑在同机 HTTP 后面（本地 `:8080`、容器内 `:8080`）。
在这里加要么无效，要么把 localhost 钉成 HTTPS 让人没法调试。它属于部署清单，不属于应用。

## 已知约束与踩过的坑

这些都是本机实测撞出来的，写下来免得重踩。

**TypeScript 7 没有 JS 编译器 API。** TS 7.0 换成 Go 原生编译器后不再提供 `ts.factory.*`（官方排在 7.1）。
`openapi-typescript` 内部 `require('typescript')` 会拿到工作区根的 TS 7.0.2，导入即崩：

```
TypeError: Cannot read properties of undefined (reading 'createKeywordTypeNode')
```

解决方式是让两个 TypeScript 共存：`typescript@7.0.2` 只当编译器（应用代码用它跑 `tsc --noEmit`），
根依赖里另有 npm 别名 `typescript5`（= 5.9.3）纯给工具喂编译器 API。
`tools/codegen/gen-ts-types.cjs` 在 `require` 之前把 `typescript` 的解析劫持到它。

**在 pnpm 12 + `node-linker=hoisted` 下，别指望用包管理器配置解决上面这个问题。** 实测过三条路，全部无效：

- `overrides` 用 npm: 别名指到 `@typescript/typescript6` —— 该别名不在 pnpm 定义的 peer 覆盖语义里，且 TS6 不满足 `^5.x`
- `overrides` 用 semver range 指到 `5.9.3` —— 文档说能覆盖 peer，实测无效
- `packageExtensions` 给它加一条 `dependencies.typescript` —— 配置能被读到，但 peer 仍从工作区根解析

**pnpm 12 不再从 `package.json` 读 `pnpm` 字段**，设置（如 `overrides`）必须写在 `pnpm-workspace.yaml`。

**Expo 的版本要用 SDK 锁的，不是 npm 上最新的。** SDK 57 锁的是 `react-native@0.86.3` + `react@19.2.3`，
而 npm 上最新是 0.87.1 / 19.3.0。权威来源是 `expo@<ver>/bundledNativeModules.json`。

**k6 的 `--summary-export` 是扁平结构**，和 `handleSummary()` 拿到的对象不一样：trend 是 `metrics[name]['p(95)']`
（不是 `.values[...]`），rate 是 `metrics[name].value`，counter 是 `.rate` / `.count`。
另外 **p(99) 默认不导出**，要用 `--summary-trend-stats` 显式声明。读错字段会静默拿到 `undefined`、门禁"全绿通过"。

**压测数据重置用 `TRUNCATE`，不用 `DROP SCHEMA`。** pgx 会缓存 prepared statement，
`DROP SCHEMA` + 重建会让缓存失效，之后所有查询报 `cached plan must not change result type` —— 看起来像后端崩了。

## 状态

**已完成**：契约与双向代码生成 · PG + Redis · 迁移 · 认证（JWT + refresh 一次性轮换，DB 权威）·
商品 / 购物车 / 订单 / 支付 · 反超卖与幂等（含并发测试）· 三角色权限与商户入驻审核 ·
商户商品管理与审核（5 状态机 + CAS）· Redis 滑动窗口限流 · 一轮安全加固（2 个 P0）·
k6 两层性能门禁 · 基线 · 性能门禁有牙验证 · 变异验证 38 条 · Expo 用户端闭环页面。

**下一步**：商户端 / 管理员端前端（`apps/merchant`、`apps/admin`）· 订单履约（发货 / 完成 / 退款，
`order_items` 加履约字段）· 微信支付 provider · 抽 `packages/api-client`。

**挂着待拍板的架构项**（不是打补丁能解决的，详见下面两份文档）：

1. **订单状态机未收归 `domain`** —— `SHIPPED / COMPLETED / REFUNDING / REFUNDED` 四态目前**无任何写入路径**，
   支付回调与取消各自手写 SQL 白名单，已经产生实质分歧（领域层判 `CREATED → PAID` 非法，实际却在走）。
2. **Go 工具链 28 条标准库漏洞** —— 全部 `Fixed in go1.26.1`，其中 `net/http` 那批是远程可达的 DoS。
3. **微信支付 / 微信登录的真实实现** —— 目前是 mock，需要商户号 + 开放平台资质。

## 文档

| 文档 | 内容 |
| --- | --- |
| [`docs/openapi/openapi.yaml`](docs/openapi/openapi.yaml) | **契约真源**。前后端唯一真源，改它必须跑 `scripts/gen.sh` 并用 `git diff --exit-code` 卡漂移 |
| [`docs/task13-delivery.md`](docs/task13-delivery.md) | 限流 / refresh 轮换原子化 / 商户商品管理与审核的交付说明（并发边界、失败重试、测试清单、变异表） |
| [`docs/security-hardening.md`](docs/security-hardening.md) | 安全加固报告：9 个攻击面、7 个真实问题（2 个 P0）、改法与证据、变异验证 |
| `README.md`（本文） | 项目总览：选型 · 目录 · 脚本 · 快速开始 · CORS · 性能门禁 · 安全 · 状态 |
