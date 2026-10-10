#!/usr/bin/env bash
# k6 性能门禁：一条命令跑完「确认依赖 → 重置数据 → 冒烟 → 负载 → 基线比对」。
#
# 用法:
#   bash scripts/perf.sh                 # 跑 load 并与基线比对
#   SCENARIO=oversell bash scripts/perf.sh   # 跑并发超卖验证
#   RECORD_BASELINE=1 RUNS=3 bash scripts/perf.sh   # 跑 3 次取中位数写入基线
#   RESET=0 bash scripts/perf.sh         # 跳过数据重置（调试时省时间）
#
# ★ 被测 API 必须跑在**放宽了限流**的配置下，否则测的是限流器而不是应用。
#   load.js 的 browse() 是匿名的，全部读请求共用一个 IP 桶，需要约 8000 次读/分钟，
#   而默认阈值是 120 次/分钟 —— 实测 ≈98% 的请求吃 429、http_req_failed 冲到 95%。
#   走 scripts/verify-perf-gate.sh 或 deploy/docker-compose.perf.yml 时已经设好了
#   （RATE_LIMIT_MAX / RATE_LIMIT_AUTH_MAX）；自己起 API 跑 perf.sh 的话要手动带上，
#   否则红线来自限流、与基线（记录于限流上线之前）也不可比。
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

# 清掉限流计数键。**必须跟 DB 重置一起做**，理由与 DB 重置完全相同：让每次跑的数字可比。
#
# 为什么不清会出事（实测）：未登录的商品读取是按 **IP** 建桶的 ——
# 键形如 `rl:dev:read:product.read:127.0.0.1`，默认 `RATE_LIMIT_MAX=120` / 1m。
# 冒烟每轮 2 次商品读取，一次 20s 的冒烟约 78 次，单跑没事；
# 但 `scripts/verify-perf-gate.sh` 会**连着跑两次** perf.sh ——
# 第二次开始时上一次的计数还在同一个桶里，累计越过 120 之后读接口开始 429，
# `http_req_failed` 直接越过冒烟的 `rate<0.01` 阈值。
# 症状是那道门禁的"还原后必须转绿"一步永远失败，而服务器其实是健康的。
#
# 只删限流前缀下的键，**不 FLUSHDB**：refresh token 等其它状态与"数字可比"无关，
# 顺手清掉只会让正在跑的会话莫名掉线。前缀与 main.go 的 `"rl:"+AppEnv` 一致。
RATE_LIMIT_KEY_PATTERN="${RATE_LIMIT_KEY_PATTERN:-rl:*}"
reset_rate_limit_keys() {
  local keys
  keys="$(docker exec -i itiao-redis redis-cli --scan --pattern "$RATE_LIMIT_KEY_PATTERN" 2>/dev/null | tr -d '\r')"
  if [ -z "$keys" ]; then
    return 0
  fi
  # 一次 DEL 带上所有键（键数很少，几十一百个量级）；xargs 会在参数过长时自动分批。
  printf '%s\n' "$keys" | xargs docker exec -i itiao-redis redis-cli DEL >/dev/null
  printf '   已清掉 %d 个限流计数键（%s）\n' "$(printf '%s\n' "$keys" | wc -l)" "$RATE_LIMIT_KEY_PATTERN"
}

# ---------------------------------------------------------------- 1. 依赖
#
# compose 文件列表统一由 lib 解析。原来这里写死 `-f deploy/docker-compose.dev.yml`，
# 而显式 -f 会让 Compose 丢掉个人覆盖（COMPOSE_FILE / override 文件），再按仓库里的
# 5432 映射**重建**容器 —— 本机 5432 落在 Windows 保留端口段，绑不上，PG 直接起不来。
# 它是本脚本的**第一步**，所以后果是"跑一次性能门禁，后面所有依赖库的步骤全红"，
# 而根因是一个被静默丢掉的端口覆盖。见 scripts/lib/dev-compose.sh。
echo "== 1/5 确认 PG + Redis =="
# shellcheck source=scripts/lib/dev-compose.sh
. "$ROOT/scripts/lib/dev-compose.sh"
resolve_dev_compose || exit 1
docker compose "${DEV_COMPOSE_ARGS[@]}" up -d >/dev/null
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
  reset_rate_limit_keys
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
