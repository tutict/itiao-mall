#!/usr/bin/env bash
# 一键跑完所有守门项。任一项失败即非零退出。
#
# 为什么需要这个入口
# ------------------
# 仓库里守门资产不少（go test / 契约漂移 / 端到端 / 压测门禁 / 变异验证），
# 但它们散在 scripts/ 下、且没有 .github/ 目录。后果不是"跑起来麻烦"，而是
# **守卫会腐烂**：改了路由策略，access_test.go 还绿着，但那个变异体三个月没人跑，
# 守卫到底还有没有牙已经没人知道了 —— 腐烂过程没有任何信号。
#
# 所以这里做两件事：
#   1. 把所有守门项串成一个入口，让人只需要记住一个命令
#   2. 断言 Go 测试的**条数下限**（见下面「静默失效」一节）
#
# 用法
# ----
#   bash scripts/check-all.sh              # 全量
#   bash scripts/check-all.sh --quick      # 跳过压测与端到端（秒级）
#
# 环境
# ----
#   DATABASE_URL   需要指向一个可清空的库（集成测试自己造数据、自己清理）。
#                  不设则集成测试整体 skip —— 所以脚本会显式检查并要求你设。

set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

QUICK=0
[ "${1:-}" = "--quick" ] && QUICK=1

# 测试条数下限。低于这个数说明有一批用例被 skip 了。
#
# 实测过的数字（2026-10-02，基线 987c6fb 之后）：
#   不设 DATABASE_URL → 27 条 PASS / 12 条 SKIP，退出码 0
#   设了 DATABASE_URL → 39 条 PASS / 0 条 SKIP，退出码 0
#
# 两个退出码都是 0，所以 CI（以及人）在环境没配好时会**以为一切正常**，
# 而丢掉的 12 条恰好是超卖、幂等、回调幂等、审核 CAS —— 最承重的那些。
# t.Skip 与真通过在退出码上无法区分，只能靠数条数。
MIN_GO_TESTS="${MIN_GO_TESTS:-38}"

FAILED=()
STEP=0
TOTAL_STEPS=7
QUICK_SKIP_REASON="（--quick 已跳过）"

step() {
  STEP=$((STEP + 1))
  printf '\n\033[1m[%d/%d] %s\033[0m\n' "$STEP" "$TOTAL_STEPS" "$1"
}

# 把一段命令包成"失败即记账但不中断"，这样一次跑完能看到全部结果，
# 而不是第一个失败就停 —— 排查时一轮看到全部问题比一轮修一个快得多。
run() {
  local label="$1"; shift
  if "$@"; then
    printf '\033[32m  ✓ %s\033[0m\n' "$label"
  else
    printf '\033[31m  ✗ %s\033[0m\n' "$label"
    FAILED+=("$label")
  fi
}

# ---- 1. 格式化与静态检查 ----
step "gofmt / go vet"
check_fmt() {
  local dirty
  dirty="$(cd services/api && gofmt -l .)"
  if [ -n "$dirty" ]; then
    echo "以下文件未格式化："
    echo "$dirty"
    return 1
  fi
  # 单独一个子 shell：直接 `cd` 会改掉当前 shell 的 cwd，
  # 后面所有相对路径都会跟着偏。踩过。
  (cd services/api && go vet ./...)
}
run "gofmt + go vet" check_fmt

# ---- 2. 契约漂移 ----
step "契约漂移（重跑 codegen 后必须无差异）"
check_drift() {
  bash scripts/gen.sh >/dev/null 2>&1
  # 注意：必须用 --exit-code。`git diff --quiet` 也能用，但它的退出码语义
  # 在重定向管道里容易被吞掉，这里用显式的 diff 更直白。
  if ! git diff --exit-code -- \
      services/api/internal/httpapi/gen \
      packages/api-contract/src/schema.d.ts >/dev/null; then
    echo "生成物与 openapi.yaml 不一致。差异："
    git diff --stat -- \
      services/api/internal/httpapi/gen \
      packages/api-contract/src/schema.d.ts
    echo "如果这是你有意改契约的结果，请把生成物一起提交。"
    return 1
  fi
}
run "契约无漂移" check_drift

