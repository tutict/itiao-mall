# 安全测试报告

日期：2026-10-10
被测版本：`ca60941`（工作树另有本轮新增的 `scripts/security-probe.mjs`）
环境：Go **1.27.2**（你刚升级的）· Docker Desktop 28.4.0 · PostgreSQL 17（宿主 **15432**）· Redis 7.4（6379）· 迁移版本 3

> 本报告只写**实测到的东西**。每条结论都带原始命令或原始响应，没有"应该没问题"。

---

## 0. 结论先行

| 项 | 结果 |
| --- | --- |
| 依赖/工具链漏洞 | **0 条可达**（上轮的 28 条标准库漏洞随 Go 升级全部消失） |
| 全量测试 | **285 个子测试全绿 / 0 SKIP / 0 FAIL** |
| 变异验证 | **34/38 抓住**，4 条是**真实覆盖空洞**（不是守卫写了没用，是根本没测） |
| 活服务器攻击面 | **56 项全过 / 0 失败**（认证、JWT、越权、CORS、头部、体量、重放、限流） |
| 新发现问题 | **6 个**，其中 **1 个 P0 前置阻塞**、**2 个 P1** |

**一句话**：外围防线（身份、权限、头部、限流、注入）这一轮实测是结实的；**问题全部集中在"业务语义"和"运维面"** —— 支付回调不核金额、探针端点泄露内网地址、以及四处"以为有保护其实没测过"的守卫。

---

## 1. 环境准备（有一处 Windows 坑要先说清）

`bash scripts/dev.sh` 起 PG 失败：

```
Error response from daemon: ports are not available: exposing port TCP 0.0.0.0:5432 ->
listen tcp 0.0.0.0:5432: bind: An attempt was made to access a socket in a way forbidden by its access permissions.
```

根因不是配置，是 **Windows 的保留端口段**：

```
$ netsh int ipv4 show excludedportrange protocol=tcp
开始端口    结束端口
    5329       5428
    5429       5528      ← 5432 落在这里
```

`net stop winnat` 需要管理员权限，本机提不了权。所以本轮用**临时 override 把宿主端口改到 15432**（容器内仍是 5432，仓库里的 compose 文件没动）：

```yaml
# pg-port-override.yml —— 放在哪儿都行（**不入库**：这是个人环境配置）。
# 本机放在 .workbuddy/ 下，那个目录已被 .gitignore。
# 内容就这三行：
services:
  pg:
    ports: !override
      - "15432:5432"
```

```bash
docker compose -f deploy/docker-compose.dev.yml -f <你的覆盖文件> up -d pg
export DATABASE_URL="postgres://itiao:itiao@localhost:15432/itiao?sslmode=disable"
```

> **要给你的一句话**：如果重启电脑后 5432 又能用了（保留段是动态的），这份 override 就可以删掉。想永久解决就 `net stop winnat && net start winnat`（管理员），但那会短暂中断所有容器网络。

---

## 2. 依赖与工具链

Go 从 1.26.0 升到 **1.27.2**。govulncheck 也重装了（旧二进制是 go1.26 编译的，解析不了 1.27 源码，会报一堆 `file requires newer Go version` —— 那不是漏洞，是工具版本不匹配）。

```
$ govulncheck ./...
No vulnerabilities found.
Your code is affected by 0 vulnerabilities.
This scan also found 0 vulnerabilities in packages you import and 3
vulnerabilities in modules you require, but your code doesn't appear to call these.
```

| 指标 | 上轮（go1.26.0） | 本轮（go1.27.2） |
| --- | --- | --- |
| 可达漏洞 | **28**（10 条在 net/http，远程 DoS） | **0** |
| 导入包漏洞 | — | 0 |
| 依赖模块级漏洞 | — | 3（**不可达**，代码没调用到） |

**上轮挂着的"待工程师操作"这一条已经关闭。** 建议在 `go.mod` 里把 `go 1.26.0` 提到 `go 1.27`（可选，只是让声明与实际工具链一致；不提也不影响构建）。

---

## 3. 全量测试（PG/Redis 就绪后）

```
$ go test -count=1 -v ./internal/...
```

| 包 | PASS | SKIP | FAIL |
| --- | --- | --- | --- |
| auth | 3 | 0 | 0 |
| config | 15 | 0 | 0 |
| domain | 6 | 0 | 0 |
| httpapi | 143 | 0 | 0 |
| ratelimit | 15 | 0 | 0 |
| service | **103** | 0 | 0 |
| **合计** | **285** | **0** | **0** |

