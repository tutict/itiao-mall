# 安全加固计划

日期：2026-10-10
依据：`docs/security-test-report.md`（本轮实测）
原则：**每条都有"验收标准"，且验收标准必须能被一条命令或一条变异体验证。** 不能被验证的加固条目不该出现在这张表里。

---

## 0. 全景

| 优先级 | 条目 | 性质 | 验收信号 |
| --- | --- | --- | --- |
| **P0** | 1 支付回调金额核对 | 资损 | 变异体 M39 被抓住 |
| **P0** | 2 真实微信支付 Provider | 上生产前置 | `APP_ENV=prod PAYMENT_PROVIDER=wechat` 能启动 |
| **P1** | 3 `/readyz` 脱敏 | 信息泄露（已实测） | 变异体 M40 被抓住 |
| **P1** | 4 补 4 条守卫空洞 | 覆盖 | 变异验证 **38/38** |
| **P2** | 5 `service` 包 `t.Skip` 清零 | 覆盖 | `go test ./...` 断言 0 SKIP |
| **P2** | 6 `Allow` 错误只带哨兵 | 纵深 / 注释诚实 | 变异体 M41 被抓住 |
| **P2** | 7 `X-Request-ID` 取值白名单 | 日志完整性 | 变异体 M42 被抓住 |
| **P3** | 8 订单状态机收归 `domain` | 业务正确性 | 4 个状态有写入路径 |
| **P3** | 9 `cover_url` 内网地址拒绝 | SSRF 面（弱） | 变异体 M43/M44/M45 |
| **P3** | 10 前端 Web 端 token 存储 | 客户端 | 明确记录为已知限制或迁移 |

另有**门禁**一节：把本轮新增的两类验证接进 `check-all.sh`，否则它们下次就失效了。

---

## P0-1 · 支付回调金额核对

**问题**（实测，非推测）

```
下单 total_cents=2274  →  回调 amount_cents=1  →  订单 PAID
```

`HandleNotify` 全程没有比较 `ev.AmountCents` 与 `order.TotalCents`。

**为什么必须做在服务端**

第三方回调里的金额是**对方说的**，不是事实。正确姿势是：以 `OutTradeNo` 为键**反查本地订单**，用本地的 `TotalCents` 与回调金额比对；不一致就拒绝并告警，订单保持待支付。只信回调报文 = 把定价权交给发报文的人。

**具体修法**

在 `HandleNotify` 里，`MarkOrderPaid` 之前加一步：

```
ev := ParseNotify(...)
order := GetOrderByOrderNo(ev.OutTradeNo)      // 先落订单
if ev.AmountCents != order.TotalCents {
    // 不落 payment_events（否则这笔"错误交易号"占用了唯一键，
    // 真回调到达时会被当成重复而丢弃）—— 这是本条最容易写错的地方
    log.Warn("支付金额不符", order_no, want, got)
    return nil, domain.UserFacingf("支付金额与订单不符", domain.ErrPaymentAmountMismatch)
}
```

要点：
1. **金额不符时不能写 `payment_events`**。写了会占用 `(provider, transaction_id)` 唯一键，导致同一笔交易的真回调被幂等判定吞掉 —— 用户付了钱订单却永远待支付。
2. 回 400 而不是 500（这是客户端/对端报文的问题）。所以要在 `mapError` 里登记新哨兵 `ErrPaymentAmountMismatch`。
3. 金额为 0 或负也要拒（`AmountCents <= 0`）。

**验收标准**

- 变体：`HandleNotify` 里删掉金额比较 → 新测试报红。
- 新增测试 `TestHandleNotify_RejectsAmountMismatch`（集成）：下单 2274 → 回调 1 → 断言返回错误、订单仍 `PENDING_PAYMENT`、`payment_events` **无新行**。
- 边界：`TestHandleNotify_AmountEqualSucceeds`（2274 == 2274 → PAID），防止把条件写成恒假。
- 变异体验证：新增 **M39**（删除金额比较），期望被 `TestHandleNotify_RejectsAmountMismatch` 抓住。

**顺带**：`payment.Provider` 接口里 `NotifyEvent.AmountCents` 已有，无需改契约。

---

## P0-2 · 真实微信支付 Provider（上生产前置）

**问题**：`cmd/api/main.go` 里 `PAYMENT_PROVIDER=wechat` 直接返回"尚未实现"。加上上轮的生产校验（生产禁止 mock），**当前服务无法以生产模式启动**。

这不是本轮造成的，是上一轮的生产校验把一个既存事实显式化了：**它从来没具备过上生产条件。**

**具体修法**（按依赖顺序）

1. **验签**：微信支付 APIv3 用平台证书 + `Wechatpay-Signature` 头做 RSA 验签；证书需带**过期自动轮换**（平台证书会轮换，写死会某天突然全部失败）。
2. **报文解密**：回调 body 是 AES-256-GCM 加密的 (`resource.ciphertext`)，用 APIv3 密钥解。
3. **金额核对**：即 P0-1，此处以微信返回的 `amount.total` 为准再比一次。
4. **回调应答格式**：微信要求特定 JSON（`{"code":"SUCCESS"}`）或 5xx 促其重试 —— 与我们的 `WriteProblem` 形状冲突，需要在 `MockPayNotify` 之外单开一个真实回调 handler，**不能复用** mock 那个（mock 那个是给压测用的公开入口）。
5. `routePolicy` 里新增真实回调路由（公开，但来自腾讯 IP 段）。

**验收标准**

- `APP_ENV=prod AUTH_PROVIDER=wechat PAYMENT_PROVIDER=wechat WECHAT_*=<测试商户号>` → 服务**能启动**。
- `APP_ENV=prod PAYMENT_PROVIDER=mock` → 仍**启动失败**（现有校验不能回退）。
- 活服务器：`/api/v1/pay/mock/notify` 在非 mock 配置下返回 404/401（路由不存在）。
- 用微信提供的**沙箱/测试回调样例**（含真实签名）跑通一次；**改一个字节**的签名必须被拒。

**风险提示**：这条是本计划里工作量最大的一条，且依赖**微信支付商户号与 APIv3 密钥**（资质）。在资质到位前，能做的只有把接口、验签骨架、证书轮换逻辑写好并用**自签证书+自造报文**做单测。

