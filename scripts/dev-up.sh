#!/usr/bin/env bash
# 一行命令拉起本地开发环境（PG + Redis + 迁移 + API + web 预览），
# 并在起的前后各做一轮体检 —— 目的是**让开发中出的问题当场暴露**，
# 而不是半小时后表现为"页面怎么不动了"。
#
# 为什么需要它
# ------------
# 仓库里起环境的步骤散在三个脚本里（dev.sh / api.sh / preview-web.mjs），
# 而每个都要人肉记住两组环境变量：`PG_COMPOSE_OVERRIDE` 与 `DATABASE_URL`。
# 这不是"麻烦"，是**静默失败**的温床：
#
#   * 忘了 PG_COMPOSE_OVERRIDE → Compose 丢掉个人端口覆盖 → 按仓库的 5432
#     重建容器 → 本机 5432 落在 Windows 保留端口段（实测 5429-5528）
#     → 容器根本起不来，而报错说的是 bind 权限，看起来像"环境坏了"。
#   * 忘了 DATABASE_URL → 集成测试整体 t.Skip（实测 27 PASS/12 SKIP，
#     退出码 0）→ 丢掉的恰好是超卖/幂等/回调幂等那批最承重的用例。
#   * 两者不一致 → API 连的是 5432、容器在 15432 → 连不上，报的却是
#     "数据库连接失败"，指向错误的方向。
#
# 所以本脚本的分工是：
#   1. **端口从容器问出来，不靠人记**（resolve_*_port）。人给的 DATABASE_URL
#      只在"与容器实际映射一致"时才被采纳，不一致就以容器为准并说明。
#   2. **体检与启动交织**：起之前查工作区状态（最便宜的真信号），
#      起之后查服务真的活着 —— 而且失败时打印**日志尾部与容器状态**，
#      不是只报一句"启动失败"。
#   3. **体检项分两档**：FAIL（可判定为错）与 WARN（启发式，不阻断）。
#      把启发式混进 FAIL 会让守卫变松（铁律 25 的反面：方向相反的两种失败
#      都存在），所以这里明确分开，WARN 只提示。
#
# 幂等：API / web 预览都用 pid 文件记账，重复执行不会起第二份；
#       已在跑就只做体检。要换端口用 API_PORT / WEB_PORT。
#
# 用法
# ----
#   bash scripts/dev-up.sh                 # 起全部 + 体检
#   bash scripts/dev-up.sh --no-web        # 不起 web 预览
#   bash scripts/dev-up.sh --check         # 只体检，不起任何东西
#   bash scripts/dev-up.sh --stop          # 停掉本脚本起过的 API / web 预览
#   bash scripts/dev-up.sh --restart       # 先停再起（改了 env 变量时用）
#
# 环境
# ----
#   API_PORT   默认 8080
#   WEB_PORT   默认 8081
#   PPROF_PORT 剖析端口，默认 6060。设 PPROF_ADDR 覆盖，或设 PROFILE_API=1 用它
#   PROFILE_API=1 起剖析端口（默认不开）。开完就能跑 scripts/profile-api.sh
#   下面这些都有默认值，通常不用设：
#   APP_ENV / AUTH_PROVIDER / PAYMENT_PROVIDER / JWT_SECRET / ORDER_PENDING_TTL
#
# 退出码：0 = 无 FAIL（WARN 不影响）；1 = 有 FAIL 项。
#
# 不做的事（明确交代）
# ----------------------
#   * 不跑 check-all.sh 那 10 项守门 —— 那是提交前的门禁（约 15 分钟），
#     这里的定位是"开发循环里 10 秒内拿到信号"。
#   * 不动你的数据库数据：只前滚迁移，不 reset、不 seed。
#     需要干净数据用 tools/loadtest/seed/{reset,seed}.sql 自己跑。

