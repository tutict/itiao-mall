# 安全加固报告（2026-10-10）

范围：`itiao-mall` 全仓（Go 后端 `services/api` + Expo 前端 `apps/`）
方法：攻击面逐项审计 → 用真实工具拿证据 → 修 → 补守卫 → 变异验证

**一句话结论**：找到 7 个真实问题，修掉 6 个，其中 2 个是 P0（一个"免费下单后门"、一个内存耗尽 DoS）。
另有 1 个需要你拍板（Go 工具链升级）。审计过程中还顺带发现**限流测试曾整体静默跳过** ——
那是比"未证明"更值得警惕的一类问题，见第八节。

> 相关文档：[`task13-delivery.md`](task13-delivery.md)（限流 / refresh 轮换原子化 / 商户商品审核的交付说明）、
> [`../README.md`](../README.md)（项目总览）。

**本文结构**：结论先行 → P0-1/P0-2（生产禁 mock）→ P0-3（请求体上限）→ P1（安全响应头）→
P2（封面 URL）→ P0-4（Go 工具链）→ 改动文件 → 测试与变异验证 → 没做的部分 → 复现命令 → 新增配置项。

---

## 一、结论先行

| # | 问题 | 等级 | 证据 | 状态 |
|---|---|---|---|---|
| 1 | 生产环境允许 mock 支付 → **任意订单可标记为已支付** | **P0** | `PAYMENT_PROVIDER` 默认 `mock`；`validate()` 只校验枚举；`Mock.ParseNotify` 不验签；`/api/v1/pay/mock/notify` 是公开路由 | ✅ 已修 |
| 2 | 生产环境允许 mock 认证 → **任意 code 可登录任意账号** | **P0** | `AUTH_PROVIDER` 默认 `mock`；`MockExchanger` 把任意 code 哈希成 openid | ✅ 已修 |
| 3 | 无请求体大小上限 → **内存耗尽 DoS** | **P0** | 全库零处 `MaxBytesReader`；oapi-codegen 把 body 整个读进内存 | ✅ 已修 |
| 4 | Go 工具链 28 个可达漏洞（含 net/http HTTP/2 系列 DoS） | **P0** | `govulncheck`：28 条全部 `Fixed in go1.26.1` | ⚠️ 待你操作 |
| 5 | 安全响应头全部缺失 | P1 | 全库 grep 零命中 | ✅ 已修 |
| 6 | `MaxHeaderBytes` 未设（默认 1 MiB × 连接数） | P1 | `http.Server` 未配置 | ✅ 已修 |
| 7 | 商品封面 URL 不校验协议 | P2 | 只校验长度 ≤500 | ✅ 已修 |

**审计过但确认没问题的**（这部分同样重要，避免重复劳动）：

| 攻击面 | 结论 |
|---|---|
| SQL 注入 | ✅ 干净。全库无一处裸拼接，sqlc 全参数化 |
| JWT 算法混淆 | ✅ 干净。`WithValidMethods` + `typ` + role 白名单三重校验 |
| 密码存储 | ✅ 干净。bcrypt DefaultCost；**72 字节截断陷阱已有专门处理** |
| 用户枚举 | ✅ 干净。登录不区分"用户不存在"与"密码错误" |
| 跨租户越权 | ✅ 干净。商户侧 SQL 全带 `merchant_id`，越权回 404 而非 403 |
| CORS 顺序 | ✅ 干净。`CORS` 在 `authGate` 之前，`Vary: Origin` 无条件加 |
| 内部错误外泄 | ✅ 干净。`domain.UserFacing` 白名单机制已生效 |
| 前端 token 存储 | ✅ 合理。native 走 Keychain/Keystore；web 走 localStorage（Expo 官方限制，已在代码注释说明） |
| 日志泄露敏感字段 | ✅ 干净。无 password/token/secret 入日志 |

---

## 二、P0-1 / P0-2：默认值在生产环境恰好是最弱配置

### 问题

两个 provider 的默认值都是 `mock`，而 mock 实现**完全不设防**：

```go
// internal/payment/mock.go —— 不验签，任何 order_no 都返回 Success: true
func (*Mock) ParseNotify(_ context.Context, body []byte, _ http.Header) (NotifyEvent, error) {
    // ... 只检查 order_no 非空
    return NotifyEvent{Success: true, ...}, nil
}

// internal/auth/wechat.go —— 任意 code 都哈希成一个 openid
func (MockExchanger) Exchange(_ context.Context, code string) (WechatUser, error) {
    sum := sha256.Sum256([]byte(code))
    return WechatUser{OpenID: "mock_openid_" + hex.EncodeToString(sum[:8])}, nil
}
```

