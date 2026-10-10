# Task 13 交付说明：限流加固 + refresh 轮换原子化 + 商户商品管理与审核

日期：2026-10-03（文件行数与测试基线于 2026-10-10 安全加固轮校正）
基线：`go test ./...` → 103 个测试函数 / 221 个断言点，全绿
变异验证：30 条变异体，见文末

> 相关文档：[`security-hardening.md`](security-hardening.md)（后续一轮安全加固报告，含 2 个 P0）、
> [`../README.md`](../README.md)（项目总览）。
> 本轮限流测试的辅助函数后来改用 `miniredis` —— 原先连不上 Redis 会**静默跳过**，
> 那意味着限流核心逻辑从未被执行过。详见安全加固报告第八节。

---

## 一、结论先行

| 要求 | 状态 | 关键决策 |
|---|---|---|
| 1. Redis 滑动窗口限流 | ✅ | ZSET + Lua 原子脚本；`auto` 降级模式（生产 fail-closed，开发 fail-open） |
| 2. refresh 轮换原子化 | ✅ | Redis Lua 互斥 + **PostgreSQL CAS 为唯一权威**；不一致时补偿回滚 |
| 3. 协程并发签发/轮换 | ✅ | `errgroup.SetLimit(2)`，上界精确；ctx 超时不向上传播 |
| 4. 商户商品管理与审核 | ✅ | 5 状态机 + 审核 CAS；跨租户 404 隔离 |

**过程中发现并修掉一个真实回归**：上一轮变异验证被 SIGTERM 打断，M5 的变异体永久残留
在 `repo/merchant.sql.go` 里（审核 CAS 条件被改成 `AND status IS NOT NULL`）。
这意味着"审核接口的 CAS 保护已经失效"——两个管理员并发裁决会互相覆盖结论，
而日志里两条都写着"成功"。详见第五节。

---

## 二、改动文件清单

### 2.1 新增文件（14 个）

**限流**
| 文件 | 行数 | 职责 |
|---|---|---|
| `services/api/internal/ratelimit/ratelimit.go` | 400 | ZSET 滑动窗口 + Lua 原子判定 + 降级决策 |
| `services/api/internal/httpapi/ratelimit_policy.go` | 140 | 路由 → 限流策略映射表（15 个 action） |
| `services/api/internal/httpapi/ratelimit_middleware.go` | 154 | 限流中间件 + `X-RateLimit-*` 响应头 |
| `services/api/internal/ratelimit/ratelimit_test.go` | 522 | 13 个测试（滑动窗口 / 并发 / 边界 / 降级 / 超时；辅助改用 miniredis） |
| `services/api/internal/httpapi/ratelimit_test.go` | 472 | 12 个测试（策略覆盖 / 中间件行为 / 429-503 区分；辅助改用 miniredis） |

**refresh 权威记录**
| 文件 | 行数 | 职责 |
|---|---|---|
| `services/api/db/migrations/0003_refresh_tokens.sql` | 86 | refresh token 表 + 3 个索引 |
| `services/api/db/queries/refresh_tokens.sql` | 99 | 6 条查询，含权威 CAS |
| `services/api/internal/repo/refresh_tokens.sql.go` | 228 | sqlc 生成 |
| `services/api/internal/service/token.go` | 384 | 签发 / 轮换 / 补偿编排（errgroup 并发） |
| `services/api/internal/service/token_integration_test.go` | 540 | 13 个集成测试 |

**商品模块**
| 文件 | 行数 | 职责 |
|---|---|---|
| `services/api/internal/domain/product.go` | 112 | `ProductStatus` 5 状态机 + 状态迁移表 |
| `services/api/db/queries/merchant_products.sql` | 281 | 商品 CRUD + 审核 CAS + 审核队列 |
| `services/api/internal/repo/merchant_products.sql.go` | 769 | sqlc 生成 |
| `services/api/internal/service/product.go` | 693 | 商品服务层：CRUD / 状态流转 / 审核 |
| `services/api/internal/service/product_integration_test.go` | 895 | 20 个集成测试 |