# 故意不用 set -e：体检项要记账后继续跑（一次看到全部问题比一轮修一个快）。
# 真正会中断的地方（build / migrate）单独判退出码。
set -uo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:/c/Program Files/Go/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# 运行时状态放 .tmp/（已 gitignore）。放 .workbuddy/ 不行 ——
# 那是个人目录，干净检出里不存在，而这里的文件是**运行必需**的。
RUNTIME="$ROOT/.tmp/dev"
API_PID_FILE="$RUNTIME/api.pid"
WEB_PID_FILE="$RUNTIME/web.pid"
API_LOG="$RUNTIME/api.log"
WEB_LOG="$RUNTIME/web.log"

API_PORT="${API_PORT:-8080}"
WEB_PORT="${WEB_PORT:-8081}"
PPROF_PORT="${PPROF_PORT:-6060}"

# 剖析端口默认**不**开。
#
# 为什么给开关而不是常开：pprof 可以读到进程内存（heap/allocs 带出分配点的
# 参数内容、goroutine?debug=2 带出每个协程的完整栈）。开发机上开一个能被
# 本机任意进程访问的内存读取端点，不该是默认状态。
#
# 需要它时：PROFILE_API=1 bash scripts/dev-up.sh --restart
PROFILE_API="${PROFILE_API:-0}"

ACTION="up"
WITH_WEB=1
[ "${1:-}" = "--check" ] && ACTION="check"
[ "${1:-}" = "--stop" ] && ACTION="stop"
[ "${1:-}" = "--no-web" ] && WITH_WEB=0
[ "${1:-}" = "--restart" ] && ACTION="restart"

FAIL=()
WARN=()

fail() { FAIL+=("$1"); printf '  \033[31m  ✗ %s\033[0m\n' "$1"; }
warn() { WARN+=("$1"); printf '  \033[33m  ! %s\033[0m\n' "$1"; }
ok() { printf '  \033[32m  ✓ %s\033[0m\n' "$1"; }
info() { printf '      %s\n' "$1"; }
section() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

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

# ---- 端口与进程的小工具 ----

# 从 netstat 找监听某端口的 PID。字段：TCP <local> <remote> LISTENING <pid>
port_pid() {
  netstat -ano 2>/dev/null \
    | awk -v p=":$1" '$4 == "LISTENING" && $2 ~ p"$" { print $5; exit }'
}

port_busy() { [ -n "$(port_pid "$1")" ]; }

port_owner_desc() {
  local pid; pid="$(port_pid "$1")"
  [ -z "$pid" ] && { echo "（未找到监听进程）"; return; }
  local name
  name="$(tasklist //FI "PID eq $pid" //FO CSV //NH 2>/dev/null | head -1 | cut -d',' -f1)"
  echo "PID ${pid}${name:+（${name}）}"
}

# 判断"本脚本起的服务还活着"。
#
# 为什么不用 pid 文件 + tasklist 判活（那版试过，是错的）：
#   * bash 后台起进程时 `$!` 是那个**子 shell** 的 PID，不是最终 exec 的
#     node/api 进程 —— 实测 pid 文件记 530，真正监听 8081 的是 40224。
#     于是"进程活着吗"这个问题从一开始就用错了 PID，问什么都不对。
#   * 就算 PID 对，`tasklist /FI "PID eq n"` 查不到时输出的是中文
#     "信息: 没有运行的任务匹配指定标准。"，grep 匹配 0 → 判死。
#     把"查不到"当成"已死"，正是铁律 27 那类毛病（跳过与真通过不可区分），
#     只不过方向相反：这里把"不知道"当成了"否"。
#
# 所以活性判据只有一个：**服务真的能应答**。
# pid 文件只用于 --stop 时的清理目标，并且与 netstat 的实际监听者取并集 ——
# 宁可多杀一个本项目的进程，也不要留一个占着端口没人认领。
pid_file_pid() {
  local f="$1"
  [ -f "$f" ] || return 1
  tr -d ' \r\n' < "$f"
}

# 监听某端口的实际 PID（空 = 没人监听）
listen_pid() { port_pid "$1"; }

