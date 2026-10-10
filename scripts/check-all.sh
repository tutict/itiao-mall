#!/usr/bin/env bash
# 一键跑完所有守门项。任一项失败即非零退出。
#
# 为什么需要这个入口
# ------------------
# 仓库里守门资产不少（go test / 契约漂移 / 端到端 / 压测门禁 / 变异验证 /
# 依赖漏洞 / 活服务器探针），但它们散在 scripts/ 下、且没有 .github/ 目录。
# 后果不是"跑起来麻烦"，而是**守卫会腐烂**：改了路由策略，access_test.go 还绿着，
# 但那个变异体三个月没人跑，守卫到底还有没有牙已经没人知道了 ——
# 腐烂过程没有任何信号。
#
# 所以这里做两件事：
#   1. 把所有守门项串成一个入口，让人只需要记住一个命令
#   2. 断言 Go 测试的**条数下限**与 **SKIP 必须为 0**（见下面「静默失效」一节）
#
# 两条顺序上的硬约定
# ------------------
#  1. **变异体残留自检排在最前**。上一次变异验证被 SIGTERM 打断时，被改坏的
#     源码留在了工作区（实测 4 处），后面所有测试都会以看不懂的方式变红 ——
#     而且"红"会指向错误的文件。先自检，才能保证后面看到的红都是真红。
#  2. **需要活服务的项排在最后**（压测门禁 / 安全探针 / 端到端）。它们最重、
#     最依赖环境与 seed 数据，--quick 只跳过这一批。
#
# 用法
# ----
#   bash scripts/check-all.sh              # 全量
#   bash scripts/check-all.sh --quick      # 跳过需要活服务的那几项
#
# 环境
# ----
#   DATABASE_URL   需要指向一个可清空的库（集成测试自己造数据、自己清理）。
#                  不设则集成测试整体 skip —— 所以脚本会显式检查并要求你设。
#   REDIS_ADDR     可选。**不设也能全跑**：Redis 相关用例默认走进程内 miniredis，
#                  设了才连真机（见各包 newRedisForTest / newTestRedis 的注释）。
#   PYTHON_BIN     可选。缺省时按托管 Python → python3 → python 顺序找。
#   PG_COMPOSE_OVERRIDE
#                  可选，**只在宿主 5432 绑不上时才需要**（本机 5432 落在 Windows
#                  保留端口段，PG 只能映射到 15432）。值是**个人** compose 覆盖
#                  文件的路径，不入库，见 scripts/lib/dev-compose.sh。
#                  不设时按仓库的 docker-compose.dev.yml —— 也就是说，在 5432 被占
#                  的机器上不设它的后果是：门禁第 8 项按 5432 重建 PG → PG 起不来 →
#                  它后面的项像"坏了"一样全红，而根因是一条被静默丢掉的端口覆盖。

set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:/c/Program Files/Go/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

QUICK=0
[ "${1:-}" = "--quick" ] && QUICK=1

# 测试条数下限。
#
# 为什么需要数字而不是只看退出码：t.Skip 与真通过在**退出码上完全一样**
# （实测过 27 PASS/12 SKIP 与 39 PASS/0 SKIP 都是 exit 0），而丢掉的恰好是
# 超卖、幂等、回调幂等、审核 CAS 这批最承重的。
#
# 实测数字（2026-10-10）：
#   411 PASS / 0 SKIP（DATABASE_URL 指向 15432 的开发库，未设 REDIS_ADDR）
# 下限取 380（约 92%）。这个数**必须跟着实测值一起改**，否则它会慢慢变成一句
# 安慰话：原来写 280 而实测 411，等于允许 1/3 的用例整批消失还全绿。
# 真正灵敏的判据是下面的 SKIP=0，这个下限只是"整批没跑"的粗糙兜底。
MIN_GO_TESTS="${MIN_GO_TESTS:-380}"

FAILED=()
STEP=0
RAN=0
TOTAL_STEPS=10
QUICK_SKIP_REASON="（--quick 已跳过）"

step() {
  STEP=$((STEP + 1))
  printf '\n\033[1m[%d/%d] %s\033[0m\n' "$STEP" "$TOTAL_STEPS" "$1"
}

# 把一段命令包成"失败即记账但不中断"，这样一次跑完能看到全部结果，
# 而不是第一个失败就停 —— 排查时一轮看到全部问题比一轮修一个快得多。
#
# RAN 单独计数：--quick 跳过的那几项也会走到 step()（编号照旧占位），
# 所以 STEP 不能拿来当"实际跑了几项"—— 否则汇总会印"全部 10 项通过"，
# 而其实只跑了 7 项。那种汇总比没有汇总更坏：它让人以为覆盖面是全的。
run() {
  local label="$1"; shift
  RAN=$((RAN + 1))
  if "$@"; then
    printf '\033[32m  ✓ %s\033[0m\n' "$label"
  else
    printf '\033[31m  ✗ %s\033[0m\n' "$label"
    FAILED+=("$label")
  fi
}

