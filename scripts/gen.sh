#!/usr/bin/env bash
# 契约生成：从 docs/openapi/openapi.yaml 这一份真源，同时生成 Go 服务端接口与 TS 类型。
#
# 生成物全部提交进仓库，CI 用 `git diff --exit-code` 卡漂移 —— 手改生成物会被立刻发现。
set -euo pipefail

# 本机 Bash 的 PATH 是断的，先补 PortableGit
export PATH="/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Users/tutic/.workbuddy/binaries/PortableGit/versions/1.2.0/bin:$PATH"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SPEC="docs/openapi/openapi.yaml"

if [ ! -f "$SPEC" ]; then
  echo "找不到契约文件: $SPEC" >&2
  exit 1
fi

echo "== 1/2 生成 Go 服务端接口 (oapi-codegen) =="
(
  cd services/api
  go tool oapi-codegen --config=../../tools/codegen/config.go.yaml "../../$SPEC"
)
echo "   -> services/api/internal/httpapi/gen/api.gen.go"

echo "== 2/2 生成 TS 类型 (openapi-typescript) =="
# 走自家包装脚本，不直接跑 CLI —— 原因见 tools/codegen/gen-ts-types.cjs 顶部注释：
# TS 7 没有 JS 编译器 API，必须把 `typescript` 劫持到工具侧的 TS 5。
if [ -d "node_modules/openapi-typescript" ] && [ -d "node_modules/typescript5" ]; then
  node tools/codegen/gen-ts-types.cjs "$SPEC" packages/api-contract/src/schema.d.ts
else
  echo "   跳过：缺少 openapi-typescript 或 typescript5。" >&2
  echo "   （lockfile 过旧时按提示补跑一次 pnpm install 即可）" >&2
fi

echo
echo "完成。检查契约是否与生成物一致："
echo "  git diff --exit-code services/api/internal/httpapi/gen packages/api-contract/src/schema.d.ts"