# 停掉某端口上的服务。pid 文件与实际监听者取并集（理由见 pid_file_pid 的注释）。
stop_on_port() {
  local port="$1" label="$2" f="$3" pid hit=0
  pid="$(pid_file_pid "$f" 2>/dev/null || true)"
  if [ -n "$pid" ]; then
    if tasklist //FI "PID eq $pid" //NH 2>/dev/null | grep -q "[0-9]"; then
      taskkill //F //PID "$pid" >/dev/null 2>&1 && { ok "已停止 $label（pid 文件 PID $pid）"; hit=1; }
    fi
  fi
  # 以 netstat 为准再收一次：那才是真正占着端口的进程。
  local lp; lp="$(listen_pid "$port")"
  if [ -n "$lp" ] && [ "$lp" != "$pid" ]; then
    if taskkill //F //PID "$lp" >/dev/null 2>&1; then
      ok "已停止 $label（端口 $port 的实际监听者 PID $lp）"
      hit=1
    fi
  fi
  rm -f "$f"
  [ "$hit" = "1" ] || info "$label 本来就没在跑"
  return 0
}

wait_http() {
  local url="$1" tries="${2:-30}" i code
  for ((i = 0; i < tries; i++)); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url" 2>/dev/null)"
    if [ "$code" = "200" ]; then return 0; fi
    sleep 0.5
  done
  return 1
}

tail_file() {
  local f="$1" n="${2:-15}"
  if [ -f "$f" ]; then
    info "---- $(basename "$f") 末尾 ${n} 行 ----"
    tail -n "$n" "$f" | sed 's/^/      /'
  else
    info "（日志文件不存在：$f）"
  fi
}

# ---- 端口从容器问出来 ----
#
# 这是本脚本最要紧的一段。人给的 DATABASE_URL 经常与容器实际映射不一致
# （个人覆盖把宿主端口改成了 15432），而连不上时报的却是"数据库连接失败"——
# 指向错误方向。这里以 docker inspect 的结果为准。

# 打印容器内 <container_port>/tcp 对应的宿主端口。
#
# 两个必须注意的地方（都实测踩过）：
#
# 1. docker 对同时绑定 IPv4/IPv6 的映射返回**两条**记录
#    （{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"},
#    {"HostIp":"::","HostPort":"15432"}]}），Go template 的 range 会把两条
#    都吐出来。
# 2. 但它们之间**没有分隔符**，于是结果是单个词 '1543215432'，不是两行 ——
#    所以 `head -1` 拦不住（实测 repr 确认 newline count == 1）。
#    这个值是纯数字，靠"是不是数字"也拦不住，只会在下游炸成一句
#    "invalid port"，看起来像配置写错，实际是探测逻辑的 bug。
#
# 修法：不靠模板切分，直接取第一个空白分隔的词。两条记录 HostPort 相同
# （同一映射的两种协议族），所以取任一条都对。
container_host_port() {
  local container="$1" cport="$2" out
  out="$(docker inspect --format \
    "{{range \$p, \$conf := .NetworkSettings.Ports}}{{if eq \$p \"${cport}/tcp\"}}{{range \$conf}}{{.HostPort}} {{end}}{{end}}{{end}}" \
    "$container" 2>/dev/null | tr -d '\r' | awk '{print $1}')"
  case "$out" in
    ''|*[!0-9]*) return 1 ;;
  esac
  echo "$out"
}

container_state() {
  docker inspect --format '{{.State.Status}}' "$1" 2>/dev/null | tr -d ' \r\n'
}

container_health() {
  docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}无 healthcheck{{end}}' \
    "$1" 2>/dev/null | tr -d ' \r\n'
}