# 解析 Python 解释器。放在最前面，因为第 1 步就要用它。
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

# ---- 1. 变异体残留自检（必须最先）----
#
# 为什么排第一：上一轮变异验证被 SIGTERM 杀掉时，被改坏的源码留在了工作区
# （实测留下 4 处）。那种状态下跑任何测试都可能红，而红的位置指向的是
# 变异体而不是真实缺陷 —— 先把"工作区到底干不干净"这件事确定下来，
# 后面所有结论才有意义。
step "变异体残留自检（工作区里是否留着上一轮没还原的变异）"
check_mutation_audit() {
  local py
  if ! py="$(resolve_python)"; then
    echo "找不到 Python。设 PYTHON_BIN 指向解释器后重试。" >&2
    return 1
  fi
  "$py" -u scripts/verify-guards.py --audit
}
run "verify-guards.py --audit" check_mutation_audit

# ---- 2. 格式化与静态检查 ----
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

# ---- 3. 契约漂移 ----
step "契约漂移（重跑 codegen 后生成物必须逐字节不变）"
#
# 判据：**重跑 codegen 之后生成物逐字节不变** —— 也就是「躺在磁盘上的生成物，
# 就是契约生成出来的那一份」。
#
# 为什么不用 `git diff --exit-code`（这条检查以前是那么写的）：
# 那是拿 HEAD 当基准线，于是把两件性质完全不同的事混成了一件 ——
#   (a) 生成物与契约不一致：真的错，必须红；
#   (b) 改了契约、已经重新生成过、但还没提交：完全正常的工作中间态。
# 后果是**每一次有意改契约都会把门禁判红**，而这恰恰是最需要这条检查
# 可靠工作的时刻；而且它的错误信息会把人往"改提交历史"上引。
#
# 判据该问的是"生成物是不是契约生成的"，不是"你提交了没有"。
# 提交卫生不需要这条检查操心：CI 跑在干净检出上，若有人提交了契约却没重新
# 生成，那时磁盘上的生成物与契约不符 —— 这条检查照样会红。
#
# 实现用**临时索引**比对，不碰工作区暂存区：先把当前生成物登记进临时索引，
# 跑 codegen，再看「索引 vs 工作区」有无差异。这样跑门禁不会顺手改掉
# 用户精心挑好的 staged 状态（`git add` 那种写法就会）。
#
# ⚠️ 临时索引必须落在**一定存在且可写**的目录里。曾经写在 `$ROOT/.workbuddy/`
# 下面 —— 那个目录被 gitignore，**干净检出里根本不存在**，于是 `git add` 以
# `Unable to create '...index.lock': No such file or directory`（exit 128）失败。
# 本机因为 .workbuddy/ 恰好存在，完全看不出来；换台机器/CI 就是第 3 步永远红。
# （实测踩过。）所以用 mktemp -d。
check_drift() {
  local paths=(
    services/api/internal/httpapi/gen
    packages/api-contract/src/schema.d.ts
  )
  local tmpdir idx
  if ! tmpdir="$(mktemp -d)"; then
    echo "无法创建临时目录（mktemp -d 失败）。" >&2
    return 1
  fi
  idx="$tmpdir/index"
  # 空索引 + 只加这几个路径 = 一条"当前磁盘状态"的基线。
  if ! GIT_INDEX_FILE="$idx" git add -A -- "${paths[@]}"; then
    echo "无法建立比对基线（git add 到临时索引失败）。" >&2
    rm -f "$idx"; rmdir "$tmpdir" 2>/dev/null || true
    return 1
  fi
  if ! bash scripts/gen.sh >/dev/null 2>&1; then
    echo "codegen 跑不起来 —— 这不是漂移，是生成器本身失败："
    bash scripts/gen.sh 2>&1 | tail -20
    rm -f "$idx"; rmdir "$tmpdir" 2>/dev/null || true
    return 1
  fi
  if ! GIT_INDEX_FILE="$idx" git diff --exit-code -- "${paths[@]}" >/dev/null; then
    echo "磁盘上的生成物不是契约的产物 —— 重跑 codegen 之后它变了。差异："
    GIT_INDEX_FILE="$idx" git diff --stat -- "${paths[@]}"
    echo "修法：跑 bash scripts/gen.sh，并把结果一起提交。"
    rm -f "$idx"; rmdir "$tmpdir" 2>/dev/null || true
    return 1
  fi
  rm -f "$idx"; rmdir "$tmpdir" 2>/dev/null || true
  # 生成物与契约一致，但若相对 HEAD 还有未提交改动，提醒一句 ——
  # 提交时落下它们的话，下一个人的干净检出里生成物就是旧的。
  if ! git diff --quiet -- "${paths[@]}"; then
    echo "  提示：生成物与契约一致，但相对 HEAD 有未提交改动 —— 提交时别落下它们。"
  fi
}
run "契约无漂移" check_drift