---

## P1-3 · `/readyz` 脱敏

**问题**（实测原始响应）

```
{"checks":{"redis":"dial tcp [::1]:6379: connectex: No connection could be made ..."},"status":"unavailable"}
```

内网地址、端口直接外泄；PG 挂掉时 pgx 还会带上 `host= user= database=`。

**具体修法**

`readyz` 只回**固定枚举**，详细错误只进日志：

```go
checks := map[string]string{"pg": "ok", "redis": "ok"}
if err := d.Pool.Ping(ctx); err != nil {
    checks["pg"] = "unavailable"
    logging.From(r.Context()).Error("readyz: pg 不可用", "err", err)   // 原文进日志
}
```

顺带一处设计澄清：`/readyz` 是编排探针，**默认不该对外**。建议在部署清单里把它限制在内网/网关白名单；应用层脱敏是第二道。

**验收标准**

- 变体：把 `"unavailable"` 换回 `err.Error()` → 测试报红。
- 新增测试 `TestReadyz_DoesNotLeakDependencyError`：注入一个返回含 `host=secret-db` 文本的假 Ping，断言响应体**不含**该串、状态码仍 503。
- 变异体验证：新增 **M40**（把 `checks["pg"]` 从 `"unavailable"` 改回 `err.Error()`），期望被上面那条测试抓住。
- **这条必须用假 Ping 注入，不能靠"把 Redis 停掉"** —— 那样测的是环境，不是代码。

---

## P1-4 · 补 4 条守卫空洞（M20/M21/M23/M24）

这四条不是"守卫没用"，而是**从未被验证过**。修法都是**补测试**，不改生产代码（除非补的过程中发现生产代码真错了）。

| 变异体 | 要补的测试 | 测试怎么造场景 |
| --- | --- | --- |
| M20 | `TestRefresh_DBGuard_ConcurrentOnlyOneSucceedsWithoutRedis` | 让 `RefreshStore` 指向一个**不可用**的 Redis（不是 miniredis，是一个必然失败的后端），于是 Lua 那层失效，**只剩 DB CAS 兜底**；20 并发刷同一个 token → 断言恰好 1 个成功 |
| M21 | `TestRefresh_RejectsRowExpiredInDB` | 直接在库里把该行 `expires_at` 改到过去（同时清掉 Redis 缓存），断言刷新被拒且**不吊销家族**（过期不是重放） |
| M23 | `TestRefresh_CompensatesWhenDBCasFails` | 先手工把该行置为 `ROTATED`（模拟 DB CAS 必然失败），同时让 Redis 侧是干净的 → 断言返回错误、且 Redis 里**旧 token 被恢复、新 token 不存在**（`Compensate` 真的跑了） |
| M24 | `TestRefresh_PropagatesNonRowError` | 让 `GetRefreshTokenByHash` 返回一个非 `ErrNoRows` 的错误（例如用已关闭的连接池，或注入一个 fake Querier）→ 断言**不**被当成"令牌不存在"，而是上抛 500 级错误 |

> **实现后记（2026-10-10）**：上面是**计划时拟的名字与手法**，落地后有两处不同，
> 照计划去核对会找不到测试 —— 所以在这里对齐，而不是去改计划本身（计划是当时的意图记录）：
>
> | 拟 | 实际落地 | 为什么换 |
> | --- | --- | --- |
> | M20 `TestRefresh_DBGuard_ConcurrentOnlyOneSucceedsWithoutRedis` | **同名，已落地** | 一致。但"恰好 1 个成功"的机制不是"只剩 CAS"这么简单：`token.go` 把 `Rotate` 失败**降级为纯 DB 路径**（而不是整体拒绝），所以 CAS 才真的成了唯一裁决点 |
> | M21 `TestRefresh_RejectsRowExpiredInDB` | **`TestConsumeRefreshToken_SQLGuards`**（另有 `TestRefresh_CompensatesWhenDBCasFails` 一起抓） | 原计划"在库里改过期时间再清 Redis 缓存"仍然走 service，而 service 的过期预检排在 CAS 之前 —— 过期行在那里就被拒了，SQL 的 `expires_at` 条件照样执行不到。改成**直调 `repo.ConsumeRefreshToken`**，绕过预检才测得到 SQL 语义 |
> | M23 `TestRefresh_CompensatesWhenDBCasFails` | **同名，已落地** | 手法换成**回拨时钟**（`Deps.Now`）骗过预检，让 CAS 因 `expires_at` 失败 —— 这比"手工置 ROTATED"更贴近真实竞争，而且不依赖对 Redis 缓存的清理时机 |
> | M24 `TestRefresh_PropagatesNonRowError` | **同名，已落地** | 手法用假 `failingDBTX`。**没有**用"已关闭的连接池"：关池会连带影响 `t.Cleanup` 里的清理语句，让用例因为"清理失败"变红 —— 那种红是噪音，会教人学会忽略失败 |

**验收标准**

- `python scripts/verify-guards.py` → **38/38**（当前 34/38）。
- 四条变异体全部从"测试仍然全绿"变为"被指定测试抓住"。
- **注意**：如果补测试时发现"其实生产代码就是错的"，那结论要反过来写成缺陷，而不是把测试改成迁就代码。

---

## P2-5 · `service` 包 `t.Skip` 清零

**问题**：三处残留（上一轮只改了 `ratelimit` 与 `httpapi` 两份副本）。

| 文件 | 现状 | 修法 |
| --- | --- | --- |
| `service/token_integration_test.go:41` | 连不上 Redis → Skip | 与 `ratelimit` 包同构：默认 **miniredis**，真机走 `RATELIMIT_TEST_REAL_REDIS=1`，**连接失败 `t.Fatal`** |
| `service/merchant_integration_test.go:45` | 同上 | 同上 |
| `service/commerce_integration_test.go:32` | 未设 `DATABASE_URL` → Skip | 这个是**合理的**（真需要 PG，没有替代品）。修法不是删掉，而是让门禁**断言 PASS 条数下限**（`check-all.sh` 已有此机制，确认覆盖到这三个文件） |
| `ratelimit/ratelimit_test.go:404` | `t.Skip("Redis 未报错…")` | 这条让"地址不泄露"断言变成环境相关。改成对**哨兵类型**的确定性断言（见 P2-6） |