# =====================================================================
# 体检：起之前（不依赖任何服务）
# =====================================================================
preflight() {
  section "起之前：工作区状态"

  # P1 变异体残留 —— 最便宜的真信号，0.5 秒。
  #
  # 为什么排最前：上一轮 verify-guards 被 Ctrl-C / SIGTERM 打断时，被改坏的
  # 源码会留在工作区（实测 4 处）。那种状态下后面的测试/启动都会红，
  # 而红指向的是变异体不是真缺陷。
  local py
  if py="$(resolve_python)"; then
    if "$py" -u scripts/verify-guards.py --audit >/dev/null 2>&1; then
      ok "变异体残留自检通过（45 条全部处于原始状态）"
    else
      fail "工作区里留着上一轮没还原的变异体 —— 先查清楚再启动"
      "$py" -u scripts/verify-guards.py --audit 2>&1 | sed 's/^/      /' | tail -15
    fi
  else
    warn "找不到 Python，跳过变异体残留自检（设 PYTHON_BIN 可启用）"
  fi

  # P2 契约与生成物的先后 —— 启发式，只提示。
  #
  # 为什么是 WARN 不是 FAIL：mtime 只说明"谁更晚被写过"，不说明生成物是错的
  # （git checkout、编辑器保存都会动 mtime）。所以它不能阻断启动。
  local spec="docs/openapi/openapi.yaml"
  local gen_go="services/api/internal/httpapi/gen/api.gen.go"
  local gen_ts="packages/api-contract/src/schema.d.ts"
  if [ -f "$spec" ]; then
    local stale=""
    [ -f "$gen_go" ] && [ "$spec" -nt "$gen_go" ] && stale="$gen_go"
    [ -f "$gen_ts" ] && [ "$spec" -nt "$gen_ts" ] && stale="$stale $gen_ts"
    if [ -n "$stale" ]; then
      warn "契约比生成物新 —— 很可能改了契约忘了跑 gen.sh"
      info "落后于契约的文件：$stale"
      info "修法：bash scripts/gen.sh（结果要一起提交）"
    else
      ok "生成物不比契约旧"
    fi
  fi

  # P3 生成物有未提交改动 —— 提交前的门禁会红，但开发中看不见。
  local dirty_gen
  dirty_gen="$(git status --porcelain -- "$gen_go" "$gen_ts" 2>/dev/null | head -2)"
  if [ -n "$dirty_gen" ]; then
    warn "生成物有未提交改动："
    printf '%s\n' "$dirty_gen" | sed 's/^/      /'
  fi

  # P4 Docker 服务端活着吗。
  #
  # 为什么值得单独查：Docker Desktop（WSL2）重启后，没有 restart 策略的
  # 容器会停在 Created 状态 —— `docker ps` 看不到它，`docker exec` 报
  # "is not running"，而 `docker info` 又是好的。分开查才能把这三种情况分开。
  if ! docker info >/dev/null 2>&1; then
    fail "Docker 服务端没起来（Docker Desktop 未运行？）"
    info "现象：docker CLI 在，报错是 named pipe 找不到"
    info "修法：启动 Docker Desktop，等托盘图标变成 running 再重跑本脚本"
    return 1
  fi
  ok "Docker 服务端可用"

  # P5 端口冲突：谁占了要用的端口。
  # 已经是我们起的（pid 文件活着）就不算冲突 —— 幂等。
  if port_busy "$API_PORT"; then
    if wait_http "http://localhost:${API_PORT}/healthz" 2; then
      info ":$API_PORT 已有 API 在应答（PID $(port_pid "$API_PORT")）—— 将复用"
    else
      fail ":$API_PORT 被占用但不应答：$(port_owner_desc "$API_PORT")"
      info "换端口：API_PORT=8081 bash scripts/dev-up.sh"
      info "若是上次遗留的本项目进程：bash scripts/dev-up.sh --stop"
    fi
  fi
  if [ "$WITH_WEB" = "1" ]; then
    if port_busy "$WEB_PORT"; then
      if curl -s -o /dev/null --max-time 2 "http://localhost:${WEB_PORT}/" 2>/dev/null; then
        info ":$WEB_PORT 已有 web 预览在应答（PID $(port_pid "$WEB_PORT")）—— 将复用"
      else
        warn ":$WEB_PORT 被占用：$(port_owner_desc "$WEB_PORT") —— web 预览可能起不来"
        info "换端口：WEB_PORT=8082 bash scripts/dev-up.sh"
      fi
    fi
  fi
  return 0
}