**最关键的变化是 `service` 包的 103 个**。上一轮 PG 掉线时这 103 个**整体 skip**，而丢掉恰好是超卖、幂等、回调幂等、审核 CAS、refresh 并发这几条最贵的路径。本轮它们**真的跑了，全过**。

`SKIP` 清零这一点单独列出来，是因为"skip 与真通过在退出码上无法区分"已经在本项目踩过两次（记忆里的铁律 14）。

---

## 4. 变异验证（38 条）

```
$ python -u scripts/verify-guards.py     # 结果：34/38
```

### 4.1 上轮遗留的两条已关闭

| 变异体 | 上轮 | 本轮 |
| --- | --- | --- |
| M37 封面地址改用黑名单 | ⏳ 待 PG | ✅ 被 `TestValidateCoverURL` 抓住 |
| M38 封面地址不校验主机名 | ⏳ 待 PG | ✅ 被 `TestValidateCoverURL` 抓住 |

### 4.2 三条是我自己把变异体写坏了（已修）

这三条的运行结果是「测试确实红了，但**不是**预期的那些：只有包级失败，没有具名测试」—— 也就是**编译不过**。**退出码非 0 不足以证明守卫有效，编译错误也会非 0。**

| 变异体 | 病因 | 修法 |
| --- | --- | --- |
| M18 | 往 struct 字面量里塞了 `_ = timeout` → 语法错误 | 只改 `ReadTimeout` 一行 |
| M19 | —（另见下条，性质不同） | 见 4.3 |
| M22 | 只改使用处 `TokenHash: hash,`，导致 `hash` 未使用 → Go 编译错误 | 改**声明**处：`hash := refreshToken` |

修完全部转"抓住"：

```
[1/5] M18 ReadTimeout 不再跟随判定超时      -> ✅ 被 TestAllow_HasItsOwnTimeout 抓住
[2/5] M19 丢掉 ErrBackendUnavailable 哨兵   -> ✅ 被 TestDecide_BackendDown, ...FailsClosedAs503 抓住
[3/5] M20 DB 轮换 CAS 去掉 status 条件      -> ❌ 仍全绿
[4/5] M21 DB 轮换 CAS 去掉过期条件          -> ❌ 仍全绿
[5/5] M22 库里改存 refresh token 明文       -> ✅ 被 TestRefresh_DatabaseStoresHashNotPlaintext 抓住
```

### 4.3 M19 的原始描述是**假条目**（这比"未证明"更值得说）

原变异体声称"限流后端地址随错误外泄给攻击者"，期望 `TestAllow_DoesNotLeakBackendAddress` 报红。实测**它抓不住，而且不该指望它抓住** —— 因为那条危害根本不成立：

- `Decide` 只在 fail-closed 分支返回 err；
- 中间件拿到 err 后**只记日志、不回响应体**（回的是固定哨兵 `ErrRateLimitUnavailable`）。

所以内网地址到不了客户端。**一条恒为"未证明"的假条目比没有更坏，它占着位置。** 已把它改成描述真实后果的变异体：

> 拆掉 `%w` 包装 → `errors.Is(err, ErrBackendUnavailable)` 恒为 false → `Decide` 落到"放行"分支 → **配置为 fail-closed 的限流器在 Redis 故障时静默变成 fail-open**。

这个后果是有牙的（实测被两条测试抓住），而且是**最坏的失效形态**：准入控制不是报错，是消失。

### 4.4 四条是真覆盖空洞（进加固计划）

| 变异体 | 为什么测试抓不住 | 后果 |
| --- | --- | --- |
| **M20** DB 轮换 CAS 去掉 `status='ACTIVE'` | Redis 健康时 Lua 已先原子消费，DB CAS 只是兜底 —— 没有测试跑"Redis 不可用时的并发刷新" | 兜底那层的正确性**从未被验证** |
| **M21** DB 轮换 CAS 去掉 `expires_at > now()` | Go 层在步骤 1 已经查过 `ExpiresAt`，SQL 里那条件是纵深防御 —— 没有测试单独验证它 | 同上 |
| **M23** `redisConsumed && DB CAS 失败 → Compensate` | 补偿回滚路径**零覆盖** | Redis 与 DB 不一致时的自愈逻辑从未被执行 |
| **M24** `GetRefreshTokenByHash` 的非 `ErrNoRows` 错误 | 没有测试制造"查库报别的错" | 把基础设施故障误判成"令牌不存在"，用户被静默登出 |

**注意这四条的性质**：不是"守卫写了没用"，而是"**我们以为有保护、但它从未被验证过**"。这正是变异验证存在的意义。