**验收标准**

- `go test ./...` 输出 **0 SKIP**（当前已达成 —— 但那是"因为 Redis 恰好起着"，不是"因为不会 skip"）。
- 用变异证明：把 Redis 停掉后跑 `service` 包，**必须红**，而不是静默跳过。这条本身就是一次变异验证（改环境而非改代码）。
- 全库 `grep -rn "t\.Skip" services/api/internal` 应该只剩下**用途正当**的那一处（PG 缺失），且带注释说明为什么不能改成 Fatal。

---

## P2-6 · `Allow` 的错误只带哨兵

**问题**：注释写着"刻意不把 err 原样上抛"，实现却是 `fmt.Errorf("%w: %v", ErrBackendUnavailable, err)`。

当前**不可达客户端**（中间件只记日志，实测确认），所以不是漏洞；但注释在撒谎，而下一个人会据此做错决定 —— 比如给 `Allow` 加一条新的调用路径并直接回 `err`。

**具体修法**

1. 改成 `fmt.Errorf("%w", ErrBackendUnavailable)`（去掉 `%v`），原始错误用 `logging` 或由调用方 `errors.Is` 后单独记。**注意这会丢失日志里的诊断信息** —— 所以正确做法是：保留一个 `Errorf("%w: %v", ...)` 供**内部日志**使用，对外只回哨兵。实现上建议 `Allow` 返回 `*BackendError`（含 `Unwrap() → ErrBackendUnavailable` 和一个 `Cause` 字段），让中间件既能 `errors.Is` 又不误把 `Cause` 写进响应。
2. 修 `TestAllow_DoesNotLeakBackendAddress`：它现在依赖 dial 失败的**形态**（本机是 timeout 所以"通过"，换成 refused 就红）。改成断言 `errors.Is(err, ErrBackendUnavailable)` 且 `strings.Contains` 检查在**任意**形态下都成立。

**验收标准**

- 新增测试：构造一个错误文本含 `host=10.0.3.11:6379` 的假后端，断言 `Allow` 返回值的 `Error()` **不含**该串（确定性，与环境无关）。
- 变异体验证：新增 **M41**（把 `%w` 换回 `%w: %v`）→ 被上面那条测试抓住。

---

## P2-7 · `X-Request-ID` 取值白名单

**问题**：`RequestID` 原样采信请求头，写进响应头与全部日志。攻击者可指定任意 id 伪造日志关联。

**具体修法**：只接受 `^[A-Za-z0-9_-]{1,64}$`，不合规就丢弃并生成新的。

**验收标准**

- 新增测试：发 `X-Request-ID: ../../evil\ninjected`（含换行会被 Go 拒掉，改测 `A` × 200 与含中文/空格的值）→ 断言响应头里的值与请求头**不同**，且是 UUID 形状。
- 变异体验证：新增 **M42**（去掉校验）→ 被抓住。

---

## P3-8 · 订单状态机收归 `domain`

**问题（既有债务，非本轮新发现）**：`SHIPPED / COMPLETED / REFUNDING / REFUNDED` 四态**无任何写入路径**；支付回调与取消各写手写 SQL 白名单，已产生实质分歧（`domain.TransitionTo` 认为 `CREATED → PAID` 非法，而支付回调直接这么写）。

**为什么算安全问题**：订单状态是资损与纠纷的判定依据。"同一个状态在两条路径上有两套合法集"意味着**审计结论依赖你查的是哪张表**。

**具体修法**：`MarkOrderPaid` / `MarkOrderCancelled` 的内联状态白名单收归 `domain` 的一张转移表，SQL 从表生成；履约四态补上写入路径（发货/完成/退款）。

**验收标准**：状态机穷举测试（5 状态 × 5 状态）覆盖所有写入路径；不存在"SQL 白名单与 domain 表不一致"的组合。

---

## P3-9 · `cover_url` 拒绝内网地址

上轮已加协议白名单（http/https）。残留：`http://169.254.169.254/latest/meta-data/` 这类**云元数据地址**可被存进商品封面；如果将来服务端做缩略图抓取，就是标准 SSRF。

**具体修法**：如果永不服务端抓取，就在契约里写明"封面仅作前端 `<img src>` 引用"，并在校验里**可选**拒绝私网/保留网段（多一层无成本）。若将来要抓取，则必须在抓取前做 DNS 解析 + 私网判定（防 DNS rebinding）。

**验收标准**：新增 **M43**（去掉私网判定）被测试抓住；或明确记录"不抓取，故不适用"。

---

## P3-10 · 前端 Web 端 token 存储

native 端用的是 Keychain（好）。Web 端受 Expo 限制，token 落在 `localStorage` —— **任何 XSS 都能直接读走 refresh token**。

**修法二选一，必须明确选一个并写下来**：
1. 接受它，并在 README 里标为已知限制（配合严格 CSP —— 但注意 API 侧已发 `default-src 'none'`，页面侧需另配）；
2. 改成 httpOnly Cookie 会话 —— 这会牵动 CORS 与 CSRF 设计，是一次架构决策，不该顺手改。

**验收标准**：README「已知约束」里有这一条，且写明选了哪条路。

---

## 门禁：让本轮的东西不会下次就失效

新增的两类验证目前都靠"我记得手动跑"。**没进 CI 的守卫等于没有守卫。**

| 项 | 加进哪 | 说明 |
| --- | --- | --- |
| `govulncheck` | `check-all.sh` 新增一项 | 依赖漏洞是**随时间变化的**（今天 0 条不代表下周 0 条）。建议允许 3 个 module 级不可达漏洞为基线，可达漏洞一律红 |
| `scripts/security-probe.mjs` | `check-all.sh` 的"端到端"档 | 需要活服务器 + PG/Redis，放在 `--quick` 之外 |
| `verify-guards.py --audit` | `check-all.sh` 首项 | **残留自检必须最先跑** —— 上一次变异验证被 SIGTERM 杀掉时留下了 4 处源码污染，是 `--audit` 抓出来的 |
| `check-all.sh` 的 PASS 条数下限 | 现有机制 | 确认它覆盖 `service` 包（103 条），否则那 103 条被 skip 也看不出来 |

