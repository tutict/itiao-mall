#!/usr/bin/env bash
# 压测期间抓一份 CPU profile，把"每请求 100µs 花在哪"拆开看。
#
# 为什么需要这个
# --------------
# 容量探底（见 docs/capacity-probe.md）已经定位到：闭环读接口饱和在
# ~10,000 req/s，即每请求约 **100µs** 的独占资源时间。
# 已排除的：连接池大小（MaxConns 20→100 吞吐零变化）、PG 饱和
# （pg_stat_activity 的 active 始终 1~4）、压测机（404 对照 11.7 万 req/s）。
#
# 也就是说：**有一个我们还不知道名字的单点在消耗那 100µs**。
# 没有 pprof 就没有任何手段回答"是哪一段"，而这时候动手优化 = 盲改 ——
# 可能改到占大头的地方，也可能什么都改不到。
#
# 用法
# ----
#   bash scripts/profile-api.sh                       # 默认 300 VU / 30 秒
#   VUS=600 SECONDS=60 bash scripts/profile-api.sh     # 更狠一点
#   PPROF_ADDR=http://127.0.0.1:6060 bash scripts/profile-api.sh
#
# 环境
# ----
#   VUS             压测并发。默认 300（探底实测的饱和区中段）
#   SECONDS         CPU 采样窗口。默认 30。**必须 ≤ PPROF_MAX_SECONDS**（默认 60）
#   PPROF_ADDR      剖析端口。被测 API 必须带 PPROF_ADDR 起，见下方"前置"
#   API_PORT        被测 API 端口，默认 8080
#   OUT_DIR         产物目录，默认 tools/loadtest/results
#
# 前置：被测 API 必须开着剖析端口
# ----------------------------------
#   PPROF_ADDR=127.0.0.1:6060 \
#   RATE_LIMIT_MAX=1000000 RATE_LIMIT_AUTH_MAX=1000000 \
#   bash scripts/api.sh
#
# 限流必须放宽，否则 profile 里全是 Redis 往返，而那测的是限流器不是系统。
#
# 退出码：0 = 抓到并解析成功；2 = 环境不可用（端口没开 / API 不在 / 抓取失败）

set -uo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VUS="${VUS:-300}"
SECONDS_PROF="${SECONDS:-30}"
API_PORT="${API_PORT:-8080}"
BASE_URL="${BASE_URL:-http://localhost:${API_PORT}}"
PPROF_ADDR="${PPROF_ADDR:-http://127.0.0.1:6060}"
OUT_DIR="${OUT_DIR:-tools/loadtest/results}"

mkdir -p "$OUT_DIR"
PROFILE="$OUT_DIR/cpu-$(date +%Y%m%d-%H%M%S).pb.gz"

# ---------------------------------------------------------------- 环境自检
echo "== 1/4 环境自检 =="

if ! command -v k6 >/dev/null 2>&1; then
  echo "   找不到 k6。安装见 k6.io" >&2
  exit 2
fi
# go tool pprof 是解读 profile 的唯一途径。缺了它抓到也是一堆二进制。
if ! command -v go >/dev/null 2>&1; then
  echo "   找不到 go（需要 go tool pprof 解读 profile）" >&2
  exit 2
fi

if ! curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "   API 未在 $BASE_URL 响应。先起：bash scripts/dev-up.sh" >&2
  exit 2
fi
echo "   API=$BASE_URL 就绪"

# 剖析端口必须可达。这里不静默跳过 ——
# 一个"端口没开所以没抓到 profile"的脚本，会让人得出"没有热点"的结论。
if ! curl -fsS -m 5 "$PPROF_ADDR/debug/pprof/heap?debug=1" >/dev/null 2>&1; then
  echo "   剖析端点 $PPROF_ADDR 不可达。" >&2
  echo "   被测 API 必须带 PPROF_ADDR 启动，例如：" >&2
  echo "     PPROF_ADDR=127.0.0.1:6060 bash scripts/api.sh" >&2
  echo "   注意 PPROF_ADDR 是**给 API 进程**的环境变量，不是本脚本的。" >&2
  exit 2
fi

# 提前探一次上限：SECONDS 超了会被剖析端点以 400 拒绝，
# 而那发生在压测跑了 30 秒之后 —— 白跑一轮。
head -c 400 "$PPROF_ADDR/debug/pprof/profile?seconds=$SECONDS_PROF" >/dev/null 2>&1 || true
echo "   剖析端点=$PPROF_ADDR  采样=${SECONDS_PROF}s  压测=${VUS} VU"

# CPU profile 全进程唯一：正在采的时候第二个请求会失败。
# 先确认没有残留的采集在跑。
if curl -fsS -m 2 "$PPROF_ADDR/debug/pprof/profile?seconds=1" >/dev/null 2>&1; then
  echo "   可以开始采集（1 秒试采成功）"
else
  # 这里不当作错误：Go 在"已有采集在跑"时回 500，
  # 而 1 秒窗口偶尔会因负载而超时。真正要抓的那次会自己判断成败。
  echo "   试采未成功，继续（真正抓取时会判定成败）"
fi

# ---------------------------------------------------------------- 起压测
echo "== 2/4 起压测 =="