### 2.2 修改文件（14 个）

| 文件 | 改了什么 |
|---|---|
| `internal/config/config.go` | 9 个限流配置项 + `TokenIOConcurrency/Timeout` + `validateRateLimit()` + `RateLimitFailsClosed()` |
| `internal/auth/refresh.go` | **完全重写**：Lua 原子轮换、`CacheToken`、`Compensate`、`HashToken`、`NewFamilyID` |
| `internal/service/auth.go` | 删掉旧 `Refresh`/`issueSession`（已移入 token.go）；`Logout` 改 DB 优先 |
| `internal/service/service.go` | `Deps.RefreshTTL` + `Logger()` 访问器 |
| `internal/domain/errors.go` | `ErrRateLimited` / `ErrRateLimitUnavailable` |
| `internal/httpapi/response.go` | 429 / 503 错误映射 |
| `internal/httpapi/router.go` | 挂限流中间件（authGate 之后、merchantStatusGate 之前）+ `Deps` 扩展 |
| `cmd/api/main.go` | 注入专用限流 Redis 客户端 + `RefreshTTL` |
| `db/queries/merchant.sql` | 新增 `GetMerchantShopName`（审核队列要显示店名） |
| `db/queries/users_products.sql` | 商品查询补 merchant 维度 |
| `internal/repo/{merchant,users_products}.sql.go` | 重新生成 |
| `docs/openapi/openapi.yaml`、`packages/api-contract/src/schema.d.ts`、`internal/httpapi/gen/api.gen.go` | 契约生成物 |
| `scripts/verify-guards.py` | 新增 M14–M30 变异体 + 残留自检 + 中断安全 + 分批参数 |

---

## 三、并发安全边界

### 3.1 refresh 轮换：Redis 互斥 ≠ 权威

这是本轮最关键的设计点。**Redis 说了不算，数据库说了才算。**

```
步骤 1  查 DB（权威）        库里没这条 → 直接拒（幽灵 token 也被挡在这里）
步骤 2a Redis Lua 原子消费    快速路径：一次 EVAL 完成"查旧 → 删旧 → 记已用 → 写新"
步骤 2b DB CAS 裁决          UPDATE ... WHERE status='ACTIVE' AND expires_at > now()
                            影响行数 0 → 若原本是 ROTATED，判定为重放 → 吊销整个家族
步骤 2c 落新记录            写 refresh_tokens 新行
步骤 2d 补缓存              仅在 2a 未成功时执行
步骤 3  errgroup 并发        取用户信息 + 签 access token
```

| 竞争场景 | 裁决者 | 结果 |
|---|---|---|
| 同一 token 20 个并发刷新 | DB CAS | 恰好 1 个成功，19 个返回 401 |
| Lua 成功但 DB CAS 落空 | 补偿 | `Compensate()` 撤销 Redis 侧消费，还原旧 token |
| Redis 挂 | DB | 降级为纯 DB 路径，只损失性能不损失安全 |
| 库里没有但 Redis 有 | DB | 拒绝（幽灵 token 无效） |
| 新记录写失败 | 补偿 | 吊销新 token + 回滚旧 token，报错返回 |
| 重放检测 | Lua 或 DB 任一 | 吊销整个 `family_id`，所有派生 token 一起失效 |

**为什么必须保留 DB 这一层**：Redis 不可用时它降级、重启时它丢数据、主从切换时它会短暂
读到旧副本。所有这些场景下"Redis 里有记录"都不构成有效凭证。实测案例：
`TestRefresh_DatabaseOverridesRedisGhost` 往 Redis 塞了一条库里不存在的 token，
该 token 必须换不到 access token。

### 3.2 并发度上界与超时

```go
g, gctx := errgroup.WithContext(ctx)
g.SetLimit(2)   // 签发：1 个 access token + 1 条 DB 写入，精确上界 2
```