**验收标准**

- `bash scripts/check-all.sh` 全绿；故意停掉 Redis 后**必须红**（而不是 skip 之后仍绿）。
- 故意让 `security-probe.mjs` 里的一条断言失效（如把期望的 401 改成 200）→ 门禁必须红。

---

## 执行顺序建议

```
P0-1（金额核对）  ← 最小改动、最大资损面，可以立刻做
   ↓
P1-3 / P1-4 / P2-5 / P2-6 / P2-7   ← 一批"补验证 + 小修"，做完变异验证应到 38/38
   ↓
门禁接入          ← 在改动还热的时候接，避免"以后再接"
   ↓
P0-2（微信支付 Provider） ← 依赖资质，可与上面并行推进
   ↓
P3-8 / P3-9 / P3-10 ← 业务与客户端，按产品节奏排
```

**不要先做 P0-2**。它工作量大、依赖外部资质，而且**不做 P0-1 就先接真实支付，等于把一个已知的资损缺口接进生产**。

---

## 执行记录（2026-10-10，第一轮）

按"执行顺序建议"推进，完成 **P0-1 → P1-3/P1-4/P2-5/P2-6/P2-7 → 门禁接入**，
P0-2 与 P3 未动（原因见文末）。

### 已完成（8 项，全部有验收信号）

| 条目 | 改动 | 验收信号（实测） |
| --- | --- | --- |
| **P0-1** 支付回调金额核对 | `service.HandleNotify` 落库前反查订单并比对金额；新增哨兵 `ErrPaymentAmountMismatch`（→400）；金额不符时**不写 `payment_events`** | 集成测试 3 条（含"错误回调不占唯一键"的复现）＋ **M39 被抓住**＋活服务器 8/8 |
| **P1-3** `/readyz` 脱敏 | `checks` 只回固定枚举 `ok`/`unavailable`；原文只进日志；拆出 `readyzHandler` 以便注入假依赖 | 新增 2 条测试＋ **M40 被抓住**＋停 Redis 实测响应 |
| **P1-4** 补 4 条守卫空洞 | 补 4 条测试（含给 `Deps` 加可注入 `Now`）；同步 4 条变异体的 `expect_fail` | **M20/M21/M23/M24 全部从"未证明"变为被抓住** |
| **P2-5** `t.Skip` 清零 | `service` 包两处 Redis skip 改 miniredis；commerce 的 PG skip 保留并加注释说明为何不能改 | 全库 **304 PASS / 0 SKIP / 0 FAIL**（该数字是当时快照；P3-9 之后为 411，见下）；用 `RATELIMIT_TEST_REAL_REDIS=1 REDIS_ADDR=127.0.0.1:1` 验证连接失败会 **FAIL 而非 SKIP** |
| **P2-6** `Allow` 错误只带哨兵 | 新增 `BackendError`（`Error()` 只回哨兵、原文挂在 `Cause`）；`Limiter.rdb` 收窄到 `redis.Scripter` 以便注入假后端 | 新增确定性泄露断言＋ **M41 被抓住**；中间件改用 `ratelimit.Cause(err)` 记日志 |
| **P2-7** `X-Request-ID` 白名单 | 只接受 `^[A-Za-z0-9_-]{1,64}$`，不合规丢弃并重新生成 | 新增 3 条测试（含"合规值必须保留"的对照）＋ **M42 被抓住** |
| **门禁** 接入 `check-all.sh` | 7 步 → 10 步：新增 `--audit` 残留自检（**排首位**）、govulncheck、安全探针；`MIN_GO_TESTS` 38 → 280（**复核时改为 380**，理由见下）；**SKIP > 0 直接判失败** | `bash scripts/check-all.sh --quick` → **10/10 全绿** |

### 关键数字（本轮实测）

```
变异验证        34/38  →  42/42          （全部被指定测试抓住）
Go 测试         285    →  304 条 / 0 SKIP / 0 FAIL
govulncheck     28 可达 →  0 可达（3 条模块级不可达）
活服务器探针    56/56  PASS=56 FAIL=0 WARN=0 SKIP=0
```

### 两处实测证据（原始响应）

**P0-1**：`total_cents=1137` 的订单收到 `amount_cents=1` 的回调 →
`400 {"type":"about:blank","title":"支付金额与订单不符","status":400,"detail":"支付金额与订单不符"}`，
订单保持 `PENDING_PAYMENT`；随后用正确金额回调 → `200 {"handled":true,...,"status":"PAID"}`。

**P1-3**：停掉 Redis 后 `/readyz` 返回
`{"checks":{"pg":"ok","redis":"unavailable"},"status":"unavailable"}` ——
对比修复前的 `{"checks":{"redis":"dial tcp [::1]:6379: connectex: ..."}}`，内网地址与端口不再出现。

### 修复过程中的两条结论

1. **"兜底路径"最容易变成"从未被验证的路径"**。M20/M21/M23/M24 四条全部落在
   互为备份的第二道防线上：过期预检挡在前面，SQL 的 `expires_at > now()` 永远执行不到；
   Redis 的 Lua 挡在前面，DB 的 `status = 'ACTIVE'` 永远执行不到。
   **两道防线只用一条的话，删掉另一条没有任何测试会红** —— 所以补测试时必须
   主动把第一道"骗过去"（直接调 repo、或注入回拨的时钟），而不是再写一个走正常流程的用例。
2. **`t.Skip` 的清理必须以"连接失败会 FAIL"来证明**，不能以"现在不 skip 了"来证明。
   本轮改用 `RATELIMIT_TEST_REAL_REDIS=1` + 死地址验证了这个性质。

### 未完成（原因）

| 条目 | 状态 | 原因 |
| --- | --- | --- |
| **P0-2** 真实微信支付 Provider | 未开始 | **依赖资质**：需要微信支付商户号与 APIv3 密钥。资质到位前只能写接口骨架 + 用自签证书做单测，无法完成验收标准里的"用真实沙箱回调跑通"。 |
| **P3-8** 订单状态机收归 `domain` | 未开始 | 是重构而非补丁：牵动 4 个状态的写入路径、SQL 白名单与转移表的一致性。风险与工作量都够单独一轮，且**不影响当前已上线路径的正确性**。 |
| **P3-9** `cover_url` 拒绝内网地址 | 未开始 | 小改动，可随时做（若确定永不服务端抓取，也可按计划记为"不适用"）。 |
| **P3-10** 前端 Web token 存储 | 未开始 | 需要产品决策（接受 + README 记录 vs 改 httpOnly Cookie），不适合顺手定。 |