# =====================================================================
# 起依赖
# =====================================================================
start_infra() {
  section "起 PG + Redis"

  # 个人端口覆盖走显式变量：命令行一旦给了 -f，Compose 就不再读
  # COMPOSE_FILE、也不合并 override 文件 —— 覆盖会被静默丢掉。
  # 细节见 scripts/lib/dev-compose.sh 顶部。
  #
  # 这里自动采用 .workbuddy/pg-port-override.yml（若存在），因为它就是
  # "个人机器专用、不入库"的那份覆盖。注意这与铁律 26 相反而安全：
  # 那条讲的是门禁脚本**硬依赖** gitignore 目录（缺失即永远红），
  # 这里依赖的是**可选**文件（缺失即回退到仓库默认），不设它照样能跑。
  if [ -z "${PG_COMPOSE_OVERRIDE:-}" ] && [ -f "$ROOT/.workbuddy/pg-port-override.yml" ]; then
    PG_COMPOSE_OVERRIDE=".workbuddy/pg-port-override.yml"
    export PG_COMPOSE_OVERRIDE
    info "自动采用个人端口覆盖：$PG_COMPOSE_OVERRIDE"
  fi
  # shellcheck source=scripts/lib/dev-compose.sh
  . "$ROOT/scripts/lib/dev-compose.sh"
  resolve_dev_compose || { fail "dev 编排解析失败"; return 1; }

  if ! docker compose "${DEV_COMPOSE_ARGS[@]}" up -d >/dev/null; then
    fail "docker compose up 失败"
    info "宿主端口绑不上时最常见的原因是落在 Windows 保留端口段"
    info "（本机实测 5429-5528 被 Hyper-V/winnat 占着，5432 在其中）"
    info "个人机器的修法见 .workbuddy/pg-port-override.yml 的注释"
    return 1
  fi

  # 等健康，且**超时后给出能直接照做的诊断** —— 只说"超时"等于没说。
  local i pg rd
  for i in $(seq 1 60); do
    pg="$(container_health itiao-pg)"
    rd="$(container_health itiao-redis)"
    if [ "$pg" = "healthy" ] && [ "$rd" = "healthy" ]; then
      ok "PG 与 Redis 均 healthy"
      return 0
    fi
    sleep 1
  done

  fail "等 PG/Redis 健康超时（pg=$pg redis=$rd）"
  # 分三种情况给不同的解法 —— 这是实测踩过的坑：Docker Desktop 重启后
  # 容器停在 Created，docker ps 看不到，报错却说"is not running"。
  local pgs rds
  pgs="$(container_state itiao-pg)"
  rds="$(container_state itiao-redis)"
  info "itiao-pg 状态=${pgs:-查不到}　itiao-redis 状态=${rds:-查不到}"
  if [ "$pgs" = "Created" ] || [ "$rds" = "Created" ]; then
    warn "容器停在 Created —— Docker Desktop（WSL2）重启后无 restart 策略的容器就是这样"
    info "修法：docker start itiao-pg itiao-redis，或 bash scripts/dev-up.sh --restart"
  elif [ "$pgs" = "running" ] || [ "$rds" = "running" ]; then
    warn "容器在 running 但 healthcheck 不通过 —— 多半是数据目录或端口映射的问题"
    info "看日志：docker logs --tail 30 itiao-pg"
  else
    warn "容器没起来 —— 端口映射失败最常见（宿主端口被占/落在保留段）"
    info "看日志：docker logs --tail 30 itiao-pg"
  fi
  return 1
}

# =====================================================================
# 编译（迁移与运行共用同一个二进制，所以必须排在两者之前）
# =====================================================================
build_api() {
  section "编译 API"
  if (cd services/api && go build -o ./bin/api.exe ./cmd/api); then
    ok "services/api/bin/api.exe"
    return 0
  fi
  fail "编译失败 —— 服务起不来，先修编译错误"
  return 1
}