- **上界是精确的，不是"大概"**：`SetLimit(2)` 保证同时最多 2 个 goroutine。
  refresh 路径是 `SetLimit(2)`（取用户 + 签 access token）。
- **错误聚合**：`errgroup.Wait()` 返回第一个非 nil 错误，被包装成带中文上下文的错误
  （如"写入 refresh 记录失败: %w"），不吞错误。
- **超时取消不向上传播**：`context.WithTimeout(ctx, tokenIOTimeout)` 在函数内派生，
  `defer cancel()`。超时只中断本次 token 操作，**不会**把调用方的 HTTP 请求 ctx 标记为取消。
- **单次 go test 硬上限**：验证脚本给每次 `go test` 加了 900s 超时，
  超时判为"卡住"而不是"证明"。

### 3.3 限流的并发安全

Lua 脚本把三步合成一次 EVAL，Redis 单线程执行天然原子：

```lua
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)  -- 清过期
local count = redis.call('ZCARD', key)                     -- 查当前
if count >= limit then ... return {count, 0, oldestAt} end -- 拒
redis.call('ZADD', key, now, member)                       -- 记本次
redis.call('PEXPIRE', key, window * 2)                     -- 定 TTL
```

**一个实测踩到的坑**：ZSET 的 member 必须唯一。最初用 `now.UnixNano()` 当 member，
在 Windows 上时钟精度只到毫秒级，同一毫秒内多次调用撞车 → 计数丢失 →
7 次请求只记了 4 条，`TestAllow_BlocksAtThreshold` 失败。
修法是 `memberPrefix(随机4字节) + 原子序号 + 毫秒时间戳` 三段拼接。

---

## 四、失败重试的边界

### 4.1 限流降级

| `RATE_LIMIT_FAIL_MODE` | Redis 挂了时 | 适用 |
|---|---|---|
| `open` | 放行 | 本地开发 |
| `closed` | 503 | 金融/支付类接口 |
| `auto`（默认） | 生产 503 / 开发放行 | 依 `APP_ENV` 自动选 |

**为什么默认 auto 而不是 closed**：限流器挂掉就 503 意味着 Redis 抖动 = 全站不可用。
但生产环境放行意味着限流失效且监控上看不出来（和"没人触发限流"完全一样）。
按环境分是唯一诚实的答案。降级发生时**打 Warn 日志**，不留静默痕迹。

### 4.2 refresh 重试语义

**没有自动重试，这是有意的。** 一次刷新失败可能处于三种状态之一，客户端无法区分：

1. Redis 降级了，DB 其实成功 → 重试会拿到"已轮换"→ 误判为重放 → 吊销整个家族
2. Lua 成功但 DB 落空，补偿也失败 → 服务端状态不明
3. 真的是重放

自动重试在场景 1 下会造成**比失败严重得多**的后果（用户会话被误吊销）。
所以策略是：**明确失败 + 客户端决定是否重新登录**。

### 4.3 明确没做的部分

| 没做 | 为什么 |
|---|---|
| IP 维度的全局限流 | 只按 user/merchant/action 分。NAT 后面大量用户共享一个 IP，按 IP 限会误伤。`clientIP()` **不信任 `X-Forwarded-For`**（可伪造） |
| refresh 自动重试 | 见 4.2，重试会误伤合法用户 |
| 限流计数落 DB | Redis TTL 到期即消失，这是准入控制不是配额账本 |
| 商品批量操作 | 当前只有单条 CRUD，批量导入的鉴权/事务边界要单独设计 |

---

## 五、过程中发现并修掉的真实回归

### 5.1 变异验证被 SIGTERM 打断，留下污染源码

**现象**：上一轮 `verify-guards.py` 跑到一半被 SIGTERM（退出码 1，无输出）。
之后基线测试出现 `TestReviewMerchant_SecondReviewIsRejected` 失败。

**根因**：SIGTERM **直接杀进程**，不执行 Python 的 `finally` 块。
于是 `path.write_bytes(m.original)` 那行还原代码根本没跑，
M5 的变异体（把 `AND status = 'PENDING'` 改成 `AND status IS NOT NULL`）
永久留在了 `internal/repo/merchant.sql.go` 里。