**注**：本轮修完 P0-1 之后，"不做 P0-1 就先接真实支付" 这个前置风险已消除 ——
P0-2 现在可以安全推进（一旦资质到位）。

---

## 执行记录（2026-10-10，第二轮：P3-9）

第一轮把 P0–P2 那批"最小改动 + 最大收益"做完了，**门禁也接上了**。
本轮接着做计划里唯一一个**不依赖资质、不牵动架构**的条目：P3-9。

选它的理由：P0-2 卡资质、P3-8 是重构、P3-10 要产品决策 —— 三条都动不了。
P3-9 是当前唯一能"做完并有验收信号"的一条，留着不做只会积灰。

### 改了什么

| 位置 | 改动 |
| --- | --- |
| `service/product.go` | `validateCoverURL` 新增主机名判定 `rejectNonPublicHost`：字面 IP 走 `ipIsNonPublic`（标准库语义谓词 + 20 条 CIDR 表），域名走后缀表（`.localhost` / `.local` / `.internal`）；主机名先归一化（小写 + 去末尾根点） |
| `service/product_validate_test.go` | 4 条新测试（拒绝表 32 条子用例 / 放行对照表 20 条 / 网段边界表 51 条 / 归一化表用内部双循环、无子用例）；原有"带端口"用例从 `localhost:8080` 改为公网域名 + 端口 —— 它的本意是测端口，加白名单后变成了"测 localhost" |
| `docs/openapi/openapi.yaml` | `MerchantProductRequest.cover_url` 补 `description`：写清公网可达要求，并把**不变式**（服务端从不解引用，只由客户端渲染）写进契约 |
| `scripts/security-probe.mjs` | 新增 **K 组**活服务器探针：先建合法公网封面的商品（正控制）再删，然后 5 种内网形态必须 400 |
| `scripts/verify-guards.py` | 新增 **M43/M44/M45**（分别关掉 IP 判定 / 归一化 / 后缀表），42 → **45 条** |
| `scripts/check-all.sh` | **修契约漂移判据**（见下节"顺带修掉的一个门禁缺陷"） |

### 验收信号（实测）

```
新增测试        4 个函数 / 103 条子用例（另改 1 条既有用例）
Go 测试全库     304  →  411 条 PASS / 0 SKIP / 0 FAIL
                （门禁口径 `--- PASS` 计数：103 条子用例 + 4 个父测试各一条 = +107）
变异验证        45/45（M43/M44/M45 各被指定测试抓住；M37/M38 未被我改动削弱）
活服务器探针    56  →  63 PASS / 0 FAIL / 0 WARN / 0 SKIP（新增 K 组 7 条）
                注意 63 里有 6 条是跨租户组 —— 它需要 2 号商户夹具。夹具原先只
                存在于不入库的 .workbuddy/ 下，所以这个数字当时**无法在干净检出里复现**；
                现已并入 tools/loadtest/seed/seed.sql，门禁也断言探针 SKIP=0。
残留自检        45 条锚点均唯一命中
gofmt / go vet  干净
```

### 顺带修掉的一个门禁缺陷：契约漂移判据

改契约之后跑门禁，第 3 步报红：

```
生成物与 openapi.yaml 不一致。差异：
 packages/api-contract/src/schema.d.ts        | 12 ++++++++++++
 services/api/internal/httpapi/gen/api.gen.go | 13 ++++++++++++-
如果这是你有意改契约的结果，请把生成物一起提交。
```

**但这不是漂移**：连跑两次 `gen.sh`，上面那个差异一行不变 —— 磁盘上的生成物
**就是**契约的产物。

问题出在判据本身：它用 `git diff --exit-code`（基准是 **HEAD**），于是把两件性质
完全不同的事混成了一件：

| | 性质 | 该不该红 |
| --- | --- | --- |
| (a) 生成物与契约不一致 | 真的错 | **必须红** |
| (b) 改了契约、已重新生成、但还没提交 | 正常的工作中间态 | 不该红 |

后果是**每一次有意改契约都会把门禁判红**，而这恰恰是最需要这条检查可靠工作的
时刻；错误信息还会把人往"改提交历史"上引。判据该问的是"生成物是不是契约生成
的"，不是"你提交了没有"。

改法：用**临时索引**比对 —— 先把当前生成物登记进 `GIT_INDEX_FILE` 指向的临时索引、
跑 `gen.sh`、再看「索引 vs 工作区」有无差异。用临时索引而不是 `git add`，是为了
不顺手把用户精心挑好的 staged 状态改掉。

**这个新判据仍然有牙**（两个方向都实测过）：

| 场景 | 结果 |
| --- | --- |
| A：契约已改 + 已重新生成（未提交） | exit 0，并打印提示"相对 HEAD 有未提交改动，提交时别落下" |
| B：生成物被手改/陈旧（不跑 gen.sh） | **exit 1**，打印差异并指出修法 |
| B 之后重跑 gen.sh 还原 | exit 0 |

丢失的只有"生成物没被提交"这一条 —— 那属于提交卫生，且 CI 跑在干净检出上时，
若有人提交了契约却没重新生成，磁盘上的生成物与契约不符，这条检查照样会红。

### 已修：探针的跨租户组在门禁里永远 SKIP

跑探针时发现 H 组（跨租户隔离，6 条）默认打印
`SKIP 跨租户隔离 (未提供 PROBE_TENANT_B_PHONE / PROBE_TENANT_A_PRODUCT_ID)` 就过去了。

原因：`check-all.sh` 直接跑 `node scripts/security-probe.mjs`、**不传**夹具变量；
而探针的退出码只看 `nFail`，**SKIP 不计**。也就是说，跨租户隔离那 6 条
**从来没在门禁里跑过** —— 一路绿着，但从没执行。

这与 Go 测试侧已经修掉的「`t.Skip` 与真通过在退出码上无法区分」是**同一个毛病**：
门禁里已经断言了 Go 测试 `SKIP=0`，但探针的 SKIP 没人管 ——
**同一个不变式在两处实现，只堵了一处**。