# ---- 4. Go 测试（条数下限 + SKIP 必须为 0）----
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

  # SKIP 必须恰好为 0。
  #
  # 走到这里说明 DATABASE_URL 已设，而 Redis 侧已改用 miniredis（不需要真机），
  # 所以**没有任何用例有正当理由被跳过**。以前 skip>0 只是打印一行提示，
  # 于是"静默跳过"能一路绿到底 —— 那正是守卫腐烂的起点。
  if [ "$skip" -gt 0 ]; then
    echo "  有 $skip 条被 skip，但此环境下不该有任何 skip："
    printf '%s\n' "$out" | grep -E '^(--- SKIP|    --- SKIP)' | sed 's/^/    /'
    echo "  （Redis 类用例应走 miniredis；PG 类用例在 DATABASE_URL 已设时不该跳过）"
    return 1
  fi

  if [ "$pass" -lt "$MIN_GO_TESTS" ]; then
    echo "  PASS 条数 $pass < 下限 $MIN_GO_TESTS：有一批用例没被执行。"
    echo "  常见原因：DATABASE_URL 指到了空库（迁移没跑）。"
    return 1
  fi
}
run "Go 测试（条数下限 $MIN_GO_TESTS，SKIP=0）" check_go_test

# ---- 5. 压测脚本自测 ----
step "压测比对逻辑单测"
run "compare.test.mjs" node --test tools/loadtest/scripts/compare.test.mjs

# ---- 6. 权限/审核/支付/安全守卫的牙齿 ----
step "变异验证（全部变异必须被指定测试抓住）"
check_guards() {
  local py
  if ! py="$(resolve_python)"; then
    echo "找不到 Python。设 PYTHON_BIN 指向解释器后重试。" >&2
    return 1
  fi
  "$py" -u scripts/verify-guards.py
}
run "verify-guards.py（变异全部被指定测试抓住）" check_guards

# ---- 7. 依赖漏洞 ----
#
# 为什么必须进 CI：依赖漏洞是**随时间变化**的。今天 0 条不代表下周 0 条 ——
# 新披露的 CVE 会追认已存在的依赖。手动跑等于"发布前想起才跑"。
step "依赖漏洞（govulncheck）"
check_vuln() {
  local govuln
  govuln="$(go env GOPATH)/bin/govulncheck.exe"
  [ -x "$govuln" ] || govuln="$(go env GOPATH)/bin/govulncheck"
  if [ ! -x "$govuln" ]; then
    echo "未找到 govulncheck。安装：go install golang.org/x/vuln/cmd/govulncheck@latest" >&2
    return 1
  fi

  local out code=0
  out="$(cd services/api && GOPROXY=https://goproxy.cn,direct "$govuln" ./... 2>&1)" || code=$?

  # govulncheck 的退出码：0 = 无漏洞，1 = 有漏洞，其它 = 工具/网络故障。
  #
  # "工具没跑起来"绝不能被当成"没有漏洞" —— 那是把门禁变成安慰剂。
  if [ "$code" -gt 1 ]; then
    printf '%s\n' "$out" | tail -20
    echo "govulncheck 运行失败（退出码 $code）：这不是「没有漏洞」，是「没检查」。" >&2
    return 1
  fi
  if [ "$code" -eq 0 ]; then
    echo "  0 条可达漏洞"
    # 模块级（不可达）的条数也报出来：它们现在不构成攻击面，
    # 但依赖一旦被别处引用就可能变成可达 —— 数字变化是需要留意的信号。
    printf '%s\n' "$out" \
      | grep -oE '[0-9]+ vulnerabilit[a-z]* in modules you require' \
      | sed 's/^/  模块级（不可达）：/' || true
    return 0
  fi

  # 退出码 1：有漏洞。只有**可达**的才阻断。
  #
  # 模块级不可达漏洞不构成攻击面（受影响函数不在调用图里），但必须打出来 ——
  # 依赖一旦被别处引用就可能变成可达。
  if printf '%s\n' "$out" | grep -q 'Example traces found'; then
    printf '%s\n' "$out" | head -60
    echo "  存在**可达**漏洞 —— 必须处理（升级依赖，或证明该调用路径不可达）。" >&2
    return 1
  fi
  local total
  total="$(printf '%s\n' "$out" | grep -cE '^Vulnerability #' || true)"
  echo "  $total 条模块级漏洞，均不可达（暂不构成攻击面，请留意）"
  printf '%s\n' "$out" | grep -E '^Vulnerability #' | head -20 | sed 's/^/    /'
  return 0
}
run "govulncheck（可达漏洞一律红）" check_vuln