# =====================================================================
# 数据库：迁移 + 数据状态
# =====================================================================
setup_db() {
  section "数据库"

  local pg_port
  pg_port="$(container_host_port itiao-pg 5432)" || pg_port=""
  if [ -z "$pg_port" ]; then
    fail "问不到 PG 的宿主端口 —— 容器没起来，或端口映射为空"
    info "容器状态：itiao-pg=$(container_state itiao-pg) 健康=$(container_health itiao-pg)"
    info "看映射：docker inspect --format '{{json .NetworkSettings.Ports}}' itiao-pg"
    return 1
  fi
  info "容器实际映射：宿主 :$pg_port → 容器 5432"

  # 人给的 DATABASE_URL 只在与容器一致时才采纳。不一致时**以容器为准**并说明
  # —— 让它继续错下去只会得到一句误导性的"数据库连接失败"。
  local derived="postgres://itiao:itiao@localhost:${pg_port}/itiao?sslmode=disable"
  if [ -z "${DATABASE_URL:-}" ]; then
    info "你未设 DATABASE_URL —— 采用容器映射（:$pg_port）"
  elif printf '%s' "$DATABASE_URL" | grep -q ":${pg_port}\(\$\|/\|@\)"; then
    ok "DATABASE_URL 与容器映射一致（:$pg_port）"
  else
    warn "你给的 DATABASE_URL 与容器实际映射不一致 —— 本脚本以容器为准"
    info "你的：$DATABASE_URL"
    info "采用：$derived"
  fi
  export DATABASE_URL="$derived"

  local rd_port
  rd_port="$(container_host_port itiao-redis 6379)" || rd_port=""
  [ -n "$rd_port" ] && export REDIS_ADDR="localhost:${rd_port}"

  # 迁移。serve 不会自动迁移（实测踩过：服务照常起，一查就 500）。
  if ! (cd services/api && ./bin/api.exe migrate >"$RUNTIME/migrate.log" 2>&1); then
    fail "迁移失败"
    tail_file "$RUNTIME/migrate.log" 20
    return 1
  fi
  ok "迁移已前滚到最新"

  # 数据状态。**只报告不动手** —— 开发库是你的，不该被脚本清掉。
  local counts
  counts="$(docker exec -i itiao-pg psql -U itiao -d itiao -tAc \
    "select 'users='||count(*) from users union all select 'products='||count(*) from products union all select 'merchant_profiles='||count(*) from merchant_profiles;" 2>/dev/null | tr -d '\r')"
  if [ -z "$counts" ]; then
    fail "查不到数据统计 —— 迁移那步可能没真落到这个库"
  else
    info "$(printf '%s' "$counts" | tr '\n' ' ')"
    local users; users="$(printf '%s' "$counts" | sed -n 's/^users=//p')"
    # 种子是 3 个用户。跑过集成测试之后 users 会膨胀到上千行且不清库，
    # 之后 mock 登录/探针的结果会变得难解释 —— 值得提醒但不该阻断。
    if [ -n "$users" ] && [ "$users" -gt 50 ]; then
      warn "users 表有 $users 行（种子只有 3）—— 跑过集成测试且没清库"
      info "要干净数据：docker exec -i itiao-pg psql -U itiao -d itiao < tools/loadtest/seed/reset.sql 然后 seed.sql"
    fi
  fi
  return 0
}