**两条修法都已落地**（单独做任何一条都堵不住）：

1. 把 2 号商户（APPROVED）加进 `tools/loadtest/seed/seed.sql` —— 夹具从此跟着种子走，
   **干净检出里也有**。这同时修掉另一个问题：夹具原先放在不入库的 `.workbuddy/` 下，
   于是报告里"63 PASS"这个数字在别人机器上根本复现不出来。
   （密码哈希与 1 号商户**逐字节相同**，所以探针不用再带一个凭据字面量 ——
   它复用已经登录成功过的 `PROBE_MERCHANT_PASSWORD`。）
2. `check-all.sh` 的探针步骤现在显式传夹具变量，并**断言 `SKIP=0`** ——
   只做第 1 条的话，夹具哪天被删或别处又加了 `skip()` 就回到静默状态。

顺带把探针的**生产防呆**补上了：它会写数据（建/删商品、下单、支付回调、refresh 轮换、
触发限流），而"打哪个环境"完全由 `PROBE_BASE` 决定。现在非本机目标默认**拒绝运行**，
必须显式 `PROBE_ALLOW_REMOTE=1` 才放行 —— 这类防呆的价值不在"防住有意为之"，
而在"把一次失误从静默变成响亮"。

### 三条设计判断（都不是显然的）

1. **这一层不是 XSS 防线，也不是 SSRF 防护，别把它说成那个**。
   scheme 白名单已经解决了 XSS；而真正的 SSRF 防护是"服务端永不抓取"。
   纯字符串层面的网段检查**挡不住 DNS rebinding**（攻击者用自己域名指向 127.0.0.1
   照样穿过）。所以代码注释与契约里都明确写了这句 —— 一个被误认为"已解决 SSRF"
   的弱检查，比没有检查更危险：它会让人放心地去加抓取逻辑。
   它真正的作用是**让这条边界在代码里显式存在**，将来加抓取的人会先撞到这张表。

2. **这里可以用黑名单，而 scheme 那里不行 —— 差别在集合是否封闭**。
   URL scheme 是开放集合（浏览器/系统不断新增、还能被协议处理器接管），
   所以只能白名单；"不可路由网段"是 IANA 在 RFC 6890 里维护的封闭清单，
   抄得完，也就黑得住。**同一个"黑名单 vs 白名单"的问题，在两个地方的正确答案相反** ——
   判断依据不是"哪个更安全"，而是"这个集合是否可穷举"。

3. **每条网段都要有"外侧紧邻"的放行用例**。
   只测"内网被拒"抓不到掩码写宽（`100.64.0.0/10` 写成 `/8`）——
   写宽之后内网照样被拒，只是顺带把一大片公网也拒了，测试全绿。
   所以 `AllowsPublicHosts` 里每个被拦网段的上下边界外侧各列一条
   （`100.63.255.255` / `100.128.0.0` 等），`TestIPIsNonPublic_Boundaries`
   再把边界逐个钉死。**这是本轮最值钱的一条**：安全工作里"过宽"和"过松"
   是两种相反的失败，而只看"该拒的拒了"只能发现后一种。

### 仍未完成

| 条目 | 原因 |
| --- | --- |
| **P0-2** 真实微信支付 Provider | 依赖资质（商户号 + APIv3 密钥） |
| 回调报文里的 `ev.Success` 从未被校验 | **既存缺陷**，属 P0-2 范围。mock 恒为 `true` 所以看不出来；接真实 provider 之前必须补（"支付失败"的回调现在会被当成功处理） |
| **P3-8** 订单状态机收归 `domain` | 重构，够单独一轮 |
| **P3-10** 前端 Web token 存储 | 需要产品决策 |

---

## 执行记录（2026-10-10，第三轮：提交前的四轴 review）

按「正确性 / 安全性 / 可维护性 / 测试覆盖」四轴各派一路**只读**复核，然后逐条自己实证 ——
子 agent 的摘要描述的是它"打算查什么"，不是事实。这一轮 15 条里有 2 条复核后**不成立**。

| # | 轴 | 结论 | 级别 | 处置 |
| --- | --- | --- | --- | --- |
| 1 | 覆盖 | 探针跨租户组（6 条）在门禁里永远不会执行：`check-all.sh` 不传夹具变量，而 `skip()` 只加计数、退出码只看 `FAIL` | **P0** | 已修：2 号商户进 `seed.sql` **且**门禁传变量并断言 `SKIP=0` |
| 2 | 维护 | 漂移判据把临时索引写在 `$ROOT/.workbuddy/`。该目录被 gitignore，**干净检出里不存在** → `git add` exit 128，第 3 步在 CI/新克隆上**永远红**（本机因目录恰好存在而看不见） | **P1** | 已修：改用 `mktemp -d` |
| 3 | 正确性 | 契约把 `MockNotifyRequest.amount_cents` 标为可选，新代码却当必填（缺省 → 0 → `<= 0` → 400）。**契约在撒谎** | P2 | 已修：改 `required: [order_no, amount_cents]` 并重新生成；`derefInt64` 随之成死代码，删掉 |
| 4 | 维护 | `check-all.sh` 注释写「实测 304 PASS」「下限 280，留约 8% 余量」，实测是 **411**，280 只保值 68% | P2 | 已修：注释改 411，`MIN_GO_TESTS` 280 → **380**（≈92%） |
| 5 | 维护 | `commerce.go` 函数头「先落事件再改状态：万一改状态失败，事件已留下痕迹，可据此重放」与代码直接矛盾（两个 `ErrNoRows` 分支都显式 `Rollback`） | P2 | 已修：改写为"事件与状态变更同生共死" |
| 6 | 覆盖 | `TestHandleNotify_AmountEqualSucceeds` 的注释声称能抓"插入写在核对之前会占住唯一键" —— **不成立**：整段在一个事务里，拒绝路径会 Rollback | P3 | 已修：注释改成它真正抓的东西（M39），并写明它抓不到"插入顺序写反" |
| 7 | 维护 | 本文档引用的 `TestRefresh_RejectsRowExpiredInDB` 不存在 | P2 | 已修：补"计划名 → 落地名"对照表（不改计划本身 —— 它是当时的意图记录） |
| 8 | 维护 | 文档指向 `.workbuddy/pg-port-override.yml` / `.workbuddy/probe-tenant-b-fixture.sql` —— 都不入库，干净检出/CI 里是失效指针 | P2 | 已修：夹具并入 `seed.sql`；端口覆盖改成"个人配置 + 内容内联给出" |
| 9 | 维护 | `--quick` 下汇总仍打印「全部 10 项通过」，实际只跑 7 项 | P3 | 已修：汇总改成"已执行的 N 项"并列出未执行项数 |
| 10 | 维护 | `verify-guards.py --help` 里「省一次 221 测试的等待」已过期 | P3 | 已修：**去掉数字**（这类计数写进散文必然过期，改对一次还是会再过期） |
| 11 | 安全 | 探针会写目标环境，却没有任何防呆；`PROBE_BASE` 打错一次就是一次真实写入 + 真实限流封禁 | P2 | 已修：非本机目标默认**拒绝运行**，需显式 `PROBE_ALLOW_REMOTE=1` |
| 12 | 正确性/安全 | `nonPublicCIDRs` 漏了 `192.88.99.0/24`、`2002::/16`、`64:ff9b::/96`、`100::/64`、`2001::/23` | P3 | **不修表、改注释**：漏项逐条写进"已知不覆盖"，并说明 `fec0::/10` 为何刻意不列（改它得同时改边界断言）。服务端不抓取，都是理论项 |
| 13 | 安全 | `ev.Success` 从未被校验 | — | **不改**：既存缺陷，属 P0-2 范围，已记入上表 |
| 14 | 安全 | `/readyz` 公开且不在限流表内，每请求两次 Ping | P3 | **不改**：既存设计；脱敏已做，限流属部署/产品决策 |
| 15 | 维护 | 契约未写明"挡不住十进制 IP 与重定向" | P3 | **驳回**：契约那段是给商户看的，实现层面的不完整披露属于代码注释（已写入） |

