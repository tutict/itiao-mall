#!/usr/bin/env python
"""变异验证：证明守卫真的有牙。

为什么必须有这个脚本
--------------------
「加了测试」和「测试能抓住回归」是两件事。一个断言写错了方向、正则失配、
或者压根没被执行到的守卫，跑起来同样是绿的 —— 它给人的安全感是假的。
本仓库已经有 `scripts/verify-perf-gate.sh` 对性能门禁做这件事；
权限/审核这类**写错了本地也一切正常**的逻辑更需要。

做法
----
对每个变异体：改坏一处源码 → 跑受影响的测试 → 要求**指定的那条测试**报红 →
还原（二进制读写 + sha256 校验字节完全一致）→ 继续下一个。

三条纪律（都是踩过坑之后加的）
------------------------------
1. 还原必须用二进制读写并比对 sha256。用文本模式 read/write 会做换行转换，
   看着"还原了"，字节其实变了 —— 之后的 `git diff --exit-code` 会莫名其妙地红。
2. 必须断言**是哪条测试**报的错，不能只看退出码非 0。
   编译错误、锚点写在别的文件里、别的测试顺带挂了，都会让退出码非 0。
   那样统计出来的"6/6 抓住"是假的，比没有统计更坏。
3. 锚点找不到时记 NOT_PROVEN，并计入未证明。
   锚点失配意味着变异根本没施加，测试当然是绿的 —— 这种"成功"必须暴露出来。

用法
----
    C:/Users/tutic/.workbuddy/binaries/python/versions/3.13.12/python.exe \
        scripts/verify-guards.py

    # 分批跑（每条变异都要跑一遍全量测试，30+ 条一起跑会超时）
    ... scripts/verify-guards.py --from 14 --to 19 --no-baseline

    # 跑验证前先自检源码树里有没有残留变异体（上一轮被中断留下的）
    ... scripts/verify-guards.py --audit

    # 环境不完整时（PG / Redis 没起）收窄到不依赖它们的包
    ... scripts/verify-guards.py --from 31 --to 36 \
        --packages "./internal/config/... ./internal/httpapi/..."

退出码：0 = 全部变异都被指定的测试抓住；非 0 = 有未证明项。

「未证明」怎么排查
------------------
本脚本只报结论，不报原因。看到「未证明」时按这三类去查（2026-10-10 实测有效）：

  A) **编译失败**。脚本输出里会写「只有包级失败，没有具名测试」，
     或者你手动 `go build ./...` 就能看到。典型成因是变异体本身写坏了 ——
     例如把 `_ = timeout` 塞进 struct 字面量里、或者改掉一个变量后它变成
     "declared and not used"。**这类变异体证明的是「文件坏了」，不是「守卫有牙」，
     必须重写变异体。**

  B) **红了但不是指定测试**。多半是断言方向/范围不对，或者指定的那条测试
     其实在 skip。手动单跑一次就能看清：
         go test -count=1 -run '^TestXxx$' -v ./internal/...
     一条经常被忽略的信号：`go test -v` 里出现 `--- SKIP: TestXxx`。
     而 skip 与通过**在退出码上无法区分** —— 整个包可以全 skip 却退出码 0。

  C) **测试全绿**。两种可能：
       - 守卫是假的（断言方向反了、断言了别的东西）
       - **这个变异体描述的后果根本不成立**。写变异体之前先问
         「它描述的后果真的会发生吗」—— 恒为「未证明」的假条目比没有更坏，
         因为它占着位置，让人以为那块有覆盖。
     实测例子：M36 最初写的是「长度白名单扩大到 GET」，但 GET 的
     `ContentLength` 是 0，两个检查都不触发，扩大之后**行为完全不变**。
     换成「漏掉 PUT/PATCH」（更新商品走 PUT，且没有别的测试覆盖）后立刻被抓。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
API_DIR = ROOT / "services" / "api"

# 单次 go test 的硬上限。集成测试要连库，超过就当"卡住"处理并报出来，
# 不能让它无限期挂着 —— 那会让"变异验证"变成一个看不出进度的黑盒。
GO_TEST_TIMEOUT = int(os.environ.get("VERIFY_GUARDS_TEST_TIMEOUT", "900"))

# 默认跑全部内部包。可用 --packages 收窄 —— 用于「PostgreSQL / Redis 没起，
# 但想先验证不依赖它们的那批变异体」这种场景。
#
# 收窄是**有代价**的：跑一部分包意味着"变异没有影响到其他包"这件事没被验证。
# 所以它只该用在环境不完整的时候，用完之后补一次全量。
PACKAGES = "./internal/..."

# 正在被变异改写、尚未还原的文件。栈顶是当前那一个。
ACTIVE: list[tuple[Path, bytes]] = []


def restore_active() -> None:
    """把当前所有被改过的文件按原字节写回去。幂等。"""
    for path, original in reversed(ACTIVE):
        try:
            path.write_bytes(original)
        except OSError as exc:  # pragma: no cover - 只在磁盘出问题时走到
            print(f"!! 还原 {path} 失败：{exc}", file=sys.stderr)


def _install_signal_guards() -> None:
    """让 SIGTERM / SIGINT / SIGHUP 也能还原，而不是留下污染的源码。"""
    import signal

    def handler(signum, _frame):
        name = signal.Signals(signum).name
        restore_active()
        print(
            f"\n收到 {name}，已还原 {len(ACTIVE)} 个被变异的文件后退出。",
            file=sys.stderr,
        )
        # 恢复默认处置再自杀，保证第二次信号能立刻生效
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for sig in ("SIGTERM", "SIGINT", "SIGHUP", "SIGBREAK"):
        s = getattr(signal, sig, None)
        if s is not None:
            try:
                signal.signal(s, handler)
            except (ValueError, OSError):  # pragma: no cover
                pass


def audit_clean() -> list[str]:
    """检查所有变异体是否都处于「原始状态」。

    这是**跑验证之前**必须做的一步。上一次运行被 SIGTERM 打断时，
    M5 的变异体永久留在了 repo/merchant.sql.go 里（CAS 条件被改成
    `status IS NOT NULL`）。那次之后：
      - 基线不再全绿，失败原因看起来像"新写的测试有问题"；
      - 而真正的病灶（源码被改坏）在任何测试输出里都不会出现。
    所以：先证明树是干净的，再谈守卫有没有牙。
    """
    problems: list[str] = []
    for m in MUTATIONS:
        path = ROOT / m.rel_path
        if not path.exists():
            problems.append(f"{m.ident}：文件不存在 {m.rel_path}")
            continue
        text = path.read_bytes().decode("utf-8")
        n_old = text.count(m.old)
        n_new = text.count(m.new)
        if n_old == 1:
            # 原始文本在位且唯一 —— 一定没被施加过。
            # 判定顺序不能反：很多 `new` 其实是 `old` 的子串
            # （例如 M21 只是从三行条件里删掉一行），先查 new 会误报残留。
            continue
        if n_new > 0:
            problems.append(
                f"{m.ident}：源码里**残留着这个变异体**（新体命中 {n_new} 次）"
                f" —— 上一轮验证被中断后没还原干净"
            )
        else:
            problems.append(
                f"{m.ident}：锚点在 {m.rel_path} 命中 0 次 —— 变异将无法施加，"
                f"本项恒为『未证明』（多半是锚点里的参数编号或缩进和代码对不上了）"
            )
    return problems


DATABASE_URL = os.environ.get(
    "DATABASE_URL", "postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable"
)


@dataclass
class Mutation:
    """一个变异体：改坏一处，期望被特定的测试抓住。"""

    ident: str
    rel_path: str
    old: str
    new: str
    expect_fail: list[str]
    why: str
    note: str = ""

    # 运行期填充
    original: bytes = field(default=b"", repr=False)


MUTATIONS: list[Mutation] = [
    Mutation(
        ident="M1 商户业务接口漏设入驻审核",
        rel_path="services/api/internal/httpapi/access.go",
        old='prefix: "/api/v1/merchant", roles: []string{auth.RoleMerchant}, requireApprovedMerchant: true}',
        new='prefix: "/api/v1/merchant", roles: []string{auth.RoleMerchant}, requireApprovedMerchant: false}',
        expect_fail=[
            "TestRoutePolicy_MerchantApprovalExemptions",
            "TestMerchantStatusGate_BlocksUnapprovedMerchants",
        ],
        why="漏设这一位之后，被驳回/被停用的商户照样能操作全部商户业务接口",
    ),
    Mutation(
        ident="M2 profile 前缀打错一个字",
        rel_path="services/api/internal/httpapi/access.go",
        old='{level: levelRoles, prefix: "/api/v1/merchant/profile", roles: []string{auth.RoleMerchant}},',
        new='{level: levelRoles, prefix: "/api/v1/merchant/profiles", roles: []string{auth.RoleMerchant}},',
        expect_fail=["TestMerchantStatusGate_BlocksUnapprovedMerchants"],
        why="豁免条目失配后，被驳回的商户读不到自己的驳回理由，只能反复盲提交",
    ),
    Mutation(
        ident="M3 审核闸门直接放行",
        rel_path="services/api/internal/httpapi/router.go",
        old="if status != service.MerchantApproved {",
        new="if false {",
        expect_fail=["TestMerchantStatusGate_BlocksUnapprovedMerchants"],
        why="闸门写成了永不拒绝 —— 这一层等于不存在，且没有任何信号",
    ),
    Mutation(
        ident="M4 重新提交不回到待审核",
        rel_path="services/api/internal/repo/merchant.sql.go",
        old="WHEN merchant_profiles.status = 'REJECTED' THEN 'PENDING'",
        new="WHEN false THEN 'PENDING'",
        expect_fail=["TestUpsertMerchantProfile_StatusMachine"],
        why="被驳回的商户改了资料却仍是 REJECTED，永远无法重新进入审核队列",
        note="改的是 sqlc 生成文件里的 SQL 文本 —— 这里要验的是 SQL 的语义，"
        "而不是生成器本身，所以没必要为了变异跑一次 codegen",
    ),
    Mutation(
        ident="M5 审核丢掉 CAS 条件",
        rel_path="services/api/internal/repo/merchant.sql.go",
        old="  AND status = 'PENDING'\nRETURNING user_id",
        new="  AND status IS NOT NULL\nRETURNING user_id",
        expect_fail=["TestReviewMerchant_SecondReviewIsRejected"],
        why="两个管理员并发审核时后者会覆盖前者，且两条都记「成功」",
    ),
    Mutation(
        ident="M6 驳回可不给理由",
        rel_path="services/api/internal/service/merchant.go",
        old='if decision == MerchantRejected && rejectReason == "" {',
        new="if false {",
        expect_fail=["TestReviewMerchant_Validation"],
        why="没有理由的驳回让商户只能靠猜，反复盲提交反而推高审核成本",
    ),

    # ---- 错误响应信息泄露（2026-10-02 四轴 review 的 P0）----
    #
    # 修复前实测到的真实响应：
    #   {"status":409,"detail":"资源冲突: can't decode JSON body: json: cannot
    #    unmarshal number into Go struct field PasswordLoginRequest.phone of type string"}
    # 这把 Go 类型名与字段名白送给任何能发请求的人。
    Mutation(
        ident="M7 4xx 恢复回内部错误原文",
        rel_path="services/api/internal/httpapi/response.go",
        old="\tdetail := domain.ExposeUserFacing(err)",
        new="\tdetail := err.Error()",
        expect_fail=["TestWriteProblem_NoInternalLeak"],
        why="回到修复前的样子：解析库原文连同 Go 类型名与字段名一起外泄",
    ),
    Mutation(
        ident="M8 把 4xx 的 detail 一律抹光（防『全抹』式假修复）",
        rel_path="services/api/internal/httpapi/response.go",
        old="\tdetail := domain.ExposeUserFacing(err)",
        new='\tdetail := ""',
        expect_fail=["TestWriteProblem_UserFacingStillVisible"],
        why="抹光也能让泄露测试变绿，但前端会失去所有字段级提示 —— 这是回归不是修复",
        note="没有这一条，M7 那种修法会被『全部抹掉 detail』轻松顶替，"
        "而守卫看起来是绿的",
    ),
    Mutation(
        ident="M9 UserFacing 只认最外层错误",
        rel_path="services/api/internal/domain/errors.go",
        old="\tvar uf *UserFacing\n\tif errors.As(err, &uf) {",
        new="\tuf, ok := err.(*UserFacing)\n\tif ok && errors.As(uf, &uf) {",
        expect_fail=["TestWriteProblem_UserFacingSurvivesWrapping"],
        why="业务层习惯再包一层，一包就静默丢掉全部提示，且没有任何报错",
    ),
    Mutation(
        ident="M10 解析失败把库错误伪装成业务文案外泄",
        rel_path="services/api/internal/httpapi/router.go",
        old="\t\t\tWriteProblem(w, r, domain.ErrValidation)",
        new='\t\t\tWriteProblem(w, r, domain.UserFacingf(err.Error(), domain.ErrValidation))',
        expect_fail=["TestRequestErrorHandler_NoDecodeLeak"],
        why="这是最可能的真实复发形态：不明目张胆拼 %v，而是包成 UserFacing 骗过白名单",
        note="守卫必须能识破『伪装』，否则 UserFacing 机制会变成新的泄露通道",
    ),
    Mutation(
        ident="M11 入驻状态不再外泄",
        rel_path="services/api/internal/httpapi/router.go",
        old='\t\t\t\tWriteProblem(w, r, domain.UserFacingf(\n\t\t\t\t\tfmt.Sprintf("当前入驻状态为 %s", status),\n\t\t\t\t\tdomain.ErrMerchantNotApproved))',
        new="\t\t\t\t_ = fmt.Sprintf\n\t\t\t\tWriteProblem(w, r, domain.ErrMerchantNotApproved)",
        expect_fail=["TestMerchantStatusGate_ReportsCurrentStatus"],
        why="前端要靠这个状态值决定跳「审核中」还是「已驳回」，丢了就只能一直停在错误页",
        note="变异体带 `_ = fmt.Sprintf` 兜住导入 —— 变异后编译不过的话，"
        "证明的是「文件坏了」而不是「守卫有牙」",
    ),

    # ---- 分页溢出 ----
    Mutation(
        ident="M12 derefInt32 去掉 int32 收窄判断",
        rel_path="services/api/internal/httpapi/convert.go",
        old="\tif n := int64(*p); n >= math.MinInt32 && n <= math.MaxInt32 {\n\t\treturn int32(n)\n\t}\n\treturn def",
        new="\t_ = math.MaxInt32\n\treturn int32(*p)",
        expect_fail=["TestDerefInt32_RejectsOverflow"],
        why="page=2147483648 截断成负数后被夹回 1，客户端请求第 2147483648 页拿到第 1 页，"
        "而且没有任何报错",
    ),
    Mutation(
        ident="M13 normalizePage 去掉溢出夹取",
        rel_path="services/api/internal/service/service.go",
        old="\tmaxPage := math.MaxInt32/pageSize + 1\n\tif page > maxPage {\n\t\tpage = maxPage\n\t}",
        new="\tmaxPage := math.MaxInt32/pageSize + 1\n\tif false && page > maxPage {\n\t\tpage = maxPage\n\t}",
        expect_fail=["TestNormalizePage_ClampsToValidRange", "TestNormalizePage_OffsetNeverExceedsInt32"],
        why="(2147483647-1)*100 溢出成 -200，负 OFFSET 送进 PostgreSQL 会让整条查询报错 —— "
        "表现为「翻到很后面时接口开始 500」",
    ),

    # ---- 限流（2026-10-03）----
    #
    # 限流的失败形态特别隐蔽：改坏了**不会报错**，接口照常工作，
    # 只是完全不受保护。所以每条守卫都必须能抓住对应的"静默失效"。
    Mutation(
        ident="M14 滑动窗口退回固定窗口（只按当前桶计数）",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old="redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)",
        new="redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)\nredis.call('DEL', key)",
        expect_fail=[
            "TestAllow_RejectsBoundaryBurst",
            "TestAllow_ConcurrentExactlyHitsLimit",
        ],
        why="删掉记录等于每次都从空集开始 —— 限流器变成永放行，且不产生任何错误。"
        "实测：把计数清空后并发用例放行了 60 次（阈值 20）",
        note="这是固定窗口最极端的一种实现（记在固定桶里但每轮清空）。"
        "真实实现会保留当前桶的数据，但后果完全一样：限额被架空",
    ),
    Mutation(
        ident="M15 被拒的请求也占额度",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old="if count >= limit then",
        new="if count >= 999999 then",
        expect_fail=["TestAllow_RejectedRequestsDoNotConsumeQuota"],
        why="攻击者用远超阈值的请求量把窗口塞满，正常用户被一起挤掉 —— "
        "而他自己的重试在帮倒忙",
    ),
    Mutation(
        ident="M16 限流键丢掉 action 段（不同接口共用一个桶）",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old='return l.prefix + ":" + string(p.Scope) + ":" + string(p.Action) + ":" + subject',
        new='return l.prefix + ":" + string(p.Scope) + ":" + subject',
        expect_fail=[
            "TestAllow_IsolatesActions",
            "TestRatePolicy_LoginEntriesAreIndependent",
        ],
        why="三个登录入口的 scope 都是 auth。丢掉 action 段之后它们共用一个额度 —— "
        "撞商户登录就能把管理员登录一起锁死，而那是平台运营停摆的起点",
        note="最初这条变异改的是 **scope** 段，想证明「scope 该不该进 key」。"
        "但那是个伪命题：action 已唯一决定 scope，scope 段是冗余的，"
        "去掉它不改变任何行为。真正不能丢的是 action 段。\n"
        "顺带一个真实教训：这条变异曾因上次运行被 SIGTERM 打断而**残留**在源码里，"
        "导致我一度以为「代码漏了 scope 段」而去改实现。方向搞反时，"
        "先问『这条变异在证明什么我同意吗』，再动代码。",
    ),
    Mutation(
        ident="M17 fail-closed 判断写反（后端故障时反而放行）",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old="if errors.Is(err, ErrBackendUnavailable) && l.failsClosed {",
        new="if errors.Is(err, ErrBackendUnavailable) && !l.failsClosed {",
        expect_fail=["TestDecide_BackendDown", "TestRateLimitGate_BackendDownFailsClosedAs503"],
        why="生产环境里 Redis 一抖动就静默变成放行 —— 限流失效与「没人触发限流」"
        "在监控上完全一样",
        note="两个断言方向相反（一个要求放行一个要求拒绝），所以任何一种写反都会被抓住",
    ),
    Mutation(
        ident="M18 限流专用客户端的 ReadTimeout 不再跟随判定超时",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        # 只改 ReadTimeout 这一行。**不要**顺手把 DialTimeout / WriteTimeout 一起改，
        # 也不要往 struct 字面量里塞 `_ = timeout` —— 那会变成语法错误，
        # 于是"测试红了"但红的是编译失败，证明的是「文件坏了」而不是「守卫有牙」。
        # 这个坑实测踩过一次（第一次跑出来就是「只有包级失败，没有具名测试」）。
        old="\t\tReadTimeout:  timeout,",
        new="\t\tReadTimeout:  5 * time.Second,",
        expect_fail=["TestAllow_HasItsOwnTimeout"],
        why="go-redis 的 ctx deadline 拦不住阻塞中的 socket 读 —— 实测 200ms 的 ctx "
        "实际耗 5.0009s，等的是客户端自己的 ReadTimeout。把它写死成 5s 之后，"
        "Redis 主从切换期间的每个请求都会被限流器拖满 5 秒："
        "准入控制反而成了全站最大的一笔延迟",
        note="锚点必须在测试**真正读到**的代码上。最初这条变异改的是 "
        "cmd/api/main.go（把专用客户端换成共用的 rdb）—— 而 main.go 不被任何 "
        "Go 测试覆盖，那样的变异体恒为『未证明』，是典型的假验证。",
    ),
    Mutation(
        ident="M19 丢掉 ErrBackendUnavailable 哨兵（fail-closed 静默退化为 fail-open）",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        # 锚点在 2026-10-10 的 P2-6 加固后从 `fmt.Errorf("%w: %v", ...)` 换成
        # `&BackendError{Cause: err}`（原文不再拼进 Error()）。变异语义不变：
        # 都是"让 errors.Is(err, ErrBackendUnavailable) 不成立"。
        #
        # 2026-10-11 第五轮：接入熔断时把判定里的局部变量从 err 改名成 runErr
        # （避免遮蔽具名返回值 —— 见 ratelimit.Allow 的注释），锚点随之更新。
        old="return Result{}, &BackendError{Cause: runErr}",
        new="return Result{}, fmt.Errorf(\"限流后端异常: %v\", runErr)",
        # 注意期望的测试换了：原来写的是 TestAllow_DoesNotLeakBackendAddress，
        # 但那条守卫**抓不住**这个变异 —— 新错误里依然用 %v 带了原始错误，
        # 泄露程度与改之前一样（都取决于 dial 失败的形态）。
        # 真正有牙的是哨兵判定：拆掉 %w 之后 Decide 里的 errors.Is 恒为 false，
        # 于是 fail-closed 分支不成立，代码落到下面那条"放行"分支 ——
        # **配置为收紧的限流器在 Redis 故障时静默变成放行**。
        expect_fail=["TestDecide_BackendDown", "TestRateLimitGate_BackendDownFailsClosedAs503"],
        why="这是最坏的一种失效形态：Redis 一挂，准入控制不是报错而是**消失**。"
        "攻击者只需打掉 Redis（或等它自己抖一下），撞库与刷单在这段时间内不受任何限制，"
        "而监控上看到的只是若干条 503/降级日志 —— 与「限流正常工作」难以区分",
        note="原始版本声称的危害是「后端地址随错误外泄」。实测不成立："
        "Decide 的错误只在 fail-closed 分支返回，而中间件**只记日志、不回响应体**，"
        "所以内网地址到不了客户端。"
        "（那时还留着一处注释与实现不符：注释说「不把 err 原样上抛」，实现却用了 "
        "`%w: %v` —— 已在 2026-10-10 的 P2-6 用 BackendError 修掉，由 M41 守着。）",
    ),

    # ---- refresh 轮换原子性（2026-10-03）----
    Mutation(
        ident="M20 DB 的轮换 CAS 去掉 status 条件（退化为先查后写）",
        rel_path="services/api/internal/repo/refresh_tokens.sql.go",
        old="WHERE token_hash = $1\n  AND status = 'ACTIVE'\n  AND expires_at > now()",
        new="WHERE token_hash = $1\n  AND expires_at > now()",
        expect_fail=[
            "TestConsumeRefreshToken_SQLGuards",
            "TestRefresh_DBGuard_ConcurrentOnlyOneSucceedsWithoutRedis",
        ],
        why="这是整套 refresh 设计的权威裁决点。去掉 status 条件后，"
        "两个并发刷新都能把同一条标成 ROTATED，签出两批有效新 token —— "
        "用户在 App 里遇到「刚刷新成功，下一次刷新又说令牌无效」",
        note="原先期望的 TestRefresh_ConcurrentOnlyOneSucceeds 抓不住它 —— 那条走的是"
        "Redis 的原子消费，并发在 Lua 那层就被挡下了，CAS 的 status 条件从没被执行到。"
        "补了两条：一条直接调 repo 验 SQL 语义（过期要拒/未过期要放/已消费要拒），"
        "一条把 Redis 换成不可达地址、只剩 DB 兜底时跑 20 并发。"
        "改的是 sqlc 生成文件里的 SQL 文本：这里要验的是 SQL 语义，"
        "不是生成器本身，所以不必为变异跑一次 codegen",
    ),
    Mutation(
        ident="M21 DB 轮换 CAS 去掉过期条件",
        rel_path="services/api/internal/repo/refresh_tokens.sql.go",
        old="WHERE token_hash = $1\n  AND status = 'ACTIVE'\n  AND expires_at > now()",
        new="WHERE token_hash = $1\n  AND status = 'ACTIVE'",
        expect_fail=[
            "TestConsumeRefreshToken_SQLGuards",
            "TestRefresh_CompensatesWhenDBCasFails",
        ],
        why="过期令牌仍能换到新的 access token，等于令牌有效期形同虚设",
        note="同样换过期望的测试。TestRefresh_ExpiredTokenRejectedWithoutFamilyRevoke "
        "抓不住它：service 的过期预检排在 CAS 之前，过期行在那里就被拒了，"
        "SQL 里的 expires_at 条件永远不会被执行。所以补了一条直接调 repo 的测试，"
        "以及一条用回拨时钟骗过预检、把请求逼到 CAS 上的测试。",
    ),
    Mutation(
        ident="M22 库里改存 refresh token 明文",
        rel_path="services/api/internal/service/token.go",
        # 锚点选在 `hash` 的**声明**处，而不是它的使用处（`TokenHash: hash,`）。
        #
        # 为什么要这样选：只把使用处改成 `TokenHash: refreshToken,` 的话，
        # `hash := auth.HashToken(refreshToken)` 就再没人用了 ——
        # Go 对未使用的局部变量是**编译错误**，于是整个包编不过。
        # 那会让判定退化成"包级失败"，证明的是「文件坏了」而不是「守卫有牙」。
        # 把声明直接改成 `hash := refreshToken`，编译照常通过、库里落的也是明文，
        # 这才是这个变异体想表达的那个缺陷。
        old="\tfamilyID := auth.NewFamilyID()\n\thash := auth.HashToken(refreshToken)",
        new="\tfamilyID := auth.NewFamilyID()\n\thash := refreshToken",
        expect_fail=["TestRefresh_DatabaseStoresHashNotPlaintext"],
        why="refresh token 是持有即等同凭证的东西。库被读走（备份泄露、注入读表、"
        "日志里有人打了 select *）时，存明文等于所有在线用户的会话同时失窃",
        note="`hash` 这个声明在 token.go 里出现两次（Refresh 与 issueSession），"
        "所以要带上上一行 familyID 做区分，否则锚点命中 2 次、变异无法施加。",
    ),
    Mutation(
        ident="M23 Redis 消费成功但 DB CAS 失败时不回滚",
        rel_path="services/api/internal/service/token.go",
        old="if redisConsumed {\n\t\t\tif rerr := s.RefreshStore.Compensate(",
        new="if false {\n\t\t\tif rerr := s.RefreshStore.Compensate(",
        expect_fail=["TestRefresh_CompensatesWhenDBCasFails"],
        why="不回滚会留下一条「库里查无此会话、却能换到 access token」的凭证 —— "
        "它能换到 token（签发不查库），于是出现最难查的一类不一致",
        note="原先期望的 TestRefresh_ConcurrentOnlyOneSucceeds 抓不住它：并发下失败的那些"
        "在 Redis Lua 层就被判成重放，根本走不到「Redis 成功 + DB 失败」这个组合。"
        "补的测试用回拨时钟骗过过期预检，把请求逼到 CAS 失败的那一刻 —— "
        "那是 Compensate 唯一被需要的场景。",
    ),
    Mutation(
        ident="M24 刷新时不先查权威侧（只信 Redis）",
        rel_path="services/api/internal/service/token.go",
        old='\tif err != nil {\n\t\tif errors.Is(err, pgx.ErrNoRows) {\n\t\t\t// 库里都没有',
        new='\tif err != nil {\n\t\tif true {\n\t\t\t// 库里都没有',
        expect_fail=["TestRefresh_PropagatesNonRowError"],
        why="Redis 说了不算，库说了才算。删掉 ErrNoRows 判断后，"
        "数据库查询故障会被当成「令牌不存在」—— 真实故障被伪装成用户凭证问题，"
        "监控上表现成「一堆用户拿着无效令牌」，运维会去查客户端而不是查数据库",
        note="原先期望的两条（DatabaseOverridesRedisGhost / RejectsUnknownToken）抓不住："
        "它们只覆盖 ErrNoRows 这一种错误，两种实现下都返回「无效」，所以全绿。"
        "补的测试用一个总是失败的假 DBTX 注入非 ErrNoRows 错误。",
    ),

    # ---- 商品管理（task 13）----
    Mutation(
        ident="M25 商户侧商品查询漏掉 merchant_id 条件（跨租户越权）",
        rel_path="services/api/internal/repo/merchant_products.sql.go",
        old="LEFT JOIN skus s ON s.product_id = p.id\nWHERE p.merchant_id = $1::uuid\n  AND ($2::text IS NULL OR p.status = $2::text)",
        new="LEFT JOIN skus s ON s.product_id = p.id\nWHERE ($2::text IS NULL OR p.status = $2::text)",
        expect_fail=["TestMerchantProduct_CrossTenantIsolation"],
        why="漏掉这个条件，商户的商品列表就变成全平台商品列表 —— "
        "而 200 响应对越权没有任何异常特征，客户端完全看不出来",
    ),
    Mutation(
        ident="M26 商品审核丢掉 CAS 条件",
        rel_path="services/api/internal/repo/merchant_products.sql.go",
        old="AND (\n        -- 通过 / 驳回：只有待审的能被裁决\n        ($1::text IN ('ON_SALE', 'REJECTED') AND status = 'PENDING_REVIEW')",
        new="AND ( true OR\n        ($1::text IN ('ON_SALE', 'REJECTED'))",
        expect_fail=["TestReviewProduct_ConcurrentOnlyOneWins"],
        why="两个管理员并发裁决时后者覆盖前者，最终状态取决于谁的网络快，"
        "而日志里两条都写着「成功」",
    ),
    Mutation(
        ident="M27 状态机长出 DRAFT → ON_SALE 的捷径",
        rel_path="services/api/internal/domain/product.go",
        old="ProductDraft:         {ProductPendingReview, ProductOffSale},",
        new="ProductDraft:         {ProductPendingReview, ProductOffSale, ProductOnSale},",
        expect_fail=[
            "TestProductStateMachine_Exhaustive",
            "TestProductStateMachine_NoDraftToOnSale",
        ],
        why="商户可以自己把商品变成在售 —— 这条捷径让整个商品审核形同虚设",
    ),
    Mutation(
        ident="M28 在售商品也允许编辑（改完仍在售）",
        rel_path="services/api/internal/domain/product.go",
        old="\treturn s == ProductDraft || s == ProductRejected",
        new="\treturn true",
        expect_fail=["TestUpdateProduct_RejectsNonEditableStates"],
        why="在售商品改标题会造成「下单时看到 A、付款后看到 B」；"
        "改价格则完全绕过了平台审核",
    ),
    Mutation(
        ident="M29 被驳回后编辑不重新提交（驳回可被绕过）",
        rel_path="services/api/internal/repo/merchant_products.sql.go",
        old="WHEN status = 'REJECTED'      THEN 'PENDING_REVIEW'",
        new="WHEN status = 'REJECTED'      THEN 'REJECTED'",
        expect_fail=["TestUpdateProduct_RejectedResubmits"],
        why="商户改完仍是 REJECTED —— 驳回变得没有意义，"
        "而他要卖货只能反复走「提交→被驳回」的循环",
    ),
    Mutation(
        ident="M30 删除条件放开到在售商品",
        rel_path="services/api/internal/repo/merchant_products.sql.go",
        old="WHERE id = $1::uuid\n  AND merchant_id = $2::uuid\n  AND status IN ('DRAFT', 'REJECTED')",
        new="WHERE id = $1::uuid\n  AND merchant_id = $2::uuid\n  AND status <> 'PENDING_REVIEW'",
        expect_fail=["TestDeleteProduct_RejectsOnSale"],
        why="在售商品有历史订单引用其 SKU。删掉会让那些订单的明细失去归属",
    ),

    # ---- 安全加固（2026-10-10）----
    #
    # 这一批的共同特征：**改坏了服务照样跑，接口照样正常**。
    # 生产环境跑 mock 支付不会让任何一条业务测试变红；
    # 少发一个安全响应头，浏览器之外没人会察觉；
    # 去掉请求体上限，功能测试全绿而内存可以被无线撑大。
    # 所以每条都必须有指定测试兜住，否则就是"看起来加固了"。
    Mutation(
        ident="M31 生产环境不再拦住 mock provider（后门重新敞开）",
        rel_path="services/api/internal/config/config.go",
        old="\tif err := c.validateMockProviders(); err != nil {\n\t\treturn err\n\t}\n",
        new="\t_ = c.validateMockProviders()\n",
        expect_fail=["TestValidate_MockProvidersRejectedInProd"],
        why="校验还在、也还在被调用，只是错误被丢弃 —— 这是最隐蔽的一种「加固失效」。"
        "生产环境跑 mock 支付时，/api/v1/pay/mock/notify 是公开路由且不验签，"
        "任何人 POST 一个订单号就能白拿商品",
        note="变异体保留调用（`_ = ...`）而不是删掉，是为了让编译照常通过 —— "
        "删掉的话会变成 unused function，证明的是「文件坏了」",
    ),
    Mutation(
        ident="M32 逃生门判断写反（显式关闭反而放行）",
        rel_path="services/api/internal/config/config.go",
        old="\tif c.AllowMockProvidersInProd {\n\t\treturn nil\n\t}",
        new="\tif !c.AllowMockProvidersInProd {\n\t\treturn nil\n\t}",
        expect_fail=["TestValidate_MockProvidersRejectedInProd"],
        why="反转之后语义变成「没设逃生门就放行、设了才检查」—— 正好把默认行为"
        "从 fail-closed 翻成 fail-open。两个方向都有断言，所以怎么翻都会被抓",
    ),
    Mutation(
        ident="M33 安全响应头不再强制覆盖",
        rel_path="services/api/internal/httpapi/hardening.go",
        old="\t\tnext.ServeHTTP(&enforcingWriter{ResponseWriter: w}, r)",
        new="\t\tnext.ServeHTTP(w, r)",
        expect_fail=[
            "TestSecurityHeaders_OverwriteInsteadOfAppend",
            "TestSecurityHeaders_NoExplicitWriteHeader",
        ],
        why="改回「只在 next 之前 Set」之后，下游任何一次同名写入都能覆盖掉安全头，"
        "而响应看上去完全正常 —— 加固变成声明，声明无法被验证就等于没有",
        note="这条变异体有个隐蔽之处：头仍然会出现在**绝大多数**响应里，"
        "只有「下游写同名头」和「handler 直接 Write 不调 WriteHeader」两条路径会漏。"
        "专项断言必须覆盖这两条，否则守卫会在绿着的情况下放过它",
    ),
    Mutation(
        ident="M34 请求体上限不再做 Content-Length 预检",
        rel_path="services/api/internal/httpapi/hardening.go",
        old="\t\t\tif expectsBody(r.Method) {",
        new="\t\t\tif false && expectsBody(r.Method) {",
        expect_fail=[
            "TestBodyLimit_RejectsOversizedBeforeReading",
            "TestBodyLimit_RejectsUnknownLength",
        ],
        why="只剩 MaxBytesReader 兜底之后，超限请求要**先被读进内存**才发现超限。"
        "安全目标（不 OOM）还在，但带宽与 CPU 被白烧，而且状态码从 413 退化成 400",
    ),
    Mutation(
        ident="M35 请求体上限的边界写成 >=（正好等于上限被拒）",
        rel_path="services/api/internal/httpapi/hardening.go",
        old="\t\t\t\tif r.ContentLength > maxBytes {",
        new="\t\t\t\tif r.ContentLength >= maxBytes {",
        expect_fail=["TestBodyLimit_ExactLimitPasses"],
        why="差一错误：恰好 1 MiB 的合法请求被挡掉。只有「超限」和「正常」"
        "两条断言时抓不到它 —— 必须有「恰好等于上限」这一条",
    ),
    Mutation(
        ident="M36 长度校验漏掉 PUT/PATCH（只检查 POST）",
        rel_path="services/api/internal/httpapi/hardening.go",
        old="\tcase http.MethodPost, http.MethodPut, http.MethodPatch:\n\t\treturn true",
        new="\tcase http.MethodPost:\n\t\treturn true",
        expect_fail=[
            "TestBodyLimit_CoversEveryBodyMethod",
            "TestBodyLimit_UnknownLengthCoveredForEveryBodyMethod",
        ],
        why="更新商品走的是 PUT。漏掉它意味着「超限 body 在更新接口上不受预检保护」——"
        "而这条路径上没有任何其他测试会覆盖到，功能测试也全绿",
        note="这条变异体换过一次方向，记下来当教训。最初写的是「白名单扩大到 GET」，"
        "假想的危害是误伤正常浏览请求 —— 但实测发现扩大之后行为**完全不变**"
        "（GET 的 ContentLength 是 0，两个检查都不触发），所以那个变异体恒为未证明。"
        "写变异体之前先问「它描述的后果真的会发生吗」，否则会得到一个占着位置的假条目。",
    ),
    Mutation(
        ident="M37 封面地址改用黑名单（只禁 javascript:）",
        rel_path="services/api/internal/service/product.go",
        old='\tswitch strings.ToLower(u.Scheme) {\n\tcase "http", "https":\n\tdefault:',
        new='\tswitch strings.ToLower(u.Scheme) {\n\tcase "javascript", "vbscript":',
        expect_fail=["TestValidateCoverURL"],
        why="黑名单永远追不上变体：data: 内联文档、file: 本地文件、"
        "以及协议相对 URL（//evil.com/x）都会从 default 掉出去而被放行。"
        "白名单只有两个值，穷举得完也就关得死",
        note="变异后的 switch 只剩一个 case，其余值直接穿过 —— 这是黑名单写法的"
        "典型形态（「只挡我知道的那几个」）。它能编译、能通过一般用例，"
        "只在 data:/file:/协议相对这几条上失守",
    ),
    Mutation(
        ident="M38 封面地址不校验主机名",
        rel_path="services/api/internal/service/product.go",
        old='\tif u.Host == "" {',
        new='\tif false && u.Host == "" {',
        expect_fail=["TestValidateCoverURL"],
        why='"http:///x.png" 能通过 scheme 白名单但没有主机名，落到前端就是一个'
        "永远加载失败的图。商户在真机上看到白框，然后回来问「为什么图片不显示」——"
        "这类问题的排查成本远高于在这里挡一下",
    ),

    # ---- 安全加固第二轮：支付语义（2026-10-10）----
    Mutation(
        ident="M39 支付回调不再核对金额",
        rel_path="services/api/internal/service/commerce.go",
        old="if ev.AmountCents <= 0 || ev.AmountCents != order.TotalCents {",
        new="if false {",
        expect_fail=[
            "TestHandleNotify_RejectsAmountMismatch",
            "TestHandleNotify_AmountEqualSucceeds",
        ],
        why="回调金额是对端「说的」，不是事实。不核对等于把定价权交给能构造请求的人 —— "
        "实测可以用 amount_cents=1 把 total_cents=2274 的订单标记为已支付（约 0.04 折买走整单）",
        note="两条测试都会被抓住，而且方向互补：RejectsAmountMismatch 证明「不符要拒」，"
        "AmountEqualSucceeds 证明「相符要放行」。只有前者的守卫可以被「一律拒绝」这种"
        "写坏的实现骗过 —— 而那会把正常支付全部挡在门外。",
    ),
    Mutation(
        ident="M40 /readyz 回依赖错误原文（内网地址外泄）",
        rel_path="services/api/internal/httpapi/router.go",
        old='\t\t\tchecks["pg"] = "unavailable"',
        new='\t\t\tchecks["pg"] = err.Error()',
        expect_fail=["TestReadyz_DoesNotLeakDependencyError"],
        why="/readyz 是公开路由，任何能发请求的人都读得到。err.Error() 里带着内网主机名"
        "与端口，pgx 还会带 host=/user=/database= —— 实测过 dial tcp [::1]:6379 "
        "直接出现在响应体里。这等于送一份内网拓扑图",
        note="只改 pg 一侧就够：redis 一侧写法完全对称，而一条变异只能有一个锚点。",
    ),
    Mutation(
        ident="M42 X-Request-ID 原样采信（日志注入 / 关联伪造面）",
        rel_path="services/api/internal/httpapi/middleware.go",
        old="if !requestIDPattern.MatchString(id) {",
        new='if id == "" {',
        expect_fail=["TestRequestID_RejectsUntrustedValues"],
        why="这个值会写进响应头与全部日志。采信任意值等于把日志内容与关联 id 的控制权"
        "交给调用方：超长串撑爆日志行，特殊字符可伪造跨系统关联",
        note="这是修复前的原始实现。正向断言（合规值必须保留）在两种实现下都成立，"
        "所以只有 RejectsUntrustedValues 对它变红 —— 那正是它存在的意义。",
    ),
    Mutation(
        ident="M41 后端错误原文重新拼进 Error()（地址随任何误用点外泄）",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old="return Result{}, &BackendError{Cause: runErr}",
        new="return Result{}, fmt.Errorf(\"%w: %v\", ErrBackendUnavailable, runErr)",
        expect_fail=["TestAllow_DoesNotLeakBackendAddress"],
        why="这正是加固前的形态：注释承诺「不把 err 原样上抛」，实现却把 Redis 原文"
        "（dial tcp 10.0.3.11:6379 ...）拼进了 Error()。当前它到不了客户端，"
        "但下一个把 Allow 的错误直接写进响应体的人会把它带出去 —— 而注释会让他以为安全",
        note="与 M19 共用同一个锚点（`&BackendError{Cause: err}`），但变异方向相反："
        "M19 让哨兵消失（fail-open），M41 让原文出现（泄露）。两条都要有 —— "
        "只防一种形态的守卫，在另一种写错方式面前会全绿。",
    ),

    # ---- 封面地址主机名判定（P3-9，2026-10-10 第三批）----
    #
    # 这一组三条都在同一个函数上，但**故意不合并成一条**：它们各自关闭的
    # 是一个独立分支，而任何一条分支单独失效，另外两条仍然正常工作 ——
    # 合并成一条大变异体的话，"哪一部分还有牙"就没人知道了。
    Mutation(
        ident="M43 封面地址不再判定字面 IP（内网/保留网段放行）",
        rel_path="services/api/internal/service/product.go",
        old="\t\tif ipIsNonPublic(ip) {",
        new="\t\tif false {",
        expect_fail=[
            "TestValidateCoverURL_RejectsNonPublicHosts",
            "TestValidateCoverURL_AllowsPublicHosts",
        ],
        why="关掉字面 IP 判定之后，`http://169.254.169.254/latest/meta-data/` 这类"
        "云元数据地址可以原样存成商品封面。今天服务端不抓取，所以它还不是活的 SSRF；"
        "但它是一条**已经通过校验、躺在库里**的载荷 —— 将来谁加了缩略图抓取就自动生效，"
        "而那时没人会想到校验这一层曾经放它进来。",
        note="注意 expect_fail 里两条测试方向相反：RejectsNonPublicHosts 证明「内网要拒」，"
        "AllowsPublicHosts 证明「公网要放」。只留前者的守卫会被「一律拒绝」的坏实现骗过，"
        "而那会把 `https://s3.amazonaws.com/...` 这种真实图床一起挡掉。"
        "另外这条锚点在 *RejectsNonPublicHosts 的「带端口」那几条上是冗余的 —— "
        "`u.Hostname()` 那层的断言不经过这里也能拦下带端口的写法，"
        "所以真正让它变红的是不带端口的 `10.0.0.5`、`169.254.169.254` 那几条。",
    ),
    Mutation(
        ident="M44 主机名不归一化（大小写与末尾根点可绕过）",
        rel_path="services/api/internal/service/product.go",
        old='\th := strings.ToLower(strings.TrimSuffix(host, "."))',
        new="\th := host",
        expect_fail=["TestValidateCoverURL_RejectsNonPublicHosts"],
        why="主机名大小写不敏感、末尾根点等价，这两条都是 DNS 的既定语义 —— "
        "不归一化的话 `http://LOCALHOST/a.png` 与 `http://localhost./a.png` "
        "会直接穿过检查。这是这一类检查最经典的写坏方式："
        "检查确实在跑、对 `localhost` 也确实拦得住，只有换个写法才漏。",
        note="只改归一化、不动后缀表：这样它证明的是「归一化这一步有牙」，"
        "而不是「整个后缀判定有牙」（后者是 M45）。"
        "对应测试里的 `LOCALHOST 大写` 与 `localhost 末尾根点` 两条子用例。",
    ),
    Mutation(
        ident="M45 本机/私网域名后缀表被清空",
        rel_path="services/api/internal/service/product.go",
        old='var nonPublicHostSuffixes = []string{".localhost", ".local", ".internal"}',
        new="var nonPublicHostSuffixes = []string{}",
        expect_fail=["TestValidateCoverURL_RejectsNonPublicHosts"],
        why="表清空后 `images.localhost`、`printer.local`、`storage.internal` 会被放行。"
        "`.local` 是 mDNS 段、`.internal` 是 ICANN 保留的私有 TLD，两者都**保证不解析到公网**，"
        "所以这些地址在真实用户设备上永远加载不出来 —— 商户填了也只会得到一个白框。",
        note="这条不会让 `localhost` 本身失守（那个由 `h == \"localhost\"` 精确匹配拦，"
        "因为 \".localhost\" 作为后缀匹配不到裸的 \"localhost\"）。"
        "两处是互补关系而不是重复：精确匹配管裸名，后缀表管子域 —— "
        "所以只测「localhost 被拒」是覆盖不到这条的。",
    ),
    Mutation(
        ident="M46 生产环境的剖析端口不再校验回环",
        rel_path="services/api/internal/config/config.go",
        old="\tif !c.IsProd() || c.AllowPublicPprofInProd {\n\t\treturn nil\n\t}\n\tif profiling.IsLoopbackAddr(c.PprofAddr) {\n\t\treturn nil\n\t}",
        new="\tif true {\n\t\treturn nil\n\t}\n\tif profiling.IsLoopbackAddr(c.PprofAddr) {\n\t\treturn nil\n\t}",
        expect_fail=["TestValidate_PprofRejectedInProd"],
        why="校验被短路后，生产环境写 `PPROF_ADDR=:6060` 就能起端口。"
        "而空 host 在 Go 里绑的是通配地址（所有网卡）—— 形状最像「本机端口」，实际全网可达。"
        "剖析端点可以读到进程内存：heap/allocs 带出分配点参数内容，"
        "goroutine?debug=2 带出每个协程的完整栈。"
        "危险形态与 validateMockProviders 同构：服务照常启动，没有任何测试变红。",
        note="只测「非回环被拒」不够 —— 还要有「回环被放行」的正控制（表里已含），"
        "否则一个「一律拒绝」或「一律放行」的坏实现都能让拒绝用例全绿。",
    ),
    Mutation(
        ident="M47 剖析端点回环判定把空 host 当作本机",
        rel_path="services/api/internal/platform/profiling/profiling.go",
        old='\thost = strings.TrimSpace(host)\n\thost = strings.Trim(host, "[]")\n\tif host == "" {\n\t\treturn false\n\t}',
        new='\thost = strings.TrimSpace(host)\n\thost = strings.Trim(host, "[]")\n\tif host == "" {\n\t\treturn true\n\t}',
        expect_fail=["TestIsLoopbackAddr_AdjacentBoundaries"],
        why="这条是 M46 的放大器：生产校验唯一依赖 IsLoopbackAddr，"
        "它一旦把空 host 判成回环，M46 那条短路就等于默认放行。"
        "两处都要有 —— 同一不变式在 N 处实现时只堵一处等于没堵。",
        note="本条与 M46 的区别：M46 让校验不执行，本条让校验执行了但判错。"
        "只堵一条的话，另一个方向的坏实现仍然能通过。",
    ),
    Mutation(
        ident="M48 剖析端点把 cmdline 挂进白名单",
        rel_path="services/api/internal/platform/profiling/profiling.go",
        old='var profiles = []string{\n\t"allocs",',
        new='var profiles = []string{\n\t"cmdline",\n\t"allocs",',
        expect_fail=["TestProfiles_NoForbiddenNameIsMounted"],
        why="cmdline 会把整个进程命令行原样吐出，"
        "而本服务的命令行参数里可能有 DATABASE_URL（含数据库密码）与 JWT_SECRET。"
        "它是 Go 标准 pprof 五件套（profile/trace/cmdline/symbol/index）之一，"
        "所以「顺手把它也挂上」是一个极其自然的错误 —— "
        "而挂上之后端点完全正常，不会有任何报错。",
        note="trace 与 symbol 同样危险但没单独立变异：它们与 cmdline 走同一条注册路径"
        "（都在 profiles 切片里），一条变异即覆盖三者。"
        "\n\n★ 这条第一次跑时是**未证明**，而它抓得住的原因是个陷阱。"
        "最初指定的是 TestHandler_UnknownProfileIsNotReachable（运行时判 404），"
        "但它**不会红** —— 因为 pprof.Handler(\"cmdline\") 内部 Lookup 返回 nil，"
        "端点自己就回 404，与「没注册」外表一模一样。"
        "实际抓住它的是另一条遍历 ProfileNames() 的测试（它把 cmdline 算作「应该可用」），"
        "于是统计出来是「红了但不是指定测试」＝ 未证明。"
        "\n两个错叠在一起：(1) 守卫只靠运行时行为判定，而那个行为恰好与正确状态同形；"
        "(2) 另一条测试遍历了实现自己的清单，等于白名单给自己作证。"
        "修法是加静态断言 TestProfiles_NoForbiddenNameIsMounted —— "
        "「白名单里出现 cmdline」这个事实本身就是要守的不变式，不该依赖运行时巧合。"
        "**遍历实现自己的清单来验证它自己，是铁律 20 那个形态。**",
    ),
    Mutation(
        ident="M49 剖析端点用 pprof.Handler(\"profile\") 注册 CPU profile",
        rel_path="services/api/internal/platform/profiling/profiling.go",
        old='mux.Handle("/debug/pprof/profile", guardSeconds(maxSeconds, http.HandlerFunc(httppprof.Profile)))',
        new='mux.Handle("/debug/pprof/profile", guardSeconds(maxSeconds, httppprof.Handler("profile")))',
        expect_fail=["TestHandler_CPUProfileActuallyWorks"],
        why="这是本轮实测踩到的真坑：pprof.Handler 内部做 pprof.Lookup(name)，"
        "而 \"profile\" 不是 runtime/pprof 里的伪 profile（Lookup 返回 nil）⇒ 端点回 404。"
        "失败形态极隐蔽：heap/goroutine/allocs 全部 200 且有数据，"
        "唯独最需要的 CPU profile 是 404 —— 而 404 在一次仓促排障里"
        "很容易被读成「这个端点没开」，结论就变成「我们没有 CPU profiling 能力」。"
        "直觉写法（遍历白名单统一用 pprof.Handler）恰好就是这个错误。",
        note="守卫的判据是「能否解出 CPU profile 的语义标记」而不是「状态码是否 200」—— "
        "只断言 200 的话，404 这个失败形态压根抓不到；"
        "反过来第一版断言的是「首字节魔数 0x0A」，那是猜的（实测 0x48），已改成语义字符串断言。",
    ),
    Mutation(
        ident="M50 剖析端点的采集时长上限被静默钳制而非拒绝",
        rel_path="services/api/internal/platform/profiling/profiling.go",
        old="\t\tif sec > maxSeconds {\n\t\t\thttp.Error(w,",
        new="\t\tif sec > maxSeconds {\n\t\t\tsec = maxSeconds\n\t\t\tr.URL.RawQuery = \"seconds=\" + strconv.Itoa(sec)\n\t\t}\n\t\tif false {\n\t\t\thttp.Error(w,",
        expect_fail=["TestGuardSeconds_RejectsOverLimit"],
        why="Go 的 net/http/pprof.Profile 对 seconds **没有上界**"
        "（ParseInt 之后直接拿去 sleep），所以 ?seconds=99999 会让一个 handler "
        "线程占用 27 小时；而 profiling 期间运行时持续采样，进程比平时慢一截 —— "
        "一条无上限的端点等于自我施加的 DoS。"
        "改成静默钳制更隐蔽：请求方以为采了 10 分钟，实际是 1 分钟，"
        "而报告里写的是「10 分钟 CPU 采样」—— 结论与请求不一致。",
        note="变异体改写 r.URL.RawQuery 而不是只改局部变量 sec："
        "下游 httppprof.Profile 读的是 r.FormValue(\"seconds\")，"
        "只改局部变量的话下游拿到的仍是原始请求值 —— 那样这个变异体描述的后果不成立。"
        "写变异体前先问「它描述的后果真的会发生吗」（M36 曾栽在这上面）。",
    ),

    # =====================================================================
    # 第五轮：高并发资源耗尽面（连接上限 / 并发闸门 / 熔断 / 容量配置）
    # =====================================================================
    Mutation(
        ident="M51 连接上限对非法值静默兜底",
        rel_path="services/api/internal/platform/connlimit/listener.go",
        old='\tif n <= 0 {\n\t\tpanic("connlimit: 连接上限必须为正数（0/负数会静默变成\'不限\'）")\n\t}',
        new='\tif n <= 0 {\n\t\tn = 1 << 30\n\t}',
        expect_fail=["TestNewLimitListener_RejectsNonPositive"],
        why="传 0/负数时静默换成一个极大的值，等于「以为设了连接上限、实际没有」。"
        "调用点几乎总是从配置里读到了空值，而这类失效**不会自己暴露**："
        "服务照常启动、压测照常通过，只在被慢速攻击抽干 fd 的那天才显形。",
        note="判据是「非法输入必须响」，不是「非法输入被换算成某个合理值」—— "
        "后者是更危险的实现，因为它把配置事故伪装成了正常行为。",
    ),
    Mutation(
        ident="M52 连接关闭时不归还槽位",
        rel_path="services/api/internal/platform/connlimit/listener.go",
        old='\tc.releaseOnce.Do(c.release)',
        new='\tc.releaseOnce.Do(func() {})',
        expect_fail=["TestLimitListener_CapsConcurrentConns"],
        why="槽位只借不还 ⇒ 连接上限会在**第一次**被占满之后永久卡死："
        "现象是「服务活着但再也不接受新连接」，与进程假死难以区分。"
        "它是这个组件最容易被写漏的一行 —— 占槽那半显眼，还槽那半藏在 Close 里。",
        note="用 releaseOnce 而不是直接 release 是另一个独立风险（重复 Close 会归还两次、"
        "把上限慢慢放大）。本条只盖「不归还」这个方向；两条都在测试里各有断言。",
    ),
    Mutation(
        ident="M53 探针豁免表被顺带扩大",
        rel_path="services/api/internal/httpapi/concurrency.go",
        old='var probeExemptPaths = map[string]struct{}{\n\t"/healthz": {},\n\t"/readyz":  {},\n}',
        new='var probeExemptPaths = map[string]struct{}{\n\t"/healthz": {},\n\t"/readyz":  {},\n\t"/metrics": {},\n}',
        expect_fail=["TestProbeExemptPaths_IsExactlyTheExpectedSet"],
        why="豁免表每多一条，就多一个「过载时仍然无条件放行」的路径。"
        "加一条的动机总是合理的（metrics 看着就该放行），而后果是那段路径"
        "在系统被压垮时依然照单全收 —— 容量闸门对它形同不存在。",
        note="★ 这条守卫必须是**静态**断言（名单写死在测试里），不能遍历 probeExemptPaths 自身。"
        "遍历式断言 = 白名单给自己作证：表扩容时它照样绿。这正是 M48 踩过的形态。",
    ),
    Mutation(
        ident="M54 探针豁免写成前缀匹配",
        rel_path="services/api/internal/httpapi/concurrency.go",
        old='\t_, ok := probeExemptPaths[r.URL.Path]\n\treturn ok',
        new='\tp := r.URL.Path\n'
            '\treturn (len(p) >= 8 && p[:8] == "/healthz") || (len(p) >= 7 && p[:7] == "/readyz")',
        expect_fail=["TestConcurrencyGate_DoesNotExemptLookalikePaths"],
        why="前缀匹配会让 /healthzz、/healthz/extra、/readyz/admin 一并放行。"
        "它比「表里多加一条」更隐蔽：表本身没变，评审时看到的还是那两条，"
        "但匹配规则已经宽了 —— 将来任何以这两个前缀开头的接口都自动免疫闸门。",
        note="这是铁律 20 的形态：只测「该放行的放行了」抓不到「掩码写宽」。"
        "测试里逐条列了形近路径（/healthzz、/healthz/extra、/readyz/admin、/health）"
        "作为边界外侧的放行断言。"
        "\n\n★ 2026-10-11 四轴 review：这条的锚点原本是调用点的 "
        "`if _, ok := probeExemptPaths[r.URL.Path]; ok {`，"
        "而那次把判定收进了 isProbeExempt（为了让「拿路径的哪个字段」有单一改动点，见 M64），"
        "锚点随之失配 —— `--audit` 立刻报「命中 0 次」。"
        "这正是它该有的表现：**重构会让锚点失效，而失效必须是响的**，"
        "否则这条会变成恒为「未证明」的假条目。现已改钉到函数体。",
    ),
    Mutation(
        ident="M55 闸门拒绝重新变成逐条 Error 日志",
        rel_path="services/api/internal/httpapi/response.go",
        old='\treturn !errors.Is(err, domain.ErrServiceOverloaded)',
        new='\treturn true',
        expect_fail=["TestShouldLogAsError"],
        why="闸门拒绝是过载时**按请求频率**产生的 5xx。重新对每一条都记 Error，"
        "日志量就正比于请求量 —— 而日志 I/O 正是故障时最该省下的资源："
        "磁盘写满会让服务从「降级但仍可用」变成「彻底不可用」。"
        "注意它把闸门「刻意排在访问日志之前」的那个安排从侧门绕过了。",
        note="守卫必须带**正控制**（普通 5xx 仍要记）：只断言「过载不记」的话，"
        "把函数写成恒 false 也能通过，而后果是所有真故障都静默。",
    ),
    Mutation(
        ident="M56 闸门拒绝回 429 而不是 503",
        rel_path="services/api/internal/httpapi/concurrency.go",
        old='\t\t\t\tWriteProblem(w, r, domain.ErrServiceOverloaded)',
        new='\t\t\t\tWriteProblem(w, r, domain.ErrRateLimited)',
        expect_fail=["TestConcurrencyGate_RejectsBeyondLimit"],
        why="429 的含义是「**你**请求太多」，503 是「**我**这边不行了」。"
        "把容量问题答成 429，客户端就会按「等一秒重试」处理一个只有扩容"
        "才能解决的问题，而且永远不会把它当容量信号上报 —— "
        "故障表现正常、根因被永久掩盖。",
        note="两个码都能「挡住请求」，所以只断言「被拦住」的测试抓不到这条。"
        "必须断言具体的状态码。",
    ),
    Mutation(
        ident="M57 限流器不再检查熔断",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old='\tif l.breaker != nil {\n\t\tif berr := l.breaker.Allow(); berr != nil {',
        new='\tif false {\n\t\tif berr := l.breaker.Allow(); berr != nil {',
        expect_fail=["TestAllow_BreakerStopsHittingBackend"],
        why="熔断器的**全部价值**在于「打开之后不再去试」。不检查它，"
        "就退化成「每个请求仍然付一次判定超时」—— 而 Decide 的降级逻辑"
        "只解决「失败怎么答」，从来没解决「还要不要每次都去试」。"
        "现象是 Redis 挂掉期间整站持续变慢，且没有任何报错指向这里。",
        note="这条守卫必须**数后端调用次数**，不能只断言返回值。"
        "一个「每次都去试、试完照样返回错误」的实现能让所有返回值断言通过，"
        "而它一秒钟资源都没省下来。",
    ),
    Mutation(
        ident="M58 调用方取消被计入熔断失败",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old='\t\t\t\tl.breaker.Abort()',
        new='\t\t\t\tl.breaker.Report(false)',
        expect_fail=["TestAllow_CallerCancelDoesNotFeedBreaker"],
        why="ctx 取消（客户端断开、上游超时）拿不到结论，不是「后端不可用」的证据。"
        "把它记成失败之后，一次客户端集体重连就足以把熔断器喂开 —— "
        "好端端的 Redis 被自己的客户端挤下线，而且恢复要等一整个冷却周期。",
        note="Abort 的存在还有第二个理由：不撤销半开期的探测位，"
        "那个被取消的探测会把熔断器永久卡在半开（测试 TestBreaker_AbortUnsticksHalfOpen 覆盖）。",
    ),
    Mutation(
        ident="M59 前置校验也喂熔断",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old='\t\treturn Result{}, fmt.Errorf("%w: 窗口与阈值必须为正数", ErrBackendUnavailable)',
        new='\t\tif l.breaker != nil {\n\t\t\tl.breaker.Report(false)\n\t\t}\n\t\treturn Result{}, fmt.Errorf("%w: 窗口与阈值必须为正数", ErrBackendUnavailable)',
        expect_fail=["TestAllow_PrecheckDoesNotFeedBreaker"],
        why="「窗口/阈值缺省」是配置错误，不是后端故障。让它也走熔断记账，"
        "一个漏配窗口的调用点就能把熔断器喂到打开 —— 真实的 Redis 故障还没发生，"
        "限流已经全量降级了。判据：**只有真的碰过远端之后的失败**才算后端的证据。",
        note="这条很容易在重构时被破坏：把 breaker 检查上移到函数最前面"
        "（「越早越好」的直觉）就会顺带把前置校验圈进去。",
    ),
    Mutation(
        ident="M60 并发闸门的默认值被改成 0",
        rel_path="services/api/internal/config/config.go",
        old='\tMaxInflightRequests int `env:"MAX_INFLIGHT_REQUESTS" envDefault:"2048"`',
        new='\tMaxInflightRequests int `env:"MAX_INFLIGHT_REQUESTS" envDefault:"0"`',
        expect_fail=["TestDefaults_AreProtective"],
        why="0 是「显式关闭」的合法取值，所以这条改动**不会让任何边界测试变红**，"
        "服务照常启动、压测照常绿 —— 只有 TestDefaults_AreProtective 这种"
        "专门断言「默认必须为正」的测试能抓住它。而它描述的后果是："
        "所有未显式配置的部署都在裸奔。",
        note="容量保护最可能的失效方式不是配错值，而是默认值被悄悄改成 0/关闭。"
        "所以要有一条把「默认必须为正」写死的断言，且它必须用 unsetEnv 读**真默认值**"
        "（baseEnv 显式设过的值会盖住它）。",
    ),
    Mutation(
        ident="M61 熔断器半开期不再独占探测",
        rel_path="services/api/internal/platform/circuitbreaker/breaker.go",
        old='\tcase StateHalfOpen:\n\t\tif b.halfOpenInFlight {\n\t\t\treturn ErrOpen\n\t\t}\n\t\tb.halfOpenInFlight = true\n\t\treturn nil',
        new='\tcase StateHalfOpen:\n\t\treturn nil',
        expect_fail=["TestBreaker_HalfOpenAllowsSingleProbeAndClosesOnSuccess"],
        why="半开期放行所有调用 = 冷却一到就给依赖一次全量冲击。"
        "故障常常就是这么被「试探性流量」二次打垮的 —— 恢复期本身也该是渐进的。",
        note="halfOpenInFlight 字段是这个不变式的载体。去掉它之后代码依然编译、"
        "状态机看起来也「对」，只有「只放一个」这条断言会红。",
    ),
    Mutation(
        ident="M62 熔断器成功时不重置失败计数",
        rel_path="services/api/internal/platform/circuitbreaker/breaker.go",
        old='\t\tif success {\n\t\t\tb.failures = 0\n\t\t\treturn\n\t\t}\n\t\tb.failures++',
        new='\t\tif success {\n\t\t\treturn\n\t\t}\n\t\tb.failures++',
        expect_fail=["TestBreaker_SuccessResetsFailureCount"],
        why="不重置就变成「累计失败」而非「连续失败」：一个 50% 失败率的抖动依赖"
        "（其实还能用）迟早会攒够阈值被熔断 —— 那是自伤，且概率随运行时长单调上升。",
        note="「连续」还是「累计」是一句话的差别，日志与状态机都看不出区别，"
        "只有交错成功/失败的序列能把它区分出来。",
    ),

    # =====================================================================
    # 2026-10-11 四轴 review 挖出的两条真缺陷 —— 给它们的守卫配的变异体
    # =====================================================================
    Mutation(
        ident="M63 内部判定超时被改回「按错误链」判成调用方取消",
        rel_path="services/api/internal/ratelimit/ratelimit.go",
        old='\t\t\tcase noConclusion:\n\t\t\t\tl.breaker.Abort()',
        new='\t\t\tcase noConclusion || errors.Is(err, context.DeadlineExceeded):\n'
            '\t\t\t\tl.breaker.Abort()',
        expect_fail=["TestAllow_InternalJudgeTimeoutFeedsBreaker"],
        why="这正是 review 前的形态，而且它**看着更简洁**（少一个变量、少一行赋值），"
        "所以很容易被后来的人「顺手简化」回去。它错在两件事叠加："
        "BackendError.Unwrap() 会把 Cause 透出来，而**内部** judgeCtx 超时时 "
        "go-redis 在池等待路径上原样返回 ctx.Err()（context.DeadlineExceeded）。"
        "于是内部判定超时被读成「调用方取消」→ 走 Abort() 不计失败 → "
        "熔断器对**最主要的**故障形态（后端卡住 + 池被占满）永远打不开。"
        "现象是「Redis 挂时整站持续变慢」，没有报错、没有日志指向这里。"
        "行为上它与原形态等价：三处分支的走向完全一致"
        "（真取消时 noConclusion 为真、原始形态走第二个 clause，同样到 Abort）。",
        note="★ 指定的守卫必须是 TestAllow_InternalJudgeTimeoutFeedsBreaker，"
        "不能是 TestAllow_CallerCancelDoesNotFeedBreaker —— 后者在错误链写法下"
        "**照样通过**（真取消走哪条分支都对）。这正是「单侧用例挡不住语义反转」："
        "一个「把所有失败都当无结论」的实现能通过全部取消用例，"
        "而它的后果是熔断形同不存在。所以两侧各有一条守卫，缺一不可。"
        "\n\n★ 这条变异体第一版写成了原形态（把 `noConclusion` 整个换成两条"
        " errors.Is 的 case），结果**编译不过** —— 变量变成「声明未使用」。"
        "harness 如实报的是「只有包级失败，没有具名测试」，而不是「抓住」。"
        "这正是方法论里那条：写变异体先问「它还能被编译吗」，"
        "再问「它描述的后果真的会发生吗」。多带 `noConclusion ||` 一项之后"
        "既保住了编译，又与原形态行为等价。",
    ),
    Mutation(
        ident="M64 探针豁免改回只看 r.URL.Path（放过百分号编码变体）",
        rel_path="services/api/internal/httpapi/concurrency.go",
        old='\tif r.URL.RawPath != "" {\n\t\treturn false\n\t}',
        new='\tif false {\n\t\treturn false\n\t}',
        expect_fail=["TestConcurrencyGate_DoesNotExemptPercentEncodedVariants"],
        why="net/http 对 `/%68ealthz` 给出 Path=\"/healthz\" + RawPath=\"/%68ealthz\"，"
        "而 chi 路由用的是 RawPath（非空时优先）⇒ chi 找不到 /healthz 这条路由、回 404。"
        "只看 Path 的话，这类请求**拿到闸门豁免却没有走到探针**："
        "既绕过容量闸门，又落进被刻意排在闸门**内层**的 AccessLog —— "
        "于是「过载时每个被拒请求不写日志」这个安排被从侧门打开，"
        "攻击者用编码变体刷请求就能既不被限并发、又持续产生日志。",
        note="这条与 M53（豁免表被扩大）、M54（豁免写成前缀匹配）是**三个不同的方向**："
        "M53 改的是「表里有哪些项」，M54 改的是「怎么比较」，"
        "M64 改的是「拿路径的哪个字段」。三条各自的守卫不可互相替代 —— "
        "LookalikePaths 那条在 M64 下照样全绿（那些路径的 Path 本来就不匹配）。",
    ),
]


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def go_test(packages: str) -> tuple[int, set[str], str]:
    """跑测试，返回 (退出码, 失败的测试全名集合, 原始输出)。"""
    try:
        proc = subprocess.run(
            ["go", "test", "-count=1", "-json", *packages.split()],
            cwd=API_DIR,
            capture_output=True,
            text=True,
            timeout=GO_TEST_TIMEOUT,
            env={**os.environ, "DATABASE_URL": DATABASE_URL},
        )
    except subprocess.TimeoutExpired:
        return 124, set(), f"go test 超过 {GO_TEST_TIMEOUT}s 未结束（判定为卡住，不算证明）"

    failed: set[str] = set()
    plain: list[str] = []
    for line in proc.stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            plain.append(line)
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            plain.append(line)
            continue
        if ev.get("Action") == "fail" and ev.get("Test"):
            failed.add(ev["Test"])
    if proc.stderr.strip():
        plain.append(proc.stderr.strip())

    return proc.returncode, failed, "\n".join(plain)


def run_mutation(m: Mutation) -> tuple[bool, str]:
    path = ROOT / m.rel_path
    if not path.exists():
        return False, f"文件不存在: {m.rel_path}"

    m.original = path.read_bytes()
    before_hash = sha256(m.original)

    text = m.original.decode("utf-8")
    hits = text.count(m.old)
    if hits != 1:
        return False, f"锚点命中 {hits} 次（要求恰好 1 次）—— 变异未施加，本项未证明"

    path.write_bytes(text.replace(m.old, m.new, 1).encode("utf-8"))

    # 关键：把「当前正在被改的文件」登记到全局，任何退出路径都会还原它。
    #
    # 为什么不能只靠 try/finally：SIGTERM（超时、被 Ctrl-C、CI 取消）默认**直接杀进程**，
    # 不跑 Python 的 finally、不跑atexit。实测踩过 —— 跑到 M5 时被 SIGTERM 打断，
    # 结果 CAS 条件 `AND status = 'PENDING'` 永久留在了 repo/merchant.sql.go 里，
    # 之后所有测试都变红，而凶手（变异体）本身已经不在脚本的视野内。
    # 一个"改坏源码"的脚本必须能在被杀之后自我修复，否则它就是个投毒器。
    ACTIVE.append((path, m.original))
    try:
        code, failed, raw = go_test(PACKAGES)
    finally:
        ACTIVE.pop()
        path.write_bytes(m.original)
        after_hash = sha256(path.read_bytes())
        if after_hash != before_hash:
            raise SystemExit(
                f"致命：{m.rel_path} 还原后哈希不一致\n  before={before_hash}\n  after ={after_hash}\n"
                "  说明还原路径不干净，后面的结论全部不可信 —— 已中止。"
            )

    if code == 0:
        return False, "测试仍然全绿 —— 这个守卫是假的（或没被执行到）"

    caught = [name for name in m.expect_fail if name in failed]
    if not caught:
        return False, (
            f"测试确实红了，但**不是**预期的那些：期望 {m.expect_fail}，"
            f"实际失败 {sorted(failed) or '（只有包级失败，没有具名测试）'}\n"
            f"      退出码非 0 不足以证明守卫有效（编译错误也会非 0）"
        )

    return True, f"被 {', '.join(caught)} 抓住"


def parse_mut_index(ident: str) -> int | None:
    head = ident.split(" ", 1)[0]
    if head.startswith("M") and head[1:].isdigit():
        return int(head[1:])
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description="变异验证：证明守卫真的有牙")
    ap.add_argument("--from", dest="lo", type=int, default=None, help="只跑编号 >= N 的变异体")
    ap.add_argument("--to", dest="hi", type=int, default=None, help="只跑编号 <= N 的变异体")
    ap.add_argument(
        "--no-baseline",
        action="store_true",
        help="跳过基线全量测试（分批续跑时用，省一次基线跑的等待）",
    )
    ap.add_argument(
        "--audit",
        action="store_true",
        help="只做残留自检：检查源码里是否还留着上一轮没还原干净的变异体",
    )
    ap.add_argument(
        "--packages",
        default="./internal/...",
        help="go test 的包路径（默认全量）。环境不完整时可收窄，"
        "例如 PG/Redis 没起时先验不依赖它们的那些变异体",
    )
    args = ap.parse_args()

    # run_mutation 读的是模块级 PACKAGES，这里改它。
    global PACKAGES
    PACKAGES = args.packages

    _install_signal_guards()

    if args.audit:
        problems = audit_clean()
        if problems:
            print("残留自检**未通过**：")
            for p in problems:
                print(f"  - {p}")
            print(
                "\n这些必须先手工修好（或从 git 恢复对应文件）再跑验证。\n"
                "带着污染跑出来的任何结论都不可信 —— 基线会红，而红的原因"
                "看起来像测试的问题，不是源码被改坏。"
            )
            return 2
        print(f"残留自检通过：{len(MUTATIONS)} 条变异体全部处于原始状态，锚点均唯一命中。")
        return 0

    selected = []
    for m in MUTATIONS:
        idx = parse_mut_index(m.ident)
        if idx is None:
            selected.append(m)
            continue
        if args.lo is not None and idx < args.lo:
            continue
        if args.hi is not None and idx > args.hi:
            continue
        selected.append(m)

    if not selected:
        print("没有匹配的变异体。")
        return 2

    print("=" * 78)
    print(f"变异验证：{selected[0].ident.split(' ', 1)[0]}–"
          f"{selected[-1].ident.split(' ', 1)[0]}，共 {len(selected)} 条")
    print("=" * 78, flush=True)

    if args.no_baseline:
        print("已跳过基线（--no-baseline）\n", flush=True)
    else:
        # 基线之前先自检。顺序不能反：带着残留变异体跑出来的"基线红"会被
        # 误读成"新写的测试有问题"，而真凶在测试输出里永远不出现。
        problems = audit_clean()
        if problems:
            print("残留自检**未通过**，拒绝开跑：")
            for p in problems:
                print(f"  - {p}")
            return 2
        print(f"残留自检通过（{len(MUTATIONS)} 条变异体均为原始状态）", flush=True)

        baseline_code, _, baseline_raw = go_test(PACKAGES)
        if baseline_code != 0:
            print("基线不通过 —— 先让测试全绿再来验守卫。")
            print(baseline_raw)
            return 2
        print("基线：./internal/... 全部通过\n", flush=True)

    proven, unproven = 0, 0
    unproven_list: list[str] = []
    for i, m in enumerate(selected, 1):
        tag = m.ident.split(" ", 1)[0]
        print(f"[{i}/{len(selected)}] {m.ident}", flush=True)
        print(f"  改坏：{m.rel_path}", flush=True)
        ok, detail = run_mutation(m)
        print(f"  -> {'✅ 抓住' if ok else '❌ 未证明'}：{detail}\n", flush=True)
        if ok:
            proven += 1
        else:
            unproven += 1
            unproven_list.append(f"{tag} {m.ident}")

    print("=" * 78)
    print(f"结果：{proven}/{len(selected)} 个变异被**指定测试**抓住")
    for line in unproven_list:
        print(f"      未证明：{line}")
    if unproven:
        print(f"      {unproven} 个未证明 —— 这些守卫目前是无牙的，别当成已有保护")
    print("=" * 78, flush=True)
    return 0 if unproven == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