而 `validate()` 只校验了枚举合法性：

```go
switch c.PaymentProvider {
case "mock", "wechat":
default:
    return fmt.Errorf(...)   // 只管"取值是否合法"，不管"生产能不能用"
}
```

### 为什么这个组合特别危险

**忘记配置的代价是不对称的**：

- 漏设 `PAYMENT_PROVIDER` → 服务**正常启动**，下单流程**看起来完全正常**，没有任何一条现有测试变红
- 只有在有人主动 POST `/api/v1/pay/mock/notify` 时才暴露
- 而那个端点是 `levelPublic`（公开路由），因为 mock 支付需要客户端"跳转"过去完成支付

对比 `JWT_SECRET`：它有一条生产校验，所以"忘记配置"会在启动时炸。`PAYMENT_PROVIDER` 没有 —— 这个不一致本身就是线索。

### 修法

按 `JWT_SECRET` 的同构方式加校验，并留一个**显式命名**的逃生门：

```go
func (c *Config) validateMockProviders() error {
    if !c.IsProd() || c.AllowMockProvidersInProd {
        return nil
    }
    // 逐条点名是哪个 provider 有问题（部署窗口里没人有空逐个变量去猜）
    ...
}
```

启动日志也加了痕迹：

```
WARN ⚠ 生产环境正在使用 mock provider（由 ALLOW_MOCK_PROVIDERS_IN_PROD 显式开启）：
     任意 code 可登录、任意订单可被标记为已支付。切勿用于真实业务。
```

### ⚠️ 一个必须告诉你的连带结论

加上这条校验之后，**这个服务目前无法以生产模式启动**：

```
PAYMENT_PROVIDER=mock + APP_ENV=prod  → 启动失败（新校验）
PAYMENT_PROVIDER=wechat + APP_ENV=prod → 启动失败（cmd/api/main.go:162 "尚未实现"）
```

**这不是我加的校验造成的障碍，而是它暴露的一个既存事实**：这个服务从来没有具备过
上生产的条件 —— 只是以前它不会告诉你。

你的三个选择：

| 选择 | 做法 | 适用 |
|---|---|---|
| A. 演示/验收环境 | `ALLOW_MOCK_PROVIDERS_IN_PROD=true` | 给客户看、内部验收。日志会有 Warn |
| B. 上真实生产 | 实现微信支付 `Provider`（需商户号 + APIv3 证书） | 真业务 |
| C. 保持现状 | 继续用 `APP_ENV=dev` 跑 | 本地开发 |

---

## 三、P0-3：请求体无上限（内存耗尽 DoS）

### 问题

oapi-codegen 生成的解码逻辑是：

```go
json.NewDecoder(r.Body).Decode(&body)
```

它会把 body **整个**读进内存。而我 grep 了全库：**零处** `MaxBytesReader`。

后果：任何人 POST 一个 1 GiB 的 body 到 `/api/v1/auth/login`
（公开端点，`authGate` 只读 `Authorization` 头、根本不看 body）就能让这台机器分配 1 GiB。

`Server.ReadTimeout`（15s）挡不住：15 秒足够在千兆网卡上灌进接近 2 GB。

这是**最廉价的一类 DoS**，不需要任何技巧。

### 修法（三道，缺一不可）

```go
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
    // 1. 有 body 的方法必须带 Content-Length，否则 411
    //    chunked 在读完之前不知道多大，"先看长度再决定"对它无效 —— 直接拒掉
    // 2. Content-Length 超限 → 413，一个字节都不读
    // 3. MaxBytesReader 兜底，防客户端撒谎（声明 10 字节、实发 100 KiB）
}
```

**为什么拒绝 chunked 而不只是"限制它"**：这是 JSON API，不是流式上传服务，
没有一条业务路径需要 chunked 请求体。拒掉它等于从根上消除"绕过长度检查"这整类手法。

**中间件顺序**（这个位置是有讲究的）：

```
CORS → authGate → rateLimitGate → BodyLimit → merchantStatusGate → handler
                        ↑ 必须在这里
```

BodyLimit 排在限流**之后**：排到前面的话，攻击者只要把 Content-Length 写超限，
就能在不触发任何限流计数的情况下持续打目标 —— 一个白送的绕过。

### 一个诚实的边界说明