# ---- 8. 性能门禁的牙齿 ----
step "压测门禁变异验证"
if [ "$QUICK" = "1" ]; then
  echo "  $QUICK_SKIP_REASON"
else
  run "verify-perf-gate.sh" bash scripts/verify-perf-gate.sh
fi

# ---- 9. 活服务器攻击面探针 ----
#
# 与 go test 的分工：go test 跑在进程内，永远走不到中间件链；本脚本用裸 HTTP
# 打活服务器，能测到**中间件顺序、响应头、状态码、错误文案**这些只有真实
# HTTP 栈才暴露的东西（典型：CORS 排在 authGate 之后时 curl 全绿、浏览器全炸）。
step "安全探针（需 :8080 API + 已 seed 的 PG/Redis）"

# 夹具变量在这里显式给出，并且在下面断言 SKIP=0。
#
# 为什么两件都要做：跨租户那 6 条 check 在缺夹具时会 skip()，而 skip() 只
# nSkip++ —— 探针退出码只看 FAIL。于是"夹具变量忘了传"表现为**一路绿着、
# 那 6 条从没执行过**。这与 Go 侧刚修掉的「t.Skip 与真通过在退出码上无法区分」
# 是同一个毛病换了一层皮，所以这里用同一个判据（SKIP 必须为 0）。
#
# 2 号商户由 tools/loadtest/seed/seed.sql 建立（此前放在不入库的 .workbuddy/ 下，
# 这正是它长期没被执行的原因）。密码不用传：探针复用 1 号商户的密码。
PROBE_TENANT_B_PHONE="${PROBE_TENANT_B_PHONE:-13800000001}"
PROBE_TENANT_A_PRODUCT_ID="${PROBE_TENANT_A_PRODUCT_ID:-aaaaaaaa-0000-4000-8000-000000000001}"
export PROBE_TENANT_B_PHONE PROBE_TENANT_A_PRODUCT_ID

check_probe() {
  local out
  if ! out="$(node scripts/security-probe.mjs 2>&1)"; then
    printf '%s\n' "$out" | tail -40
    return 1
  fi

  local skip
  skip="$(printf '%s\n' "$out" | sed -n 's/.*SKIP=\([0-9]\{1,\}\).*/\1/p' | tail -1)"
  printf '%s\n' "$out" | grep -E '^结果' | sed 's/^/  /' || true

  if [ -z "$skip" ]; then
    echo "解析不出探针的 SKIP 计数 —— 汇总行的格式变了？原始输出末尾：" >&2
    printf '%s\n' "$out" | tail -20
    return 1
  fi
  if [ "$skip" -gt 0 ]; then
    echo "  有 $skip 条探针用例被跳过（= 从未执行），但此环境应全部可执行：" >&2
    printf '%s\n' "$out" | grep -E '^  SKIP' | sed 's/^/    /' || true
    echo "  （跨租户组缺夹具时就会这样。夹具见 tools/loadtest/seed/seed.sql）" >&2
    return 1
  fi
}
if [ "$QUICK" = "1" ]; then
  echo "  $QUICK_SKIP_REASON"
else
  run "security-probe.mjs（SKIP=0）" check_probe
fi

# ---- 10. 端到端验收 ----
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
# 只报**实际执行**的项数。--quick 下那几项没跑，就不该出现在"全部通过"里 ——
# 一个把覆盖率说大了的汇总，比没有汇总更容易让人放心地漏掉东西。
if [ "$RAN" -lt "$TOTAL_STEPS" ]; then
  printf '\033[33m（--quick：%d 项未执行，共 %d 项）\033[0m\n' "$((TOTAL_STEPS - RAN))" "$TOTAL_STEPS"
fi
if [ ${#FAILED[@]} -eq 0 ]; then
  printf '\033[32m已执行的 %d 项全部通过。\033[0m\n' "$RAN"
  exit 0
fi

printf '\033[31m%d 项失败（共执行 %d 项）：\033[0m\n' "${#FAILED[@]}" "$RAN"
for f in "${FAILED[@]}"; do
  printf '  - %s\n' "$f"
done
exit 1
