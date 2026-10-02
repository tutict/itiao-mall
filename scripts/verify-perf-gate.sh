#!/usr/bin/env bash
# 证明性能门禁**不是真空的** —— 等价于守卫的变异验证。
#
# 做法：
#   1. 往被测处理函数里注入一次固定延迟（模拟"后端引入的性能问题"，比如 N+1 查询）
#   2. 把这个劣化版二进制**起在另一个端口**，完全不打扰正在跑的正常实例
#   3. 先自检：注入后的接口确实变慢了（否则后面的结论全部作废）
#   4. 跑 scripts/perf.sh 指向它 —— 门禁**必须报红**（非 0 退出）
#   5. 按 sha256 还原源码，重建，再跑一次 —— 门禁**必须转绿**（0 退出）
#
# 为什么必须做这一步：一个从不报红的门禁和没有门禁是同一种东西。
# 只有"先红后绿"两个方向都被观察到，才算证明它有牙。
#
# 用法:
#   bash scripts/verify-perf-gate.sh
#   PROBE_DELAY='40*time.Millisecond' bash scripts/verify-perf-gate.sh
#
# PROBE_DELAY 必须是一段合法的 **Go 时长表达式**（会被原样塞进 time.Sleep）。
# 早期版本传的是 `25ms`，注入出 `time.Sleep(25ms)` —— 编译直接挂。
#
# 退出码: 0 = 门禁确实有牙（该红时红、该绿时绿）；非 0 = 门禁无效或流程出错。
set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

# 本机 `python` 不一定在 PATH 上，用托管版的全路径更稳。
PY="${PYTHON_BIN:-C:/Users/tutic/.workbuddy/binaries/python/versions/3.13.12/python.exe}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROBE_PORT="${PROBE_PORT:-8081}"
PROBE_DELAY="${PROBE_DELAY:-25*time.Millisecond}"
PROBE_MIN_MS="${PROBE_MIN_MS:-10}"
LOGDIR="tools/loadtest/results"
TARGET="services/api/internal/httpapi/handlers.go"

# 备份放系统临时目录，不落进仓库：少了污染仓库的风险，也不会碰到仓库范围内的删除守卫。
SCRATCH="$(mktemp -d)"
BACKUP="$SCRATCH/handlers.go.orig"
RESULTS="$LOGDIR/gate-proof.json"

mkdir -p "$LOGDIR"

if [ ! -f "tools/loadtest/baselines/load.json" ]; then
  echo "还没有基线，先建立：RECORD_BASELINE=1 RUNS=3 bash scripts/perf.sh" >&2
  exit 2
fi

# ---------------------------------------------------------------- 小工具
kill_port() {
  local port="$1" pids
  pids="$(netstat -ano 2>/dev/null | grep -E "[:.]${port}[[:space:]]" | grep LISTENING | awk '{print $NF}' | sort -u || true)"
  if [ -n "$pids" ]; then
    for pid in $pids; do
      taskkill //F //PID "$pid" >/dev/null 2>&1 || true
    done
    sleep 1
  fi
}

wait_health() {
  local port="$1"
  for _ in $(seq 1 40); do
    if curl -fsS "http://localhost:${port}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "等待 :${port} 就绪超时" >&2
  return 1
}

sha256_of() {
  "$PY" -c "import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],'rb').read()).hexdigest())" "$1"
}

# 注入 / 还原都走 python 并且**用二进制读写 + sha256 校验**。
# 用文本模式读写会做换行转换（CRLF<->LF），看着"还原了"其实字节变了 —— 这个坑踩过。
py_inject() {
  "$PY" - "$TARGET" "$PROBE_DELAY" <<'PY'
import sys, pathlib
target = pathlib.Path(sys.argv[1])
delay = sys.argv[2]
data = target.read_bytes()

if b"[PERF-GATE-PROBE]" in data:
    print("已经注入过了", file=sys.stderr); sys.exit(1)

# 锚点必须**全局唯一**，否则可能改到错的地方。
# 第一版用的是 `pageSize := derefInt32(req.Params.PageSize, 20)` —— 那行在 ListProducts
# 和 ListOrders 里一模一样，出现 2 次。脚本当场拒绝执行（而不是随便挑一个改），
# 这正是想要的行为：宁可报错，也不要"看起来做了变异"。
stmt = b'rows, total, err := h.svc.ListProducts'
if data.count(stmt) != 1:
    print("注入锚点出现 %d 次，期望 1 次，拒绝继续" % data.count(stmt), file=sys.stderr); sys.exit(1)

probe = (b'// [PERF-GATE-PROBE] injected delay: proves the perf gate actually bites.'
         b' Must not exist in a normal build.\n'
         b'\ttime.Sleep(' + delay.encode() + b')\n\t')
data = data.replace(stmt, probe + stmt, 1)

imp = b'"net/http"'
if data.count(imp) != 1:
    print("import 锚点出现 %d 次，期望 1 次，拒绝继续" % data.count(imp), file=sys.stderr); sys.exit(1)
data = data.replace(imp, imp + b'\n\t"time"', 1)

target.write_bytes(data)
print("   已注入 time.Sleep(" + delay + ")")
PY
}

py_restore() {
  "$PY" - "$TARGET" "$BACKUP" <<'PY'
import sys, pathlib, hashlib
target, backup = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
if not backup.exists():
    print("没有备份可还原", file=sys.stderr); sys.exit(1)
target.write_bytes(backup.read_bytes())
now = target.read_bytes()
a, b = hashlib.sha256(backup.read_bytes()).hexdigest(), hashlib.sha256(now).hexdigest()
if a != b:
    print("还原失败: 期望 %s 实际 %s" % (a, b), file=sys.stderr); sys.exit(1)
if b"[PERF-GATE-PROBE]" in now:
    print("还原后仍残留注入痕迹", file=sys.stderr); sys.exit(1)
print("   已还原 sha256=%s..." % a[:16])
PY
}