第三道（`MaxBytesReader`）触发时，下游拿到的是解码错误，最终回 **400 而不是 413**。
这是有意的折中：这条路的状态码精度差一点，但**安全目标（不分配超量内存）已经达成**，
而为了把 400 改写成 413 需要包一层 `ResponseWriter` 去猜"这个 400 是不是因为超限"——
那点复杂度换不来安全收益。（oapi-codegen 的错误类型没有 `Unwrap`，`errors.As` 穿不透。）

---

## 四、P1：安全响应头

### 问题

`X-Content-Type-Options` / `X-Frame-Options` / `CSP` / `Referrer-Policy` /
`Cross-Origin-Resource-Policy` 全部缺失。

一个纯 JSON API 为什么需要这些 —— 最实际的一条是 `nosniff`：
没有它，浏览器会**忽略** `Content-Type` 去嗅探内容，而我们的错误体里有
`type`/`title` 字段（RFC7807），是可控输入能影响到的位置。嗅探 + 渲染 = XSS 的经典组合。

### 实现中踩到的坑（值得记下来）

第一版是"在 `next.ServeHTTP` 之前 `Set` 一遍"。写完测试立刻发现不牢：

**下游任何一次同名写入都能覆盖掉它。** 而安全头一旦被覆盖，
"浏览器一定收到了 nosniff"这句话就不成立了 —— 它的价值全在"声明性"上。

改成包装 `ResponseWriter`，在 `WriteHeader` **真正落地的瞬间**重新刷一遍：

```go
func (w *enforcingWriter) WriteHeader(code int) {
    w.commit()   // 把安全头刷成权威值（此时头还没发出去，Set 仍有效）
    w.ResponseWriter.WriteHeader(code)
}
func (w *enforcingWriter) Write(b []byte) (int, error) {
    w.commit()   // ← 这条不能少
    return w.ResponseWriter.Write(b)
}
```

`Write` 里那次 `commit` 是关键：handler 直接 `Write` 而不调 `WriteHeader` 时，
状态码由 net/http 隐式补 200，只处理 `WriteHeader` 的实现会**整条路径漏掉**。
这是安全头实现里最典型的漏洞形态，专项测试把它钉住了。

另外加了 `Unwrap()`，让 `http.ResponseController` 能找到原始 writer ——
少了它，将来加 SSE 或 WebSocket 会遇到"莫名其妙不工作"。

**刻意没加 HSTS**：它要挂在域名级 TLS 终止点上，而本服务跑在同机 HTTP 后面
（本地 `:8080`、容器内 `:8080`）。在这里加要么无效（HTTP 响应里的 HSTS 被浏览器忽略），
要么把 localhost 钉成 HTTPS 而让人没法调试。**它属于部署清单，不属于应用。**

---

## 五、P2：封面 URL 协议白名单

`cover_url` 只校验了长度。而这个字段会进入前端渲染路径
（`<Image source>` / `<img src>`，将来也可能有人顺手塞进 `<a href>` 做"点击看大图"）。

只做黑名单（禁止 `javascript:`）追不上变体：scheme 大小写不敏感、
可以带前导空白与控制字符。改成**白名单只有两个值**：

```go
switch strings.ToLower(u.Scheme) {
case "http", "https":
default:
    return domain.UserFacingf("封面地址必须以 http:// 或 https:// 开头", domain.ErrValidation)
}
if u.Host == "" { ... }   // "http:///x.png" 能过 scheme 但没主机名
```

文案刻意点出期望形态：商户最常犯的错是从资源管理器复制了本地路径，
告诉他"必须 http(s):// 开头"比"地址非法"有用得多。

---

## 六、P0-4：Go 工具链漏洞（**需要你操作**）

```
$ govulncheck ./...
Your code is affected by 28 vulnerabilities from the Go standard library.
This scan also found 8 vulnerabilities in packages you import and 13 in modules
you require, but your code doesn't appear to call these.
```

**28 条全部是标准库，全部 `Fixed in: go1.26.1`**，当前是 `go1.26.0`。
分布：

| 包 | 条数 | 代表性问题 |
|---|---|---|
| `net/http` | 10 | HTTP/2 HPACK 编码器竞态导致崩溃、双倍流控返还、重复窗口变更耗尽 CPU |
| `crypto/x509` | 6 | 畸形证书导致 panic、name constraint 检查绕过 |
| `crypto/tls` | 4 | ECH 外层扩展引用校验 |
| `net/url` | 2 | IPv6 主机字面量解析错误 |
| `net/textproto` | 2 | MIME 头解析绕过内存限制 |

