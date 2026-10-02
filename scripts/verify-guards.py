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

退出码：0 = 全部变异都被指定的测试抓住；非 0 = 有未证明项。
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
API_DIR = ROOT / "services" / "api"

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
]


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def go_test(packages: str) -> tuple[int, set[str], str]:
    """跑测试，返回 (退出码, 失败的测试全名集合, 原始输出)。"""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-json", packages],
        cwd=API_DIR,
        capture_output=True,
        text=True,
        env={**os.environ, "DATABASE_URL": DATABASE_URL},
    )

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

    try:
        code, failed, raw = go_test("./internal/...")
    finally:
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


def main() -> int:
    print("=" * 78)
    print("变异验证：权限 / 入驻审核 守卫")
    print("=" * 78)

    baseline_code, _, baseline_raw = go_test("./internal/...")
    if baseline_code != 0:
        print("基线不通过 —— 先让测试全绿再来验守卫。")
        print(baseline_raw)
        return 2
    print("基线：./internal/... 全部通过\n")

    proven, unproven = 0, 0
    for m in MUTATIONS:
        print(f"[{m.ident}]")
        print(f"  改坏：{m.rel_path}")
        print(f"  理由：{m.why}")
        if m.note:
            print(f"  说明：{m.note}")
        ok, detail = run_mutation(m)
        print(f"  -> {'✅ 抓住' if ok else '❌ 未证明'}：{detail}\n")
        if ok:
            proven += 1
        else:
            unproven += 1

    print("=" * 78)
    print(f"结果：{proven}/{len(MUTATIONS)} 个变异被**指定测试**抓住")
    if unproven:
        print(f"      {unproven} 个未证明 —— 这些守卫目前是无牙的，别当成已有保护")
    print("=" * 78)
    return 0 if unproven == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