# 清限流计数：不清的话 profile 里可能混进 429 路径，而那是限流器不是系统。
if [ "${SKIP_RESET:-0}" != "1" ]; then
  keys="$(docker exec -i itiao-redis redis-cli --scan --pattern 'rl:*' 2>/dev/null | tr -d '\r')"
  if [ -n "$keys" ]; then
    printf '%s\n' "$keys" | xargs docker exec -i itiao-redis redis-cli DEL >/dev/null 2>&1 || true
    echo "   已清限流计数键 $(printf '%s\n' "$keys" | wc -l) 个"
  fi
fi

# 压测跑得比采样窗口略长一些：采集在窗口开始后立刻生效，
# 但前几百毫秒是爬坡期，那段数据不该混进 profile。
RAMP=5
STAGE=$(( SECONDS_PROF + 15 ))

STEPS="$VUS" STAGE_SECONDS="$STAGE" RAMP_SECONDS="$RAMP" \
BASE_URL="$BASE_URL" k6 run \
  --summary-trend-stats="avg,min,med,max,p(90),p(95),p(99)" \
  tools/loadtest/scenarios/probe.js > "$OUT_DIR/profile-load.log" 2>&1 &
K6_PID=$!

# 抓 profile 放在后台，与压测并发。
# 顺序很重要：必须**先**让 k6 起起来再开始采集，
# 否则采到的是空载期的数据 —— 那份 profile 的热点全是 GC 与后台任务，
# 与"每请求 100µs"无关。一个看起来合理但答非所问的 profile 比没有更坏。
sleep 3
echo "   抓取 ${SECONDS_PROF}s CPU profile → $PROFILE"
curl -fsS --max-time $(( SECONDS_PROF + 20 )) \
  "$PPROF_ADDR/debug/pprof/profile?seconds=$SECONDS_PROF" -o "$PROFILE"
CURL_RC=$?

# 顺手抓一份 goroutine：看有没有请求被卡在同一个地方（队列的证据）。
GOROUTINE="$OUT_DIR/goroutine-$(date +%Y%m%d-%H%M%S).txt"
curl -fsS -m 10 "$PPROF_ADDR/debug/pprof/goroutine?debug=2" -o "$GOROUTINE" 2>/dev/null || true

wait $K6_PID
K6_RC=$?

echo "== 3/4 校验产物 =="

if [ $CURL_RC -ne 0 ] || [ ! -s "$PROFILE" ]; then
  echo "   ✗ 没有抓到 profile（curl 退出码 $CURL_RC）。" >&2
  echo "     常见原因：seconds 超过 PPROF_MAX_SECONDS（默认 60）被 400 拒绝；" >&2
  echo "     或已有采集在进行（CPU profile 全进程唯一）。" >&2
  echo "     API 日志里应该有对应的 400/500 记录。" >&2
  exit 2
fi

# 大小下限：空 profile 也有几十字节，这个下限只挡"抓了个空文件"。
PROFILE_SIZE=$(wc -c < "$PROFILE" | tr -d ' ')
if [ "$PROFILE_SIZE" -lt 512 ]; then
  echo "   ✗ profile 只有 ${PROFILE_SIZE} 字节，太小了 —— 多半是错误响应体。" >&2
  head -c 200 "$PROFILE" >&2
  echo >&2
  exit 2
fi
echo "   profile = ${PROFILE_SIZE} 字节"
echo "   压测退出码 = $K6_RC（探底里非 0 不一定是失败，看日志）"

# ---------------------------------------------------------------- 解读
echo "== 4/4 解读 =="

# -top 按 flat（自身耗时）排序，回答"哪个函数最花 CPU"。
# 刻意用 flat 而不是 cum：cum 会把中间件链整条算进去，
# 而 flat 才能指出"到底哪一行代码在烧 CPU"。
go tool pprof -top -nodecount=25 "$PROFILE" 2>/dev/null || {
  echo "   go tool pprof 解析失败 —— 文件可能不是有效的 pprof 二进制" >&2
  exit 2
}

echo
echo "---- 累计视角（含调用链，看请求从入口到出口都经过谁）----"
go tool pprof -top -cum -nodecount=25 "$PROFILE" 2>/dev/null | tail -30

echo
echo "产物："
echo "  $PROFILE            CPU profile（用 go tool pprof -http $PROFILE 看火焰图）"
if [ -s "$GOROUTINE" ]; then
  echo "  $GOROUTINE     goroutine 全栈"
  echo
  echo "---- goroutine 归类（按阻塞点前 3 行聚合，看请求堆在哪个函数上）----"
  grep -E "^#|\.go:[0-9]+" "$GOROUTINE" 2>/dev/null | head -40
fi
echo "  $OUT_DIR/profile-load.log    k6 压测日志"

cat <<'EOF'

怎么读这份 profile
------------------
1. 先看 flat top 里 **net/http** 之外的项。HTTP 栈的开销是固定的，
   它高不代表有问题 —— 要看的是**谁在 HTTP 栈之下**。
2. 重点怀疑三个（本项目的具体候选）：
   - encoding/json：oapi-codegen 的编解码 + 我们的响应组装
   - Redis 往返：限流器的 Lua 脚本每次请求都要跑一次
   - 日志写盘：AccessLog 每个请求都写一行
3. 如果 flat top 里 pgx/pgxpool 占大头，那是 DB 侧；
   如果 goroutine 大量停在 chan/select，那是限流器的 Redis 往返在等。
4. **不要**拿这份 profile 去论证"该上分库分表"——
   10k req/s 的量级下分库是零收益，先找到那 100µs 的归属。
EOF