# =====================================================================
# 起 API
# =====================================================================
start_api() {
  section "API（:$API_PORT）"

  if wait_http "http://localhost:${API_PORT}/healthz" 4; then
    ok "已在运行（PID $(listen_pid "$API_PORT")），跳过启动"
    return 0
  fi
  rm -f "$API_PID_FILE"

  export APP_ENV="${APP_ENV:-dev}"
  export AUTH_PROVIDER="${AUTH_PROVIDER:-mock}"
  export PAYMENT_PROVIDER="${PAYMENT_PROVIDER:-mock}"
  export JWT_SECRET="${JWT_SECRET:-dev-only-secret-please-change}"
  export ORDER_PENDING_TTL="${ORDER_PENDING_TTL:-15m}"
  export LOG_LEVEL="${LOG_LEVEL:-warn}"
  export HTTP_ADDR=":${API_PORT}"

  # 剖析端口。绑**显式回环**而不是 ":${PPROF_PORT}" ——
  # 后者在 Go 里是通配地址（所有网卡），形状最像"本机端口"实际全网可达。
  # 即便这里只是开发机，也按生产校验的同一口径写，免得脚本本身示范了坏写法。
  if [ "$PROFILE_API" = "1" ]; then
    export PPROF_ADDR="${PPROF_ADDR:-127.0.0.1:${PPROF_PORT}}"
  else
    # 显式清空：否则继承了调用者的 PPROF_ADDR 却不自知，
    # 而体检项会报"剖析端口没开"，现象与意图相反。
    unset PPROF_ADDR
  fi

  mkdir -p "$RUNTIME"
  : > "$API_LOG"
  info "auth=$AUTH_PROVIDER payment=$PAYMENT_PROVIDER（mock 仅限本地）"

  # 后台起；日志落文件。不用 /dev/null —— 本机上它不可靠。
  (
    cd services/api || exit 1
    exec ./bin/api.exe serve >>"$API_LOG" 2>&1
  ) &
  local api_pid=$!
  echo "$api_pid" > "$API_PID_FILE"

  if wait_http "http://localhost:${API_PORT}/healthz" 40; then
    ok "起来了（PID $api_pid）"
    if [ "$PROFILE_API" = "1" ]; then
      # 单独确认剖析端口：**启动日志说开了**不等于**真的在应答**。
      # 两者不一致时现象是"我明明配了 PPROF_ADDR 却抓不到 profile"，
      # 而配置、启动日志、实际监听三处看起来一切正常。
      if wait_http "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap?debug=1" 4; then
        ok "剖析端口 http://127.0.0.1:${PPROF_PORT}（CPU profile 抓取：scripts/profile-api.sh）"
      else
        fail "剖析端口 :$PPROF_PORT 没在应答 —— 抓 profile 会失败"
        tail_file "$API_LOG" 15
      fi
    fi
    return 0
  fi

  fail "起不来：:$API_PORT 的 /healthz 没在 20 秒内返回 200"
  tail_file "$API_LOG" 25
  rm -f "$API_PID_FILE"
  stop_on_port "$API_PORT" "API" "$API_PID_FILE"
  return 1
}

# =====================================================================
# 起 web 预览
# =====================================================================
start_web() {
  section "web 预览（:$WEB_PORT）"

  if [ "$WITH_WEB" != "1" ]; then
    info "--no-web：跳过"
    return 0
  fi

  local dir="apps/app/dist-web"
  if [ ! -d "$dir" ]; then
    fail "没有 $dir —— web 预览需要先导出静态产物"
    info "修法：cd apps/app && pnpm run export:web"
    return 1
  fi

  if curl -s -o /dev/null --max-time 2 "http://localhost:${WEB_PORT}/" 2>/dev/null; then
    ok "已在运行（PID $(listen_pid "$WEB_PORT")）"
  else
    rm -f "$WEB_PID_FILE"
    mkdir -p "$RUNTIME"
    : > "$WEB_LOG"
    node scripts/preview-web.mjs --port "$WEB_PORT" --dir "$dir" >>"$WEB_LOG" 2>&1 &
    local pid=$!
    echo "$pid" > "$WEB_PID_FILE"
    # 活性判据是"能应答"，不是"进程还在" —— 后者在 bash 后台起
    # node 的场景下问的是子 shell 的 PID，永远问不准（详见 pid_file_pid）。
    local i
    for ((i = 0; i < 20; i++)); do
      if curl -s -o /dev/null --max-time 2 "http://localhost:${WEB_PORT}/" 2>/dev/null; then
        break
      fi
      sleep 0.3
    done
    if curl -s -o /dev/null --max-time 2 "http://localhost:${WEB_PORT}/" 2>/dev/null; then
      ok "起来了（PID $(listen_pid "$WEB_PORT")）"
    else
      fail "起不来：:$WEB_PORT 没有应答"
      tail_file "$WEB_LOG" 15
      return 1
    fi
  fi

  # 静态导出不会自动跟随源码改动 —— 改了 app 源码却忘了重新导出，
  # 页面看起来"改了没反应"。这是开发里最难自查的一类问题。
  if [ -f "$dir/index.html" ]; then
    local newest
    newest="$(find apps/app/app apps/app/src -type f \( -name '*.tsx' -o -name '*.ts' \) -newer "$dir/index.html" 2>/dev/null | head -3)"
    if [ -n "$newest" ]; then
      warn "有源码比 dist-web 新 —— 预览看到的是旧产物"
      printf '%s\n' "$newest" | sed 's/^/      /'
      info "重新导出：cd apps/app && pnpm run export:web"
    fi
  fi
  return 0
}

