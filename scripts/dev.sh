#!/usr/bin/env bash
# 起本地开发依赖（PG + Redis）并执行迁移。
set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# compose 文件列表统一由 lib 解析 —— 个人端口覆盖必须能生效。
# 细节见 scripts/lib/dev-compose.sh 顶部：显式 -f 会让 Compose 丢掉个人覆盖，
# 进而按仓库映射重建容器，把本机改过的宿主端口清掉。
# shellcheck source=scripts/lib/dev-compose.sh
. "$ROOT/scripts/lib/dev-compose.sh"

echo "== 启动 PG + Redis =="
resolve_dev_compose || exit 1
docker compose "${DEV_COMPOSE_ARGS[@]}" up -d

echo "== 等待健康 =="
for i in $(seq 1 40); do
  pg=$(docker inspect --format '{{.State.Health.Status}}' itiao-pg 2>/dev/null || echo starting)
  rd=$(docker inspect --format '{{.State.Health.Status}}' itiao-redis 2>/dev/null || echo starting)
  if [ "$pg" = "healthy" ] && [ "$rd" = "healthy" ]; then
    echo "   PG 与 Redis 均 healthy"
    break
  fi
  sleep 2
done

echo "== 执行迁移 =="
(
  cd services/api
  go run ./cmd/api migrate
)

echo
echo "完成。启动 API：bash scripts/api.sh"
