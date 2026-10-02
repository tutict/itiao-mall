#!/usr/bin/env bash
# 起本地开发依赖（PG + Redis）并执行迁移。
set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "== 启动 PG + Redis =="
docker compose -f deploy/docker-compose.dev.yml up -d

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
