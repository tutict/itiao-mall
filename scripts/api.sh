#!/usr/bin/env bash
# 本地启动 API 服务。
#
# AUTH_PROVIDER / PAYMENT_PROVIDER 默认都是 mock —— 在拿到微信开放平台资质与
# 微信支付商户号之前，这是唯一能跑通完整闭环的配置。
set -euo pipefail

export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT/services/api"

export APP_ENV="${APP_ENV:-dev}"
export HTTP_ADDR="${HTTP_ADDR:-:8080}"
export LOG_LEVEL="${LOG_LEVEL:-info}"
export DATABASE_URL="${DATABASE_URL:-postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable}"
export REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
export AUTH_PROVIDER="${AUTH_PROVIDER:-mock}"
export PAYMENT_PROVIDER="${PAYMENT_PROVIDER:-mock}"
export JWT_SECRET="${JWT_SECRET:-dev-only-secret-please-change}"
export ORDER_PENDING_TTL="${ORDER_PENDING_TTL:-15m}"

echo "构建..."
go build -o ./bin/api.exe ./cmd/api

echo "启动 API  $HTTP_ADDR  auth=$AUTH_PROVIDER payment=$PAYMENT_PROVIDER"
exec ./bin/api.exe serve