---

## 5. 活服务器攻击面测试

新增可复现脚本 `scripts/security-probe.mjs`（零依赖，Node 22 原生 `fetch` + `node:http`）。

**为什么要单独做这个**：单元/集成测试跑在进程内，走不到中间件链。而"中间件顺序"这类缺陷只有以**真实 HTTP 形态**发请求才看得见 —— 典型例子是 CORS 排在 authGate 之后时，`curl` 逐个接口测全是好的，只有浏览器会炸。

```
$ node scripts/security-probe.mjs
结果  PASS=56  FAIL=0  WARN=0  SKIP=0     (exit 0)
```

| 组 | 覆盖 | 结果 |
| --- | --- | --- |
| A 认证门禁 | 6 个受保护端点 + 1 个未登记路径 | 7/7（未登记路径 **401 fail-closed**） |
| B JWT 伪造 | alg=none ×2 / 算法混淆 / 错密钥 / typ 混用 / 未定义角色 / 过期 / 结构损坏 | **8/8 全 401** |
| C 越权矩阵 | 7 组合法角色交叉 + 登录入口隔离 ×2 + 账号枚举对照 | 10/10 |
| D 安全响应头 | 200 / 401 / 404 / 400 四种响应形态 | 4/4 齐备 |
| E CORS | 预检不带 Authorization、Vary、未授权来源 ×2 | 5/5 |
| F 请求体上限 | 2 MiB→413、**恰好 1 MiB 通过**、chunked→411、撒谎 Content-Length 8 MiB | 4/4（未 5xx） |
| G 信息泄露 | 畸形 JSON / 404 / readyz | 3/3（见 F4） |
| H 跨租户隔离 | 6 种动词打别家商品 | 6/6 **404** |
| I refresh 重放 | 轮换 / 重放 / 家族吊销 / 未知 token | 4/4 |
| J 限流 | 触发点、Retry-After、X-RateLimit-*、响应体 | 5/5 |

### 5.1 探针自身踩的两个坑（已修，值得记）

1. **探针用自己的额度把自己挡住了**。`BodyLimit` 排在 `rateLimitGate` **之后**（这是刻意的设计，见 `router.go` 注释：否则写超限的 `Content-Length` 就能绕过限流计数），所以打 `/auth/merchant/login` 做体量测试的 4 次请求**都计入认证档配额**，跨租户用例成了第 10 次 → 429。
   修法：给准备动作加 429 退避重试；体量测试改打**带 token 的写接口**（write 档 120/min，占用可忽略）。
   **不能用"少测几个用例"来绕** —— 那等于让限流强度反向决定测试覆盖面。

2. **PUT 必须带合法 body**。gen 层的 schema 校验排在归属检查之前，空 body 会先撞上"商品标题不能为空"得到 400，测不到越权。这条一度让我误判为"跨租户写入漏洞"，实测澄清如下。

### 5.2 一条被澄清的**假警报**（记录在案，避免下次重复劳动）

现象：商户 B 对商户 A 的商品发 `PUT {}` 返回 **400 商品标题不能为空**，而其他动作返回 404。

验证它是不是"存在性探测 oracle"：

| 请求 | 存在且属于 A | 不存在 |
| --- | --- | --- |
| 非法 body | 400 | **400** |
| 合法 body | 404 | **404** |

**两组完全同形 → 不构成 oracle**，400 是纯 schema 校验、与资源无关。真实验证跨租户写入：

```
B PUT A 的 DRAFT 商品（合法 body）-> 404 资源不存在
A 读回 title= A的草稿商品        -> 未被改写
```

**结论：跨租户隔离是有效的。**

---

## 6. 新发现（按严重度）

### F1 · P0 · 生产环境可用 mock provider（免费下单后门）

上轮已修（`validateMockProviders()` + 显式逃生门），**本轮复核通过**：生产环境下 `AUTH_PROVIDER=mock` / `PAYMENT_PROVIDER=mock` 会让服务启动失败，除非显式设 `ALLOW_MOCK_PROVIDERS_IN_PROD=true`（并在启动日志打 Warn）。

活服务器（dev）启动日志确认两条 mock 告警都在：
```
msg="使用 mock provider（开发环境）" auth_provider=mock payment_provider=mock env=dev
```

### F2 · P0 前置 · 服务**无法以生产模式启动**

```go
// cmd/api/main.go
default:
    payProvider = payment.NewMock()   // PAYMENT_PROVIDER=wechat 直接返回"尚未实现"
```

