#!/usr/bin/env bash
# 阶梯压测探底：把系统一档一档往上推，找出"错误率抬头 / p99 爆炸"的那个档。
#
# 为什么要单独写一个探底脚本（而不是把 load.js 的 VU 调大）
# ------------------------------------------------------
# load.js 的 browse() 每个迭代末尾 `sleep(0.3)`，而基线只跑 50 VU，
# 于是它能产生的最大吞吐 ≈ 50/0.31 ≈ 161 req/s —— 实测基线 173.7 req/s
# 与这个理论值同量级。**那个数字测的是压测脚本的节流阀，不是系统容量。**
# 见 tools/loadtest/scenarios/probe.js 的说明。
#
# 所以探底用 probe.js：闭环无 sleep，每档独立，k6 侧不停。
#
# 用法
# ----
#   bash scripts/probe-capacity.sh                       # 默认阶梯
#   STEPS=50,100,200 bash scripts/probe-capacity.sh      # 自定义阶梯
#   STAGE_SECONDS=45 bash scripts/probe-capacity.sh      # 每档跑久一点（更稳）
#   BASE_URL=http://localhost:8092 bash scripts/probe-capacity.sh
#
# 环境
# ----
#   STEPS           逗号分隔的 VU 档位。默认 50,150,300,600,1000
#   STAGE_SECONDS   每档的稳态时长（秒）。默认 30
#   RAMP_SECONDS    每档的爬升时长（秒）。默认 10
#   PER_STEP=1      每档独立跑一次 k6（默认）。设为 0 则一次性连续推
#                   （更快，但无法定位"拐点发生在哪一档"）
#   API_PORT        被测 API 端口，默认 8080
#
# 输出
# ----
#   tools/loadtest/results/probe-step-<vus>.json   每档原始结果
#   tools/loadtest/results/probe-summary.json     汇总（供 compare/成图用）
#
# 退出码：0 = 全部档位都跑完（**不代表系统没崩**，看汇总里的 failed 列）；
#        2 = 阶梯本身没跑起来（环境问题）。

set -uo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STEPS="${STEPS:-50,150,300,600,1000}"
STAGE_SECONDS="${STAGE_SECONDS:-30}"
RAMP_SECONDS="${RAMP_SECONDS:-10}"
PER_STEP="${PER_STEP:-1}"
API_PORT="${API_PORT:-8080}"
BASE_URL="${BASE_URL:-http://localhost:${API_PORT}}"
RESULTS_DIR="tools/loadtest/results"

# 尾延迟正是抓"后端引入问题"最灵的指标，而 k6 默认统计集**不含 p(99)**。
# 不显式声明，汇总里那列会是 undefined（实测）。
TREND_STATS="avg,min,med,max,p(90),p(95),p(99)"

mkdir -p "$RESULTS_DIR"

# ---------------------------------------------------------------- 环境自检
#
# 为什么要分三段报错：探底跑 5 分钟，失败在最后一步才显现的话，
# 那 5 分钟全白费 —— 而失败原因通常是下面三条之一。
echo "== 1/4 环境自检 =="

if ! command -v k6 >/dev/null 2>&1; then
  echo "   找不到 k6。安装：brew install k6 / 见 k6.io" >&2
  exit 2
fi

# 探底要求限流放宽 —— 否则测的是限流器。
#
# 为什么这条最容易踩：未登录的商品读取按 **IP** 建桶（rl:dev:read:product.read:127.0.0.1），
# 默认 120 次/分钟。50 VU 就能一分钟打出几千次读，于是实测 ≈98% 的请求吃 429、
# http_req_failed 冲到 95% —— 那条曲线看起来像"系统一压就崩"，
# 实际是限流器在工作。**这正是探底最容易得出错误结论的地方**，
# 所以它排第一，且必须先确认。
RATE_KEYS="$(docker exec -i itiao-redis redis-cli --scan --pattern 'rl:*' 2>/dev/null | tr -d '\r' | wc -l | tr -d ' ')"
if [ -n "${RATE_KEYS:-}" ] && [ "$RATE_KEYS" != "0" ]; then
  echo "   注意：Redis 里有 $RATE_KEYS 个限流计数键。探底前会清掉 ——"
  echo "   但如果被测 API 的限流阈值仍是默认的 120/min，读接口会立刻 429。"
  echo "   那样的曲线不是系统容量，是限流器。请用放宽限流的实例："
  echo "   RATE_LIMIT_MAX=1000000 RATE_LIMIT_AUTH_MAX=1000000 起 API"
fi

if ! curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "   API 未在 $BASE_URL 响应。" >&2
  echo "   用 dev-up.sh 起：bash scripts/dev-up.sh" >&2
  exit 2
fi
echo "   API=$BASE_URL 就绪"

# 清限流计数：让第一档与后面几档站在同一起跑线上。
#
# 不清的后果（实测）：键里累计着上一档的次数，到后面某档突然开始 429，
# 于是"拐点"出现在一个纯属人为的数字上。这个坑比看不出拐点更坏 ——
# 它给出一个**指向错误方向**的结论。
if [ "${SKIP_RESET:-0}" != "1" ]; then
  keys="$(docker exec -i itiao-redis redis-cli --scan --pattern 'rl:*' 2>/dev/null | tr -d '\r')"
  if [ -n "$keys" ]; then
    printf '%s\n' "$keys" | xargs docker exec -i itiao-redis redis-cli DEL >/dev/null
    echo "   已清掉 $(printf '%s\n' "$keys" | wc -l) 个限流计数键"
  fi
fi

# ---------------------------------------------------------------- 逐档跑
echo "== 2/4 阶梯压测 =="
echo "   阶梯=$STEPS  每档稳态=${STAGE_SECONDS}s  爬升=${RAMP_SECONDS}s"