**为什么危险**：这不是测试问题，是**审核 CAS 保护已经失效**。
两个管理员并发裁决同一条申请会互相覆盖结论，两条日志都写"成功"。

**修了两处**：
1. 还原 M5 的源码（`AND status = 'PENDING'` 已恢复，与源 SQL `db/queries/merchant.sql:136` 一致）
2. **给脚本加了中断安全**——这是真正的修复：

```python
ACTIVE: list[tuple[Path, bytes]] = []   # 正在被改、尚未还原的文件

def restore_active():
    for path, original in reversed(ACTIVE):
        path.write_bytes(original)

def _install_signal_guards():
    # SIGTERM/SIGINT/SIGHUP/SIGBREAK 全部挂上：先还原再退出
    def handler(signum, _frame):
        restore_active()
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)
```

一个"改坏源码"的脚本必须能在被杀之后自我修复，否则它就是个投毒器。

### 5.2 顺手补的残留自检

`--audit` 模式：跑验证**之前**先证明源码树是干净的。

```bash
python scripts/verify-guards.py --audit
```

首次运行就抓出了另外两处残留（M21 过期条件、M28 编辑白名单）——
它们没让基线变红，所以单靠跑测试是发现不了的。
另外修了自己两个 bug：
- 判定顺序错：先查 `new` 命中会误报（M21 的 `new` 是 `old` 的子串）
- M30 锚点参数编号写错（`$2/$3` 实际应为 `$1/$2`），那条变异体一直是"未证明"

### 5.3 go-redis 的 ctx deadline 不管用

`TestAllow_HasItsOwnTimeout` 断言 200ms 超时，实测耗 **5.0009s**。
go-redis v9 在"连得上但不回话"场景下不理会 ctx deadline，等的是自己的 `ReadTimeout`。
修法：`NewDedicatedRedis()` 专用客户端，`ReadTimeout/WriteTimeout/DialTimeout`
全部等于判定超时。**共用客户端会让限流器成为全站最大的一笔延迟。**

---

## 六、测试场景

### 6.1 新增测试（103 个函数 / 221 个断言点）

限流 25 + token 13 + 商品 19 = 57 个新增测试函数；连同既有用例共 103 个。
（全库现为 **120 个函数** —— 安全加固轮又加了 17 个，见 `security-hardening.md` 第八节。）

**限流 25 个**（`internal/ratelimit/ratelimit_test.go` 13 个 + `internal/httpapi/ratelimit_test.go` 12 个）
- 滑动窗口边界突发（固定窗口会双倍放行）
- 并发恰好命中阈值（20 并发，恰好 20 通过）
- 被拒请求不占额度
- 不同 action / 不同 subject 额度隔离
- 窗口滑动后额度恢复
- 后端不可用时 fail-closed / fail-open **两个方向都测**（写反任一个都被抓）
- 自带超时（专用客户端）+ 仅靠 ctx 无法兜住黑洞连接
- 错误不外泄后端地址
- 策略表覆盖全部契约端点（含 `/api/v1/me`）
- 三个登录入口额度互相独立、refresh 阈值比登录宽
- 429 与 503 的区分、`Retry-After`、`X-RateLimit-*` 头
- OPTIONS 预检与非注册路由直接放行

**token 13 个**（`token_integration_test.go`）
- 20 并发刷新**恰好 1 个成功**
- 轮换链式可用（刷新后新 token 还能继续刷）
- DB 覆盖 Redis 幽灵 token
- 重放 → 吊销整个家族
- 过期 token 拒绝且**不**吊销家族
- 库里存 hash 不存明文
- Redis 数据全丢后仍能工作（纯 DB 路径）
- Redis 完全不可用时降级
- 登出吊销、幂等、未知 token 不牵连他人
- family 集合不增长（防 Redis 内存泄漏）

