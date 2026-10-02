#!/usr/bin/env bash
# k6 性能门禁：一条命令跑完「确认依赖 → 重置数据 → 冒烟 → 负载 → 基线比对」。
#
# 用法:
#   bash scripts/perf.sh                 # 跑 load 并与基线比对
#   SCENARIO=oversell bash scripts/perf.sh   # 跑并发超卖验证
#   RECORD_BASELINE=1 RUNS=3 bash scripts/perf.sh   # 跑 3 次取中位数写入基线
#   RESET=0 bash scripts/perf.sh         # 跳过数据重置（调试时省时间）
#
# 退出码即结论：0 = 未检测到回归，非 0 = 有回归或指标缺失。
# 已由 scripts/check-all.sh 收口；将来接 CI 时可直接复用这个退出码。
set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SCENARIO="${SCENARIO:-load}"
RESET="${RESET:-1}"
RECORD_BASELINE="${RECORD_BASELINE:-0}"
RUNS="${RUNS:-3}"
BASE_URL="${BASE_URL:-http://localhost:8080}"
RESULTS_DIR="tools/loadtest/results"
BASELINE="tools/loadtest/baselines/${SCENARIO}.json"

# ★ 实测：k6 --summary-export 的 trend 统计集默认只有 avg,min,med,max,p(90),p(95)，
#   **p(99) 不在其中**。不显式声明的话，compare.mjs / record-baseline.mjs 读 p(99)
#   会拿到 undefined —— 而它们对读不到值一律判失败，于是门禁直接报"指标缺失"。
#   所以这里必须补上 p(99)：尾延迟正是抓"后端引入问题"（行锁排队、N+1 查询）最灵的指标。
#
#   另注：v1.7.1 有 --new-machine-readable-summary 会改变导出结构，本脚本依赖的是
#   实测过的扁平结构，**不要开那个选项**。
TREND_STATS="avg,min,med,max,p(90),p(95),p(99)"

mkdir -p "$RESULTS_DIR" tools/loadtest/baselines

# ---------------------------------------------------------------- 1. 依赖
echo "== 1/5 确认 PG + Redis =="
docker compose -f deploy/docker-compose.dev.yml up -d >/dev/null
for _ in $(seq 1 40); do
  pg=$(docker inspect --format '{{.State.Health.Status}}' itiao-pg 2>/dev/null || echo starting)
  rd=$(docker inspect --format '{{.State.Health.Status}}' itiao-redis 2>/dev/null || echo starting)
  [ "$pg" = "healthy" ] && [ "$rd" = "healthy" ] && break
  sleep 2
done
[ "$pg" = "healthy" ] && [ "$rd" = "healthy" ] || { echo "PG/Redis 未就绪" >&2; exit 1; }
echo "   PG=healthy Redis=healthy"

# ---------------------------------------------------------------- 2. API
echo "== 2/5 探测 API =="
if ! curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "   API 未在 $BASE_URL 响应。" >&2
  echo "   请另开一个终端先跑：bash scripts/api.sh" >&2
  exit 1
fi
echo "   $BASE_URL/healthz 正常"

# ---------------------------------------------------------------- 3. 数据
if [ "$RESET" = "1" ]; then
  echo "== 3/5 重置数据（保证每次跑的数字可比）=="
  docker exec -i itiao-pg psql -U itiao -d itiao -q -v ON_ERROR_STOP=1 < tools/loadtest/seed/reset.sql
  docker exec -i itiao-pg psql -U itiao -d itiao -q -v ON_ERROR_STOP=1 < tools/loadtest/seed/seed.sql
  echo "   已清空并重新灌入固定种子"
else
  echo "== 3/5 跳过数据重置（RESET=0）=="
fi

# ---------------------------------------------------------------- 4. 冒烟
echo "== 4/5 冒烟（先确认脚本与接口还能对话）=="
BASE_URL="$BASE_URL" k6 run --summary-trend-stats="$TREND_STATS" \
  --summary-export="$RESULTS_DIR/smoke.json" \
  tools/loadtest/scenarios/smoke.js

# ---------------------------------------------------------------- 5. 正式场景
if [ "$RECORD_BASELINE" = "1" ]; then
  echo "== 5/5 建立基线：连跑 $RUNS 次取中位数 =="
  inputs=()
  for i in $(seq 1 "$RUNS"); do
    echo "   --- 第 $i/$RUNS 次 ---"
    BASE_URL="$BASE_URL" k6 run --summary-trend-stats="$TREND_STATS" \
      --summary-export="$RESULTS_DIR/${SCENARIO}-run${i}.json" \
      "tools/loadtest/scenarios/${SCENARIO}.js"
    inputs+=("$RESULTS_DIR/${SCENARIO}-run${i}.json")
  done
  node tools/loadtest/scripts/record-baseline.mjs \
    --scenario "$SCENARIO" --out "$BASELINE" "${inputs[@]}"
  echo
  echo "基线已更新。请像审查代码一样审查它，并在提交信息里说明为什么基线变了。"
  exit 0
fi

echo "== 5/5 跑场景: $SCENARIO =="
BASE_URL="$BASE_URL" k6 run --summary-trend-stats="$TREND_STATS" \
  --summary-export="$RESULTS_DIR/${SCENARIO}.json" \
  "tools/loadtest/scenarios/${SCENARIO}.js"

echo
if [ ! -f "$BASELINE" ]; then
  echo "还没有基线（$BASELINE）。"
  echo "先建立基线：RECORD_BASELINE=1 RUNS=3 bash scripts/perf.sh"
  exit 0
fi

echo "== 与基线比对 =="
node tools/loadtest/scripts/compare.mjs \
  --baseline "$BASELINE" --current "$RESULTS_DIR/${SCENARIO}.json"