# ---- 3. Go 测试（含条数下限）----
step "Go 测试"
check_go_test() {
  if [ -z "${DATABASE_URL:-}" ]; then
    cat >&2 <<'EOF'
DATABASE_URL 没设，集成测试会整体 skip —— 而它们正是超卖/幂等/回调/审核 CAS
那批最承重的用例。这种情况下的绿灯是没有意义的。

起本地依赖：bash scripts/dev.sh
EOF
    return 1
  fi

  # 用子 shell 跑，把输出收进来；不在这里 cd。
  local out
  if ! out="$(cd services/api && go test -count=1 -v ./... 2>&1)"; then
    printf '%s\n' "$out" | grep -E '^(--- FAIL|    --- FAIL)|\.go:[0-9]+:' | head -30 || true
    echo "（完整输出已省略，用 'cd services/api && go test -v ./...' 看全量）"
    return 1
  fi

  local pass skip
  pass="$(printf '%s\n' "$out" | grep -cE '^(--- PASS|    --- PASS)' || true)"
  skip="$(printf '%s\n' "$out" | grep -cE '^(--- SKIP|    --- SKIP)' || true)"

  echo "  PASS=$pass  SKIP=$skip（下限 $MIN_GO_TESTS）"

  if [ "$skip" -gt 0 ]; then
    echo "  有 $skip 条被 skip —— 逐条列出来，因为 skip 掉的往往正是最承重的："
    printf '%s\n' "$out" | grep -E '^(--- SKIP|    --- SKIP)' | sed 's/^/    /'
  fi

  if [ "$pass" -lt "$MIN_GO_TESTS" ]; then
    echo "  PASS 条数 $pass < 下限 $MIN_GO_TESTS：有一批用例没被执行。"
    echo "  常见原因：Redis 没起（商户相关用例会 skip）或 DATABASE_URL 指到了空库。"
    return 1
  fi
}
run "Go 测试（条数下限 $MIN_GO_TESTS）" check_go_test

# ---- 4. 压测脚本自测 ----
step "压测比对逻辑单测"
run "compare.test.mjs" node --test tools/loadtest/scripts/compare.test.mjs

# ---- 5. 权限/审核守卫的牙齿 ----
step "变异验证（权限 + 审核 + 错误泄露 + 分页）"
resolve_python() {
  if [ -n "${PYTHON_BIN:-}" ]; then echo "$PYTHON_BIN"; return 0; fi
  local managed="C:/Users/tutic/.workbuddy/binaries/python/versions/3.13.12/python.exe"
  if [ -x "$managed" ]; then echo "$managed"; return 0; fi
  local c
  for c in python3 python; do
    if command -v "$c" >/dev/null 2>&1; then command -v "$c"; return 0; fi
  done
  return 1
}
check_guards() {
  local py
  if ! py="$(resolve_python)"; then
    echo "找不到 Python。设 PYTHON_BIN 指向解释器后重试。" >&2
    return 1
  fi
  "$py" -u scripts/verify-guards.py
}
run "verify-guards.py（变异全部被指定测试抓住）" check_guards

# ---- 6. 性能门禁的牙齿 ----
step "压测门禁变异验证"
if [ "$QUICK" = "1" ]; then
  echo "  $QUICK_SKIP_REASON"
else
  run "verify-perf-gate.sh" bash scripts/verify-perf-gate.sh
fi

# ---- 7. 端到端验收 ----
step "用户端端到端验收"
if [ "$QUICK" = "1" ]; then
  echo "  $QUICK_SKIP_REASON"
else
  # 需要先起 API 与 web 预览。缺哪台就明说，不静默跳过 ——
  # 静默跳过正是"守卫腐烂"的第一步。
  run "acceptance-app.mjs（需 :8080 API + :8081 web 预览）" node scripts/acceptance-app.mjs
fi

# ---- 汇总 ----
printf '\n'
if [ ${#FAILED[@]} -eq 0 ]; then
  printf '\033[32m全部 %d 项通过。\033[0m\n' "$STEP"
  exit 0
fi

printf '\033[31m%d 项失败（共 %d 项）：\033[0m\n' "${#FAILED[@]}" "$STEP"
for f in "${FAILED[@]}"; do
  printf '  - %s\n' "$f"
done
exit 1
