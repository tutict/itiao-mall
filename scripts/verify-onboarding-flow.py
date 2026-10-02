#!/usr/bin/env python
"""商户入驻全链路验收（对活服务器跑，退出码即结论）。

为什么要有这个脚本
------------------
单测能证明「策略表里这一条标了 requireApprovedMerchant」，
但证明不了「真实请求穿过 CORS → authGate → merchantStatusGate → chi 路由」这条链上
确实照做了。中间任何一环顺序被改、前缀匹配被绕过，单测都是绿的。

所以这里用真 HTTP 把整条生命周期走一遍，每一步断言**精确状态码**：

    注册(201, PENDING) → 业务接口 403 → 平台驳回(理由必填) → 商户读到驳回理由
    → 改资料重新提交(回 PENDING) → 平台通过 → 业务接口不再是 403
    → 重复审核 404(CAS) → 重复注册 409 → 跨角色越权各打一遍

退出码：0 = 全部符合预期；非 0 = 有一步不符（并打印实际响应体）。

用法：
    C:/Users/tutic/.workbuddy/binaries/python/versions/3.13.12/python.exe \
        scripts/verify-onboarding-flow.py [--base http://localhost:8080]
"""

from __future__ import annotations

import argparse
import json
import random
import sys
import time
import urllib.error
import urllib.request

ORIGIN = "http://localhost:8081"

# 注：这里刻意用 urllib 而不是 curl + shell 变量。
# 本机的 shell 输出层会把长字符串截断（实测 356 字符的 JWT 显示成 21 字符），
# token 一律用解析器读，肉眼看 shell 输出等于在猜。


class Failure(Exception):
    pass


class Client:
    def __init__(self, base: str) -> None:
        self.base = base.rstrip("/")

    def call(self, method: str, path: str, body=None, token: str | None = None):
        url = self.base + path
        data = None
        headers = {"Origin": ORIGIN}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if token:
            headers["Authorization"] = "Bearer " + token

        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read().decode("utf-8", "replace")
                return resp.status, _maybe_json(raw)
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            return e.code, _maybe_json(raw)
        except urllib.error.URLError as e:
            raise Failure(f"连不上 {url}: {e}") from e


def _maybe_json(raw: str):
    if not raw:
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return raw


def brief(payload, limit: int = 160) -> str:
    if payload is None:
        return "(空)"
    if isinstance(payload, str):
        return payload[:limit]
    return json.dumps(payload, ensure_ascii=False)[:limit]


class Report:
    def __init__(self) -> None:
        self.rows: list[tuple[str, str, str, bool]] = []

    def check(self, name: str, expected, actual, payload=None, extra: bool = True) -> None:
        ok = extra and (actual == expected)
        self.rows.append((name, str(expected), str(actual), ok))
        mark = "OK  " if ok else "FAIL"
        line = f"  [{mark}] {name}: 期望 {expected}，实际 {actual}"
        if not ok:
            line += f"\n         响应: {brief(payload)}"
        print(line, flush=True)
        if not ok:
            raise Failure(f"{name} 期望 {expected} 实际 {actual}｜响应 {brief(payload)}")

    def expect(self, name: str, cond: bool, detail: str = "") -> None:
        self.rows.append((name, "True", str(cond), cond))
        mark = "OK  " if cond else "FAIL"
        print(f"  [{mark}] {name}{(' —— ' + detail) if detail else ''}", flush=True)
        if not cond:
            raise Failure(f"{name}｜{detail}")

    @property
    def failed(self) -> int:
        return sum(1 for *_, ok in self.rows if not ok)