**商品 19 个**（`product_integration_test.go`）
- 状态机穷举：5 状态 × 5 状态 = 25 组合
- 无 DRAFT → ON_SALE 捷径
- 跨租户隔离（列表 / 详情 / 编辑 / 删除全部 404）
- 审核并发只有一个赢家
- 驳回必填理由、非法结论拒绝
- 强制下架
- 被驳回后编辑自动重新提交
- 在售商品不可编辑、不可删除
- 编辑时整体替换 SKU、删除级联删 SKU
- 列表按状态过滤、审核队列倒序、页码夹取

### 6.2 变异验证（30 条）

**「加了测试」和「测试能抓住回归」是两件事。** 一个断言写错方向、压根没被执行到的守卫，
跑起来同样是绿的。

| 编号 | 主题 | 抓什么 |
|---|---|---|
| M1–M6 | 权限 / 入驻审核 | 漏设审批位、审核闸门永不拒绝、CAS 丢失 |
| M7–M11 | 错误信息泄露 | 4xx 泄露 Go 类型名、`UserFacing` 只认最外层 |
| M12–M13 | 分页溢出 | `page=2147483648` 变负 OFFSET → 接口 500 |
| **M14–M19** | **限流** | 退回固定窗口、被拒请求占额度、key 丢 scope、fail-closed 写反、自带超时失效、后端地址外泄 |
| **M20–M24** | **refresh 原子性** | DB CAS 丢 status/过期条件、存明文、Lua 成功不回滚、只信 Redis |
| **M25–M30** | **商品** | 跨租户越权、审核 CAS 失效、状态机长捷径、在售可编辑、驳回不重提交、可删在售 |

M14 的实测数据：把计数清空后并发用例放行了 **60 次**（阈值 20）。

### 6.3 仍需补充的测试场景

| 优先级 | 场景 | 原因 |
|---|---|---|
| P0 | 真实 Redis 集群下的限流（当前用 miniredis） | Lua 在主从切换时的行为、`PEXPIRE` 丢失窗口未覆盖 |
| P0 | `Compensate()` 自身失败的路径 | 现在只测了补偿成功；补偿也失败时的一致性残留是理论盲区 |
| P1 | 限流 key 的 TTL 回收 | 大量不同 subject 会留下多少 key、内存增长曲线未测 |
| P1 | 商品审核队列的翻页稳定性 | 已加 `p.id DESC` 做 tie-break，但没测并发插入时的游标一致性 |
| P1 | refresh 在 `TokenIOTimeout` 边界的行为 | 超时恰好发生在 Lua 与 CAS 之间时的补偿正确性 |
| P2 | 契约层与实现的漂移守卫 | 现有 `TestRatePolicy_CoversEveryContractEndpoint` 只覆盖限流策略，商品/订单端点没有同类守卫 |

---

## 七、复现命令

```bash
# 全量测试
cd services/api && DATABASE_URL="postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable" \
  go test -count=1 ./...

# 残留自检（跑验证前必做）
python scripts/verify-guards.py --audit

# 变异验证（分批，避免超时）
python scripts/verify-guards.py --from 14 --to 19
python scripts/verify-guards.py --from 20 --to 30 --no-baseline
```

## 八、配置项清单

```bash
# 限流
RATE_LIMIT_ENABLED=true          # 关掉后登录接口无任何频率限制，仅限本地开发
RATE_LIMIT_WINDOW=1m             # 业务接口窗口
RATE_LIMIT_MAX=120               # 业务接口阈值
RATE_LIMIT_AUTH_WINDOW=1m        # 登录/注册/刷新窗口
RATE_LIMIT_AUTH_MAX=10           # 登录阈值（refresh 单独放宽 6 倍）
RATE_LIMIT_TIMEOUT=300ms         # 判定超时（同时是专用客户端的 socket 超时）
RATE_LIMIT_FAIL_MODE=auto        # auto | open | closed

# token 并发
TOKEN_IO_CONCURRENCY=4           # 签发并发度上界
TOKEN_IO_TIMEOUT=5s              # 单次签发/轮换的超时
```