### 复核后证伪的两条

- 「`/readyz` 的 `logging.From(ctx)` 可能返回 nil」—— `logging.From` 有 `slog.Default()` 兜底，永不返回 nil。
- 「`BackendError.Unwrap()` 会让 `%+v` / `slog.Any` 绕过 `Error()` 泄露原文」—— 全库无 `%+v`；
  `slog` 对 `error` 调的是 `Error()`；`Cause()` 只有 `ratelimit_middleware.go` 一处日志在用。

（写下来是因为"P1 被证伪"与"P1 修好了"同样有价值 —— 否则下一轮还会有人重新报一遍。）

### 顺手做的一次"步骤之间会不会互相踩"的检查

第 9 步（探针）加进来之后，它成了门禁里唯一**会主动打满限流**的一项
（J 组连打 15 次错误登录触发 429），而紧随其后的第 10 步（端到端验收）第一步
就是微信登录。两者会不会互相踩？如果会，那这门禁就是"跑一次红一次、只能靠改顺序蒙过去"。

**实测：不会。** 探针跑完后立刻登录 `wechat/login` 三次都是 200。
原因在 `httpapi/ratelimit_policy.go` 里已经写明了：三个登录入口分属三个 `action`，
**刻意不共用**额度 —— "共用的话，商户端被撞库会把管理员入口的额度一起吃掉"。
一个设计判断在这里免费挡住了一次跨步骤污染。

这类检查值得固化成习惯：**每往门禁里加一项，先问"它会改变什么共享状态"**
（限流额度、种子数据、容器、端口），再看排在它后面的项是否依赖那份状态。

## 执行记录（2026-10-10，第四轮：把门禁真正跑通）

前三轮把"守卫有没有牙"查得很细，但 **`check-all.sh` 自己能不能端到端跑完**从没被验证过 ——
它一直在 `--quick` 或单项模式下用。第一次完整跑 10 项，挖出 **5 处缺陷，全在脚本层**，
其中 1 处是**本轮新引入的回归**（已修）。

### 五处缺陷

| # | 位置 | 缺陷 | 后果（实测） | 处置 |
| --- | --- | --- | --- | --- |
| 1 | `verify-perf-gate.sh` | `PROBE_PORT` 默认 **8081**，而 `preview-web.mjs` / `acceptance-app.mjs` 的默认端口也是 8081 | 第 8 步 `kill_port 8081` 先把 web 预览干掉再绑劣化版 API；第 10 步随即连不上预览。**门禁永远不可能 10/10**，而红的那一项叫"端到端验收" | 默认改 **8091** |
| 2 | `acceptance-app.mjs` | 启动前 `rm -rf OUT/.profile`，跑过几轮后那是 190+ 文件的批量删除 | 被本机沙箱的批量删除守卫拦下（`SAFE_DELETE_BULK_CONFIRM_REQUIRED`），异常抛在任何一步之前 → 只记一条"执行中断"，浏览器都没起 | 改 `mkdtemp` 一次性目录；清理挪到报告写完之后并吞掉失败 |
| 3 | `perf.sh` / `dev.sh` | `docker compose -f deploy/docker-compose.dev.yml up -d` 用**显式 `-f`** | Compose 的规则：给了 `-f` 就不再读 `COMPOSE_FILE`、也不合并 override → 按仓库的 5432 映射**重建**容器。本机 5432 在 Windows 保留端口段，绑不上 → PG 进 `Created` 状态彻底起不来 → 之后所有依赖库的步骤全红 | 新增 `scripts/lib/dev-compose.sh` 统一解析，个人覆盖走 `PG_COMPOSE_OVERRIDE=<文件>` |
| 4 | `tools/loadtest/scenarios/smoke.js` | **本轮新引入**：新增的"金额不符必须 400"断言被 k6 计入 `http_req_failed` | 阈值是 `rate<0.01`，实测单次冒烟即 15.13% → 健康服务器上也红。**一条"守卫正常工作"的证据把门禁判红了** | 用 `rawHttp.expectedStatuses({min:200,max:399}, 400)` + 逐请求 `responseCallback` |
| 5 | `load.js` × 限流（**设计冲突，非机械缺陷**） | `browse()` 匿名 → 全部读请求共用一个 IP 桶，需要约 8000 次读/分钟，默认阈值 120 次/分钟 | 实测 ≈98% 的读请求吃 429、`http_req_failed` 95%，三路场景全红。**测的是限流器，不是应用**；且与基线不可比（`baselines/load.json` 记录于限流上线之前，`http_req_failed=0`） | 压测环境放宽阈值（见下） |