这不是本轮引入的障碍，而是上轮加的生产校验**暴露了一个既存事实**：这个服务从来没具备过上生产条件，只是以前它不会告诉你。**微信支付 Provider 是上生产的前置条件**，不是"加固项"。

### F3 · P1 · 支付回调**不核对金额**（实测）

`HandleNotify` 从头到尾没有比较 `ev.AmountCents` 与 `order.TotalCents`。实测：

```
下单 status=201  order_no=20261010150141AT7JHKZC  total_cents=2274  status=PENDING_PAYMENT
回调 amount_cents=1 -> 200 {"handled":true,"order_no":"20261010150141AT7JHKZC","status":"PAID"}
订单最终 status= PAID
>>> 1 分钱把 2274 分的订单标记为已支付: ★是
```

在 mock 下这"看起来理所当然"（mock 连签名都不验），但**这条路径与真实支付共用**（`MockPayNotify` 刻意走与真实回调完全相同的 `HandleNotify`）。接微信支付时，**服务端必须自己按 `out_trade_no` 反查订单金额并比对**，而不是只信回调报文里的 amount —— 这是电商支付最基本的一条对账纪律，漏掉就是资损。

顺带：mock 模式下 `payment_events` 唯一约束保证了幂等，这部分是好的。

### F4 · P1 · `/readyz`（公开、免认证）泄露内网地址（实测）

```go
checks["pg"] = err.Error()      // ← 原始错误直接进响应体
checks["redis"] = err.Error()
```

实测（短暂停掉 Redis 后立即恢复）：

```
$ curl -s http://localhost:8080/readyz
{"checks":{"pg":"ok","redis":"dial tcp [::1]:6379: connectex: No connection could be made
because the target machine actively refused it."},"status":"unavailable"}   [503]
```

泄露了**内网地址与端口**。如果 PG 挂掉，pgx 的错误还会带上 `host= / user= / database=` —— 那是**凭据上下文**，比地址更敏感。

这是「探针端点把内部拓扑当健康状态发出去」的经典形态，而它是**公开路由**。

### F5 · P2 · 四条安全守卫从未被验证（M20/M21/M23/M24）

见 §4.4。四条都在 refresh 轮换的**兜底与自愈**路径上。

### F6 · P2 · `service` 包还有 3 处 `t.Skip` 残留

| 文件 | 触发条件 | 丢掉的覆盖 |
| --- | --- | --- |
| `service/token_integration_test.go:41` | 连不上 Redis | refresh 轮换全部 |
| `service/merchant_integration_test.go:45` | 连不上 Redis | 商户入驻与审核 |
| `service/commerce_integration_test.go:32` | 未设 `DATABASE_URL` | 超卖/幂等/回调幂等 |

上一轮只改了 `ratelimit` 与 `httpapi` 两处副本，**这两份 `service` 里的副本漏了**。同一个辅助函数在多个包里各有一份时，改一处不等于改完 —— 这条教训本轮再次应验。

另：`ratelimit_test.go:404` 有一条 `t.Skip("Redis 未报错…")`，它让"地址不泄露"那条断言变成**环境相关**（本机 dial 失败形态是 timeout 而不是 refused，所以它"通过"）。

### F7 · P3 · 注释与实现不一致：`Allow` 仍拼了原始错误

```go
// 注释：刻意**不**把 err 原样上抛：Redis 的错误原文含内网地址与拓扑信息
return Result{}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)   // ← 但又 %v 拼上了
```

当前**没有对外泄露**（中间件不回响应体，实测确认），但注释在撒谎，而撒谎的注释会让下一个人据此做错决定。修法见计划 P2-6。

### F8 · P3 · `X-Request-ID` 客户端可控且原样回显入日志

`RequestID` 中间件直接采信请求头里的 `X-Request-ID`，写进响应头与所有日志。攻击者可以指定任意 id 伪造日志关联。长度受 `MaxHeaderBytes`(16 KiB) 限制，所以危害有限。

### F9 · 信息 · 3 个依赖模块级漏洞（不可达）

govulncheck 报"3 vulnerabilities in modules you require, but your code doesn't appear to call these"。当前不构成风险，但应在门禁里持续跟踪。

---

## 7. 实测确认**干净**的面（别重复劳动）