`net/http` 那批是**远程可达的 DoS** —— 正是这次加固要防的东西，但它们不在我们的代码里，
只能靠升级工具链修掉。

**这需要你操作**（下载安装 Go 1.26.1+），我不能替你装系统级工具链：

```bash
# 升级后把 go.mod 的版本声明也改掉
cd services/api && go mod edit -go=1.26.1
```

改完之后 `govulncheck ./...` 应当归零。**在此之前，这一条不能算修好。**

---

## 七、改动文件

### 新增

| 文件 | 行数 | 职责 |
|---|---|---|
| `internal/httpapi/hardening.go` | 179 | 安全响应头（含强制覆盖）+ 请求体上限 |
| `internal/httpapi/hardening_test.go` | 344 | 12 个测试 |
| `internal/config/config_test.go` | 199 | 3 个测试 |
| `internal/service/product_validate_test.go` | 90 | 2 个测试（含 23 个 URL 变体） |

### 修改

| 文件 | 改了什么 |
|---|---|
| `internal/config/config.go` | `AllowMockProvidersInProd` / `MaxBodyBytes` / `MaxHeaderBytes`；`validateMockProviders()`；`MockProvidersAllowed()` |
| `internal/domain/errors.go` | `ErrPayloadTooLarge`(413) / `ErrLengthRequired`(411) |
| `internal/httpapi/response.go` | 413 / 411 映射 |
| `internal/httpapi/router.go` | 挂 `SecurityHeaders`、`BodyLimit`；`Deps.MaxBodyBytes` |
| `internal/service/product.go` | `validateCoverURL()`（协议白名单 + 主机名） |
| `cmd/api/main.go` | `MaxHeaderBytes`；mock provider 启动告警 |
| `scripts/verify-guards.py` | M31–M38 变异体；`--packages` 参数 |
| `internal/ratelimit/ratelimit_test.go` | 测试辅助改用 `miniredis`，消除 **10 个静默跳过**（见第八节） |
| `internal/httpapi/ratelimit_test.go` | 同上，消除 **1 个静默跳过**（429 响应头用例） |

---

## 八、测试与变异验证

**新增 17 个测试函数**（全库从 103 → 120）：

| 文件 | 函数 | 覆盖重点 |
|---|---|---|
| `config_test.go` | 3 | 生产禁止 mock（7 个环境组合）、逃生门、体积上限下限 |
| `hardening_test.go` | 12 | 安全头覆盖面/覆盖语义/隐式 200；body 上限的 413/411/边界/撒谎客户端/方法白名单 |
| `product_validate_test.go` | 2 | 23 个 URL 变体（含大小写混淆、前导空白、`data:`、`file:`、协议相对） |

**变异验证**（`scripts/verify-guards.py`，新增 M31–M38）：

| 编号 | 变异 | 结果 |
|---|---|---|
| M31 | 生产不再拦住 mock provider | ✅ 抓住 |
| M32 | 逃生门判断写反 | ✅ 抓住 |
| M33 | 安全响应头不再强制覆盖 | ✅ 抓住（2 条断言） |
| M34 | 请求体上限不做 Content-Length 预检 | ✅ 抓住（2 条断言） |
| M35 | 边界写成 `>=` | ✅ 抓住 |
| M36 | 长度校验漏掉 PUT/PATCH | ✅ 抓住（2 条断言） |
| M37 | 封面地址改黑名单 | ⏳ 待验证（需 PG） |
| M38 | 封面地址不校验主机名 | ⏳ 待验证（需 PG） |

**M36 是一次真实的"未证明"**，记下来当教训：它最初写的是"白名单扩大到 GET"，
假想的危害是误伤正常浏览请求 —— 但实测发现扩大之后**行为完全不变**
（GET 的 `ContentLength` 是 0，两个检查都不触发）。那个变异体恒为"未证明"，
比没有更坏，因为它占着位置。

改成"漏掉 PUT/PATCH"（更新商品走 PUT，这条路径上没有任何其他测试覆盖）之后立刻被抓住。
**写变异体之前先问：它描述的后果真的会发生吗。**

### 顺带发现：限流测试曾经**整体静默跳过**

跑变异验证时，M16 等限流相关的变异体恒为"未证明"。追下去发现根因不在变异体，
而在**测试根本没被执行**。

`internal/ratelimit` 与 `internal/httpapi` 两处测试辅助函数都是"连不上 Redis 就 `t.Skipf`"，
而本机没有 Redis（Docker 未起、也没装独立 Redis）。后果：