### 缺陷 4 的试错过程（值得记下来）

- `tags: { expected_response: 'true' }` 在 k6 v1.7.1 上**无效**（实测 8/8 仍算失败）。
- 有效写法是原生模块的 `http.expectedStatuses(...)` + 逐请求 `responseCallback`：
  对照实验设计成可判别（一半声明为期望、一半不声明）→ 实测 `http_req_failed: 50.00% 4 out of 8`。
- 坑中坑：`tools/loadtest/lib/auth.js` 自己**封装**了一个 `http`（为统一补 `Origin` 头），
  它不带 `expectedStatuses`。要用 `import rawHttp from 'k6/http'`；
  请求照旧走 lib 的 `http`（保住补头），只有"预期状态码"的声明取自原生模块。

### 缺陷 5 的处理：放宽压测环境的限流阈值，而不是关掉

`load.js` 的 `browse()` 不带 token（这是刻意的：浏览就该是匿名路径），
而读接口按 IP 建桶。这不是"改个数字"能两全的事，所以做了个明确的选择：

- **放宽**（`RATE_LIMIT_MAX=1000000`）而不是 `RATE_LIMIT_ENABLED=false`：
  关掉会让限流中间件整条跳过，等于把被测路径摘掉一段；给个大阈值则限流**照常执行**
  （Lua 照跑、每条请求都过一遍），只是不在这个量级上触发 429。
- 落在两处压测专用配置：`scripts/verify-perf-gate.sh` 的探针 API、`deploy/docker-compose.perf.yml`。
- **不动 `scripts/api.sh` 的默认值**：开发环境的 API 必须保留限流，
  否则探针里那些"打了 15 次错误登录必须 429"的断言会失去意义。
- 残留（已知、已记录）：直接对着 `scripts/api.sh` 起的 :8080 跑 `bash scripts/perf.sh`
  仍然会红，且红在限流上。`perf.sh` 的头部注释已写明要带上这两个变量。

> 这一条属于**需要工程师确认的产品/运维决策**：当前的取舍是"压测环境放宽限流"。
> 另一条路是把 `browse()` 改成带 token（每个 VU 一个桶），但那样压的就不是匿名浏览路径了。

### 一处我先判错、随后推翻的结论

我曾把 `测试商品 1 stock=2000050` 当成集成测试造成的漂移。重灌种子后仍是 2000050
→ 它是**种子自身的设定**（3 个 SKU：1000000 + 1000000 + 50）。真实漂移只有
`users` 表的行数（种子 3 行，跑完 go test 后上千行、且不清库）。
**教训：看到反常数字先跟种子对一遍，再下"漂移"的结论。**




### 缺陷 5 的闭环：按仓库规程重录基线

`baselines/load.json` 记录于**限流上线之前**，而限流给每条请求都加了一次 Redis Lua 往返 ——
系统已经变了，旧基线不再是有效参照。按仓库自己的规程重录（`RECORD_BASELINE=1 RUNS=3`
取 3 次中位数），并且**录在放宽限流的实例上**，否则录进去的是 429 的延迟。

| 指标 | 旧（2026-10-02） | 新（2026-10-10） | 变化 |
| --- | --- | --- | --- |
| `http_reqs.rate` | 174.12 /s | 173.73 /s | **-0.2%** |
| 总 p95 | 2.846 ms | 3.573 ms | +25.5% |
| 总 p99 | 3.323 ms | 6.115 ms | +84.0% |
| `browse_read` p95 | 1.696 ms | 3.058 ms | +80.3% |
| `order_write` p95 | 3.255 ms | 4.513 ms | +38.6% |
| `login_write` p95 | 1.820 ms | 3.492 ms | +91.9% |
| `http_req_failed` | 0 | 0 | 持平 |

**怎么审这份新基线**：吞吐几乎没动（-0.2%）而延迟全线上升 —— 这正是"每条请求多付一次
Redis 往返"的形状（容量没被吃掉，单请求成本抬高）。若反过来是"吞吐掉一半、延迟不变"，
那就是有东西被阻塞了，这个解释盖不住，得继续查。

⚠️ 这条不只是数字，还是**运维决策的依据**：限流把 p99 抬了将近一倍（3.32 → 6.11 ms）。
如果读接口有 p99 预算，得先确认 6.11 ms 在预算内 —— 否则"加了限流"会以最不显眼的方式
吃掉 SLO。

### 一次"看起来像 bug、其实是我的输出层在撒谎"的排查

准备提交时扫到 `check-all.sh` 里这行写法可疑：

    PROBE_TENANT_B_PHONE="${PROBE_TENANT_B_PHONE:-13800000001}"

**显示出来**的赋值目标与下一行 `export` 的名字对不上，看着就是一处会让该变量导出成
空值、跨租户那 6 条 check 静默 skip 的真 bug。机械核对推翻了它：

- 字节计数：可疑名字出现 **0** 次，而 `PROBE_TENANT_B_PHONE` 出现 **3** 次；
- 布尔断言：第 337 行与期望字节串逐字节相等，返回 **True**。

**字符串被显示层改了，字节计数和布尔没有。** 这是本机那条老坑的又一次应验：
凡是要判断"读到的值是什么"，中间必须有一次哈希/布尔/计数比较 ——
这一次它差点让我去"修"一段完全正确的代码，而且是往坏里修（把对的改成错的）。

### 顺手补的一处文档缺口

`check-all.sh` 的「环境」节原来只列了 `DATABASE_URL` / `REDIS_ADDR` / `PYTHON_BIN`，
而它第 8 项起 PG 走的是 `PG_COMPOSE_OVERRIDE`。在 5432 被保留端口段占用的机器上，
漏设它的表现恰好就是本轮踩过的那个坑（按 5432 重建 PG → 起不来 → 后面全红），
而红的那几项看起来像"应用坏了"。已补进「环境」节，并写明**不设时的后果**，
而不只是"有这么个变量"。