def unique_phone() -> str:
    return "1" + f"{random.randrange(10**10):010d}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:8080")
    args = ap.parse_args()

    c = Client(args.base)
    rep = Report()

    status, payload = c.call("GET", "/healthz")
    if status != 200:
        print(f"API 未就绪（/healthz -> {status}）", file=sys.stderr)
        return 2
    print(f"API {args.base} 就绪\n")

    phone = unique_phone()
    password = "merchant-secret-1"

    try:
        print("== 1. 匿名访问边界 ==")
        st, p = c.call("GET", "/api/v1/merchant/profile")
        rep.check("未登录读入驻资料", 401, st, p)
        st, p = c.call("GET", "/api/v1/merchant/products")
        rep.check("未登录读商户商品", 401, st, p)

        print("\n== 2. 注册校验 ==")
        st, p = c.call("POST", "/api/v1/auth/merchant/register",
                       {"phone": phone, "password": "short", "shop_name": "短密码店"})
        rep.check("密码过短被拒", 400, st, p)
        st, p = c.call("POST", "/api/v1/auth/merchant/register",
                       {"phone": phone, "password": password, "shop_name": "   "})
        rep.check("店名为空白被拒", 400, st, p)

        print("\n== 3. 注册成功 ==")
        st, p = c.call("POST", "/api/v1/auth/merchant/register",
                       {"phone": phone, "password": password, "shop_name": "验收测试店",
                        "contact_name": "验收联系人", "contact_phone": phone})
        rep.check("注册", 201, st, p)
        rep.expect("注册返回 merchant 身份", p and p.get("user", {}).get("role") == "merchant",
                   repr(p and p.get("user", {}).get("role")))
        merchant_token = p["access_token"]
        merchant_id = p["user"]["id"]

        print("\n== 4. 审核中：业务被挡、资料可读 ==")
        st, p = c.call("GET", "/api/v1/merchant/profile", token=merchant_token)
        rep.check("审核中读自己的资料", 200, st, p)
        rep.expect("状态为 PENDING", p and p.get("status") == "PENDING", repr(p and p.get("status")))
        st, p = c.call("GET", "/api/v1/merchant/products", token=merchant_token)
        rep.check("审核中读商户商品", 403, st, p)
        st, p = c.call("POST", "/api/v1/merchant/orders/x/ship", token=merchant_token)
        rep.check("审核中发货", 403, st, p)
        rep.expect("403 里带上了当前状态（前端要靠它路由）",
                   p and "PENDING" in json.dumps(p, ensure_ascii=False),
                   brief(p))

        print("\n== 5. 平台驳回 ==")
        st, p = c.call("POST", "/api/v1/auth/admin/login",
                       {"phone": "13700000000", "password": "admin123"})
        rep.check("管理员登录", 200, st, p)
        admin_token = p["access_token"]

        st, p = c.call("GET", "/api/v1/admin/merchants?status=PENDING&page_size=100", token=admin_token)
        rep.check("待审队列", 200, st, p)
        ids = [it["user_id"] for it in (p or {}).get("items", [])]
        rep.expect("刚注册的商户出现在待审队列里", merchant_id in ids,
                   f"队列 {len(ids)} 条")

        st, p = c.call("POST", f"/api/v1/admin/merchants/{merchant_id}/review",
                       {"decision": "REJECTED"}, token=admin_token)
        rep.check("驳回不给理由", 400, st, p)

        st, p = c.call("POST", f"/api/v1/admin/merchants/{merchant_id}/review",
                       {"decision": "REJECTED", "reject_reason": "营业执照照片模糊，请重新上传"},
                       token=admin_token)
        rep.check("驳回", 200, st, p)
        rep.expect("状态为 REJECTED", p and p.get("status") == "REJECTED", repr(p and p.get("status")))

        st, p = c.call("GET", "/api/v1/merchant/profile", token=merchant_token)
        rep.check("被驳回后仍能读资料", 200, st, p)
        rep.expect("商户能读到驳回理由",
                   p and "营业执照" in (p.get("reject_reason") or ""),
                   repr(p and p.get("reject_reason")))
        st, p = c.call("GET", "/api/v1/merchant/products", token=merchant_token)
        rep.check("被驳回后读商户商品", 403, st, p)

        print("\n== 6. 重新提交 ==")
        st, p = c.call("PUT", "/api/v1/merchant/profile",
                       {"shop_name": "验收测试店（已补充资质）"}, token=merchant_token)
        rep.check("改资料重新提交", 200, st, p)
        rep.expect("状态回到 PENDING", p and p.get("status") == "PENDING", repr(p and p.get("status")))
        rep.expect("驳回理由被清空", p and not p.get("reject_reason"),
                   repr(p and p.get("reject_reason")))

        print("\n== 7. 平台通过 ==")
        st, p = c.call("POST", f"/api/v1/admin/merchants/{merchant_id}/review",
                       {"decision": "APPROVED"}, token=admin_token)
        rep.check("通过", 200, st, p)
        rep.expect("状态为 APPROVED", p and p.get("status") == "APPROVED", repr(p and p.get("status")))

        # 商户商品/订单接口尚未实现（任务 13/14），所以通过闸门后的信号是 404 而不是 200。
        # 404 恰好证明请求越过了闸门并进到了 chi 路由 —— 和 403 是两件完全不同的事。
        st, p = c.call("GET", "/api/v1/merchant/products", token=merchant_token)
        rep.check("通过后读商户商品（越过了闸门，进到未实现的路由）", 404, st, p)

        print("\n== 8. CAS：一条申请只能裁决一次 ==")
        st, p = c.call("POST", f"/api/v1/admin/merchants/{merchant_id}/review",
                       {"decision": "REJECTED", "reject_reason": "反悔"}, token=admin_token)
        rep.check("二次审核应落空", 404, st, p)
        st, p = c.call("GET", "/api/v1/merchant/profile", token=merchant_token)
        rep.expect("二次审核没有改动状态", p and p.get("status") == "APPROVED",
                   repr(p and p.get("status")))

        print("\n== 9. 幂等与唯一约束 ==")
        st, p = c.call("POST", "/api/v1/auth/merchant/register",
                       {"phone": phone, "password": password, "shop_name": "重复注册店"})
        rep.check("同手机号重复注册", 409, st, p)

        print("\n== 10. 跨角色越权 ==")
        st, p = c.call("GET", "/api/v1/admin/merchants", token=merchant_token)
        rep.check("商户读审核队列", 403, st, p)
        st, p = c.call("GET", "/api/v1/merchant/profile", token=admin_token)
        rep.check("管理员读商户入驻资料", 403, st, p)
        st, p = c.call("GET", "/api/v1/admin/merchants", token="not-a-jwt")
        rep.check("坏 token 读审核队列", 401, st, p)

        st, p = c.call("POST", "/api/v1/auth/wechat/login", {"code": "any"})
        rep.check("用户端 mock 登录", 200, st, p)
        user_token = p["access_token"]
        st, p = c.call("GET", "/api/v1/admin/merchants", token=user_token)
        rep.check("用户读审核队列", 403, st, p)
        st, p = c.call("GET", "/api/v1/merchant/profile", token=user_token)
        rep.check("用户读商户入驻资料", 403, st, p)

    except Failure as e:
        print(f"\n❌ 中断：{e}")
        print(f"\n结果：{len(rep.rows) - rep.failed}/{len(rep.rows)} 步符合预期")
        return 1

    print(f"\n✅ 全部通过：{len(rep.rows)}/{len(rep.rows)} 步符合预期")
    print(f"   注册的测试商户 phone={phone} id={merchant_id}（留在库里，可手工清理）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