| 现象 | 真相 |
|---|---|
| `go test` 退出码 0、报 PASS | 看起来"限流全绿" |
| 实际 **10/13** 个限流测试没执行 | 滑动窗口、Lua 原子性、并发判定、降级方向 —— **核心逻辑一条都没验证过** |
| `httpapi` 的 `TestRateLimitGate_Returns429WithHeaders` 也跳过 | 它是唯一断言"429 带 `Retry-After` 与 `X-RateLimit-*`、detail 不泄露内部结构"的用例 |
| `check-all.sh` 的 `MIN_GO_TESTS` 下限 | 只数 PASS 条数，数不出被 skip 掉的那些 |

这是项目铁律「**`t.Skip` 与真通过在退出码上无法区分**」的第三次重演 —— 而这次丢掉的恰好是
**限流器本身**。更坏的是它污染了变异验证：施加变异后"测试仍全绿"，被记成"守卫是假的"，
实际是守卫压根没跑。

**修法**：两处辅助函数都改成默认走 `miniredis`（纯 Go 的内存 Redis，无需外部进程，支持 ZSET 与 EVAL），
真机 Redis 只作可选覆盖（`RATELIMIT_TEST_REAL_REDIS=1`）。连接失败由 `t.Skip` 改成 `t.Fatal`。

**结果**（本机实测）：

| 包 | 修前 | 修后 |
|---|---|---|
| `internal/ratelimit` | 3 PASS / **10 SKIP** | **13 PASS / 0 SKIP** |
| `internal/httpapi` | 46 PASS / **1 SKIP** | **47 PASS / 0 SKIP** |

耗时就势还从 23s 降到 7.8s（少了连真 Redis 的开销）。

**仍待处理**：`internal/service/token_integration_test.go` 与 `merchant_integration_test.go`
里还有同样的"连不上 Redis 就 skip"。它们依赖 PG，需在 PG + Redis 环境就绪时一并改成 miniredis。

---

## 九、没做的部分（明确交代）

| 没做 | 为什么 |
|---|---|
| **升级 Go 工具链** | 需要你下载安装系统级工具链，我不能替你装。这是 28 条漏洞的唯一修法 |
| **实现微信支付 Provider** | 需要商户号 + APIv3 证书。这是 P0-1 的另一半（`PAYMENT_PROVIDER=wechat` 目前直接报"尚未实现"） |
| **HSTS** | 属于部署清单。加了要么无效，要么破坏本地调试 |
| **前端 localStorage 加固** | Expo web 的官方限制（无 httpOnly cookie 通路）。要根治得改成 BFF 模式，是架构级改动 |
| **`order_status` 状态机收归 domain** | 上一轮就挂着的历史债（`SHIPPED/COMPLETED/REFUNDING/REFUNDED` 四态无写入路径）。它是**业务逻辑风险**不是安全漏洞，本轮没动 |
| **限流 key 的内存增长曲线** | 未测。大量不同 subject 会留下多少 key 是未知数 |
| **依赖供应链扫描以外的部分** | 前端 `npm audit` 未跑（沙箱里 npm 会崩，见项目记忆） |

---

## 十、复现命令

```bash
cd services/api

# 依赖漏洞扫描
go install golang.org/x/vuln/cmd/govulncheck@latest
"$(go env GOPATH)/bin/govulncheck.exe" ./...

# 新增测试
go test -count=1 ./internal/config/... ./internal/httpapi/...
go test -count=1 -run 'TestValidateCoverURL' ./internal/service/

# 变异验证（PG 不可用时收窄到不依赖 DB 的包）
python scripts/verify-guards.py --from 31 --to 36 --packages "./internal/config/... ./internal/httpapi/..."

# 残留自检（跑验证前必做）
python scripts/verify-guards.py --audit
```

## 十一、新增配置项

```bash
# 生产环境禁止 mock provider 的显式逃生门。默认 false。
# 打开时启动日志会 Warn，且校验被跳过。
ALLOW_MOCK_PROVIDERS_IN_PROD=false

# 请求体上限（字节）。1 MiB = 最大合法请求（创建商品+SKU，实测几 KB）的约 200 倍。
# 若将来支持图片内联上传，要按 base64 膨胀率（×1.37）重算。
MAX_BODY_BYTES=1048576

# HTTP 头上限（字节）。Go 默认 1 MiB，主动收到 16 KiB。
# HTTP 头里装不下任何合法业务数据，而上限直接乘在连接数上。
MAX_HEADER_BYTES=16384
```