# =====================================================================
# 收尾
# =====================================================================
summary() {
  printf '\n'
  printf '\033[1m== 汇总 ==\033[0m\n'
  if [ "${#WARN[@]}" -gt 0 ]; then
    printf '\033[33m%d 项提示（WARN，不阻断）\033[0m\n' "${#WARN[@]}"
  fi
  if [ "${#FAIL[@]}" -gt 0 ]; then
    printf '\033[31m%d 项失败：\033[0m\n' "${#FAIL[@]}"
    for f in "${FAIL[@]}"; do printf '  - %s\n' "$f"; done
    printf '\n停掉本脚本起过的服务：bash scripts/dev-up.sh --stop\n'
    exit 1
  fi

  printf '\n\033[32m起来了。\033[0m\n'
  if wait_http "http://localhost:${API_PORT}/healthz" 2; then
    printf '  API        http://localhost:%s   （日志 %s）\n' "$API_PORT" "${API_LOG#$ROOT/}"
  fi
  if [ "$WITH_WEB" = "1" ] && curl -s -o /dev/null --max-time 2 "http://localhost:${WEB_PORT}/" 2>/dev/null; then
    printf '  web 预览   http://localhost:%s\n' "$WEB_PORT"
  fi
  printf '  健康检查   http://localhost:%s/healthz\n' "$API_PORT"
  if [ "$PROFILE_API" = "1" ] && wait_http "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap?debug=1" 1; then
    printf '  剖析端口   http://127.0.0.1:%s/debug/pprof/   （scripts/profile-api.sh 抓 CPU profile）\n' "$PPROF_PORT"
  fi
  printf '\n提交前跑完整门禁（约 15 分钟）：bash scripts/check-all.sh\n'
  printf '停掉服务：bash scripts/dev-up.sh --stop\n'
  exit 0
}

# =====================================================================
# 主流程
# =====================================================================
stop_all() {
  section "停服务"
  mkdir -p "$RUNTIME"
  stop_on_port "$WEB_PORT" "web 预览" "$WEB_PID_FILE"
  stop_on_port "$API_PORT" "API" "$API_PID_FILE"
  ok "PG / Redis 保持运行（--stop 不动数据与依赖）"
  exit 0
}

stop_all_quiet() {
  stop_on_port "$WEB_PORT" "web 预览" "$WEB_PID_FILE" >/dev/null 2>&1
  stop_on_port "$API_PORT" "API" "$API_PID_FILE" >/dev/null 2>&1
}

mkdir -p "$RUNTIME"

case "$ACTION" in
  stop) stop_all ;;
  check)
    # 只体检：不改任何状态。除了 preflight，还要在服务已在跑时探一下活性。
    if ! preflight; then
      summary
    fi
    if wait_http "http://localhost:${API_PORT}/healthz" 2; then
      ok "API :$API_PORT 活着"
    else
      warn "API :$API_PORT 没在跑（正常 —— 检查模式不启动它）"
    fi
    summary
    ;;
  restart)
    stop_all_quiet
    ;;
esac

if ! preflight; then
  # Docker 都没起来时，后面每一步都会以不同的方式失败 ——
  # 一次报全不如在这里停下、把话说清楚。
  summary
fi

start_infra || summary
build_api  || summary
setup_db   || summary
start_api  || summary
start_web  || summary

# 把端口写进环境，供同一 shell 后续使用（例如手动跑 go test）。
cat > "$RUNTIME/env.sh" <<ENVEOF
export DATABASE_URL="${DATABASE_URL:-}"
export REDIS_ADDR="${REDIS_ADDR:-}"
export API_PORT="${API_PORT}"
export WEB_PORT="${WEB_PORT}"
ENVEOF
info "环境变量已写入 .tmp/dev/env.sh（同一 shell 里 source 它即可）"

summary