| 面 | 验证方式 | 结论 |
| --- | --- | --- |
| 认证门禁 fail-closed | 活服务器：未登记路径 → 401 | ✅ 且 401 而非 404，不送接口探测 |
| JWT 算法与载荷 | 活服务器：8 种伪造全 401（含 alg=none、算法混淆） | ✅ `WithValidMethods` + typ + role 三重 |
| 角色越权 | 活服务器：7 组交叉 + 2 个入口隔离 | ✅ 403 |
| 账号枚举 | 活服务器：不存在账号与错密码响应**逐字节相同** | ✅ |
| 跨租户隔离 | 活服务器：6 种动词 → 404，且无存在性 oracle | ✅ 404 而非 403 |
| CORS 顺序 | 活服务器：预检不带 Authorization → 204；`Vary: Origin` 无条件 | ✅ |
| 安全响应头 | 活服务器：4 种响应形态（含直接 `Write` 路径） | ✅ |
| 请求体上限 | 活服务器：413 / 411 / 边界 1 MiB / 撒谎客户端不 5xx | ✅ |
| 错误不泄露内部结构 | 活服务器：畸形 JSON 与 404 的响应体 | ✅ 无 Go 类型名 |
| refresh 重放与家族吊销 | 活服务器：重放后新 token 同样失效 | ✅ |
| 限流 | 活服务器：429 + Retry-After + X-RateLimit-* | ✅ |
| SQL 注入 | 静态：全库 sqlc 参数化，无裸拼接 | ✅ |
| 密钥与配置 | 静态：`IsProd` 白名单反查、生产强制 `JWT_SECRET`、mock 生产校验 | ✅ |

---

## 8. 复现方式

```bash
# 1) 依赖
#    干净检出（compose 默认把 PG 映到 5432）：
docker compose -f deploy/docker-compose.dev.yml up -d
export DATABASE_URL="postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable"
#
#    本机（Windows）特例：系统保留端口段占了 5432，所以本机额外叠了一份覆盖文件
#    把端口换成 15432。那份覆盖文件是个人环境配置、不入库 —— 换机器时自己叠一个，
#    或直接用上面的 5432 版本：
#      docker compose -f deploy/docker-compose.dev.yml -f <你的覆盖文件> up -d
#      export DATABASE_URL="postgres://itiao:itiao@localhost:15432/itiao?sslmode=disable"

# 2) 迁移 + 种子
(cd services/api && go run ./cmd/api migrate)
docker exec -i itiao-pg psql -U itiao -d itiao -q < tools/loadtest/seed/reset.sql
docker exec -i itiao-pg psql -U itiao -d itiao -q < tools/loadtest/seed/seed.sql

# 3) 全量测试（门禁口径：PASS 下限 380，SKIP 必须为 0）
(cd services/api && go test -count=1 -v ./...)

# 4) 变异验证（全量约 6 分钟；--audit 是残留自检）
python -u scripts/verify-guards.py

# 5) 活服务器攻击面
bash scripts/api.sh &                     # 或另开一个终端
PROBE_TENANT_B_PHONE=13800000001 \
PROBE_TENANT_A_PRODUCT_ID=aaaaaaaa-0000-4000-8000-000000000001 \
node scripts/security-probe.mjs

# 或直接跑总入口（它会替你传上面那两个变量，并断言探针 SKIP=0）
bash scripts/check-all.sh
```

> **跨租户组需要的 2 号商户现在就在仓库种子里**（`tools/loadtest/seed/seed.sql`）——
> 上面第 2 步跑完 seed 就已就位，不需要任何额外夹具。
>
> 密码不用传：种子里 2 号商户与 1 号商户**共用同一个密码哈希**，探针复用
> `PROBE_MERCHANT_PASSWORD` 即可。
>
> 不传那两个变量时该组仍会 SKIP 并打印原因（不会假装通过），但
> `scripts/check-all.sh` 会断言探针 `SKIP=0` —— 在门禁里它必须真的跑起来。
>
> 这个缺口曾经真实存在过：夹具一度只放在不入库的 `.workbuddy/` 下，于是门禁里
> 那 6 条 check **一路绿着但从未执行**（`skip()` 只加计数，探针退出码只看 `FAIL`）。
> 这与 Go 侧「`t.Skip` 与真通过在退出码上无法区分」是同一个毛病换了一层皮，
> 所以两处用的是同一个判据：**跳过不算通过**。

---

## 附：本轮对仓库的改动

| 文件 | 改动 |
| --- | --- |
| `scripts/security-probe.mjs` | **新增**。活服务器攻击面探针，零依赖，退出码即结论 |
| `scripts/verify-guards.py` | 修 M18/M19/M22 三条写坏的变异体；M19 重定义为真实后果 |

（另有一份绕开 Windows 保留端口的 compose override（`pg-port-override.yml`）——
那是**个人环境配置，不进仓库**，所以不列在"对仓库的改动"里。内容见上面「复现方式」。）