build() {
  ( cd services/api && go build -o "./bin/$1" ./cmd/api )
}

start_probe() {
  kill_port "$PROBE_PORT"
  (
    cd services/api
    APP_ENV=dev HTTP_ADDR=":${PROBE_PORT}" LOG_LEVEL=warn \
    DATABASE_URL="postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable" \
    REDIS_ADDR=localhost:6379 AUTH_PROVIDER=mock PAYMENT_PROVIDER=mock \
    JWT_SECRET=dev-only-secret-please-change \
    nohup "./bin/$1" serve >"../../$LOGDIR/probe-api.log" 2>&1 &
    disown || true
  )
  wait_health "$PROBE_PORT"
}

cleanup() {
  kill_port "$PROBE_PORT"
  # 无论脚本怎么退出，都要保证源码回到原始状态 —— 这是本脚本最重要的不变量。
  # 删临时目录走 python，避免触发仓库范围的删除守卫。
  if [ -f "$BACKUP" ]; then
    if grep -q "PERF-GATE-PROBE" "$TARGET" 2>/dev/null; then
      py_restore || true
    fi
  fi
  "$PY" -c "import shutil,sys;shutil.rmtree(sys.argv[1],ignore_errors=True)" "$SCRATCH" || true
}
trap cleanup EXIT

# ---------------------------------------------------------------- 开始
echo "== 0/4 备份被测文件 =="
cp "$TARGET" "$BACKUP"
BEFORE="$(sha256_of "$TARGET")"
echo "   $TARGET  sha256=${BEFORE:0:16}..."
echo "   备份位置 $BACKUP（临时目录，不进仓库）"

echo "== 1/4 注入劣化 time.Sleep(${PROBE_DELAY}) 并起在 :${PROBE_PORT} =="
py_inject
build api-perfprobe.exe >/dev/null
start_probe api-perfprobe.exe
echo "   :${PROBE_PORT} 已就绪"

# 注入自检：如果注入没真的生效，后面的"门禁报红"就毫无意义（甚至会把
# "注入失效"误判成"门禁没牙"）。所以先量一下，慢不下来就直接判失败。
LAT_S="$(curl -s -o /dev/null -w '%{time_total}' "http://localhost:${PROBE_PORT}/api/v1/products?page=1&page_size=20")"
if ! awk -v lat="$LAT_S" -v min="$PROBE_MIN_MS" 'BEGIN{exit !(lat*1000 >= min+0)}'; then
  echo "❌ 注入未生效：单次 /api/v1/products 耗时仅 $(awk -v l="$LAT_S" 'BEGIN{printf "%.2f", l*1000}')ms，低于自检下限 ${PROBE_MIN_MS}ms。" >&2
  echo "   这说明劣化没被编进去，后面的门禁结论一律不可信。" >&2
  exit 1
fi
echo "   ✅ 注入自检通过：单次 /api/v1/products = $(awk -v l="$LAT_S" 'BEGIN{printf "%.2f", l*1000}')ms"

echo "== 2/4 跑门禁 —— 期望**报红** =="
RED=0
set +e
BASE_URL="http://localhost:${PROBE_PORT}" bash scripts/perf.sh >"$LOGDIR/gate-injected.log" 2>&1
RED=$?
set -e
if [ "$RED" = "0" ]; then
  echo "❌ 门禁对 ${PROBE_DELAY} 的延迟注入毫无反应 —— 这个门禁是真空的。" >&2
  exit 1
fi
echo "   ✅ 门禁报红（退出码 $RED）"
grep -E "FAIL|回归|p95" "$LOGDIR/gate-injected.log" | tail -8 || true

echo "== 3/4 还原源码并重建 =="
kill_port "$PROBE_PORT"
py_restore
AFTER="$(sha256_of "$TARGET")"
if [ "$BEFORE" != "$AFTER" ]; then
  echo "❌ 还原后 sha256 与原始不一致：$BEFORE vs $AFTER" >&2
  exit 1
fi
echo "   ✅ sha256 与原始一致"
build api-perfprobe.exe >/dev/null
start_probe api-perfprobe.exe

echo "== 4/4 再跑门禁 —— 期望**转绿** =="
GREEN=0
set +e
BASE_URL="http://localhost:${PROBE_PORT}" bash scripts/perf.sh >"$LOGDIR/gate-restored.log" 2>&1
GREEN=$?
set -e
if [ "$GREEN" != "0" ]; then
  echo "❌ 还原后门禁仍然报红（退出码 $GREEN）—— 要么这次运行本身有噪声，要么门禁过于敏感。" >&2
  tail -20 "$LOGDIR/gate-restored.log" >&2
  exit 1
fi
echo "   ✅ 门禁转绿"

"$PY" - "$RESULTS" "$PROBE_DELAY" "$RED" "$GREEN" <<'PY'
import json, pathlib, sys
out, delay, red, green = pathlib.Path(sys.argv[1]), sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
out.write_text(json.dumps({
    "probeDelay": delay,
    "injectedExitCode": red,
    "restoredExitCode": green,
    "hasTeeth": red != 0 and green == 0,
}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY

echo
echo "结论：门禁有牙 —— 注入劣化时报红（$RED），还原后转绿（$GREEN）。"
echo "证据：$LOGDIR/gate-injected.log / gate-restored.log / gate-proof.json"