IFS=',' read -r -a STEP_ARRAY <<< "$STEPS"
SUMMARY="$RESULTS_DIR/probe-summary.json"
: > "$RESULTS_DIR/probe-summary.ndjson"

for vus in "${STEP_ARRAY[@]}"; do
  vus="$(echo "$vus" | tr -d ' ')"
  [ -z "$vus" ] && continue
  printf '   --- 档位 %s VU ---\n' "$vus"

  # 每档之前清限流：档位之间也要同一起跑线（同上）。
  if [ "${SKIP_RESET:-0}" != "1" ]; then
    keys="$(docker exec -i itiao-redis redis-cli --scan --pattern 'rl:*' 2>/dev/null | tr -d '\r')"
    [ -n "$keys" ] && printf '%s\n' "$keys" | xargs docker exec -i itiao-redis redis-cli DEL >/dev/null
  fi

  STEPS="$vus" STAGE_SECONDS="$STAGE_SECONDS" RAMP_SECONDS="$RAMP_SECONDS" \
  BASE_URL="$BASE_URL" k6 run \
    --summary-trend-stats="$TREND_STATS" \
    --summary-export="$RESULTS_DIR/probe-step-${vus}.json" \
    tools/loadtest/scenarios/probe.js >/dev/null 2>&1
  rc=$?

  f="$RESULTS_DIR/probe-step-${vus}.json"
  if [ ! -f "$f" ]; then
    echo "      ✗ 这一档没有产出结果（k6 退出码 $rc）"
    continue
  fi
  # k6 退出码非 0 在探底里**不一定是失败**：错误率越线会让它红，
  # 而那正是我们要观察的现象。判据交给下面的读取器。
  node tools/loadtest/scripts/probe-read.mjs --in "$f" --vus "$vus" >> "$RESULTS_DIR/probe-summary.ndjson"
done

# ---------------------------------------------------------------- 汇总
echo "== 3/4 汇总 =="
if [ ! -s "$RESULTS_DIR/probe-summary.ndjson" ]; then
  echo "   没有任何一档产出可读结果。" >&2
  echo "   常见原因：k6 没跑起来、被测 API 限流（见上方提示）。" >&2
  exit 2
fi

node tools/loadtest/scripts/probe-read.mjs --report "$RESULTS_DIR/probe-summary.ndjson" --out "$SUMMARY"

# ---------------------------------------------------------------- 结论提示
echo "== 4/4 结论 =="
node -e '
const fs = require("node:fs");
const rows = fs.readFileSync(process.argv[1], "utf8").trim().split("\n").map(JSON.parse);
const bad = rows.filter(r => !r.hasData);
const ok = rows.filter(r => r.hasData);
if (bad.length) {
  console.log(`   注意：${bad.length} 档数据不完整，未参与拐点判断：`);
  for (const b of bad) console.log(`     ${b.vus} VU 缺 ${(b.missing||[]).join("/") || b.error || "?"}`);
}
if (!ok.length) { console.log("   没有完整档位，无法判断拐点"); process.exit(0); }
const knee = ok.find(r => r.failed > 0.01);
if (!knee) {
  const top = ok[ok.length - 1];
  console.log(`   最高档 ${top.vus} VU 错误率仍为 ${(top.failed*100).toFixed(2)}% —— 没探到天花板`);
  console.log(`   本次可测到的上限 ≈ ${top.rate.toFixed(0)} req/s`);
  console.log("   ⚠️ 这**不代表系统的上限**。已验证（2026-10-11）：同机 k6 打 404 空响应");
  console.log("      能到 11.7 万 req/s，所以 k6 不是瓶颈；这个数是应用侧的。");
  console.log("      要往上探需要先排除本机回环的 TCP backlog（见 transport_error 档）。");
} else {
  console.log(`   拐点在 ${knee.vus} VU：错误率 ${(knee.failed*100).toFixed(2)}%，吞吐 ${knee.rate.toFixed(0)} req/s，p99 ${knee.p99.toFixed(1)}ms`);
  const prev = ok[ok.indexOf(knee) - 1];
  if (prev) console.log(`   上一档 ${prev.vus} VU 干净（错误率 ${(prev.failed*100).toFixed(2)}%）→ 系统真实容量落在 ${prev.vus}~${knee.vus} VU 之间`);

  const sc = knee.statusCounts || {};
  const nT = sc.transport_error || 0;
  const n4 = sc["4xx"] || 0;
  const n5 = sc["5xx"] || 0;

  if (nT > 0 && nT >= n4 + n5) {
    // 这是最容易得出反向结论的一支，必须单独说。
    console.log(`   ⚠️ 失败里有 ${nT} 次是**传输层错误**，而 4xx=${n4} / 5xx=${n5}。`);
    console.log("      原文实测为 `actively refused` —— 内核 TCP 监听队列打满后直接 RST，");
    console.log("      应用一个错误码都没返回（说明它还在正常干活，只是连接进不来）。");
    console.log("      **这不是应用容量到顶**，继续加 VU 只会让这个比例更高、测不到真拐点。");
    console.log("      处置：降并发档位 / 加 think time / 上独立压测机，别改应用代码。");
  } else if (n4 > 0 && n4 > n5) {
    console.log(`   ⚠️ 失败以 4xx 为主（${n4} 次，5xx ${n5} 次）—— 多半是限流器在工作，不是系统崩。放宽限流再测。`);
  } else if (n5 > 0) {
    console.log(`   ⚠️ 5xx 有 ${n5} 次 —— 这次是真崩（不是限流），去看 API 日志。`);
  }
}
' "$RESULTS_DIR/probe-summary.ndjson"
