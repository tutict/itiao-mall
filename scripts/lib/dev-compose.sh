#!/usr/bin/env bash
# dev 编排（PG + Redis）的 compose 文件列表解析。
#
# 为什么需要单独一个文件：dev.sh 与 perf.sh 都要起这套依赖，而两边各写一遍
# `docker compose -f ... up -d` 时，改一处漏一处 —— 这类"同一个不变式在多个
# 地方各有一份实现"的坑在本仓库已经踩过（见 README/记忆里的 ratelimit helper）。
#
# ---------------------------------------------------------------------------
# 要解决的真正问题：显式 `-f` 会**丢掉**个人 compose 覆盖
#
# 仓库里的 deploy/docker-compose.dev.yml 把 PG 映射到宿主 5432，这在多数机器和
# CI 上是对的。但有些开发机 5432 根本绑不上（本机是 Windows 的保留端口段
# Hyper-V/winnat 覆盖了 5429-5528），只能在个人环境里覆盖宿主端口。
#
# 关键在于 Compose 的规则：命令行一旦给出 `-f`，它就不再读 COMPOSE_FILE，
# 也不再自动合并 docker-compose.override.yml。于是
#
#     docker compose -f deploy/docker-compose.dev.yml up -d
#
# 的实际效果是：按仓库里的映射**重建**容器 —— 个人那份 `15432:5432` 被静默清掉，
# 而 5432 又绑不上，PG 直接起不来。症状很误导人：跑一次性能门禁，本地库没了；
# 而报错出现在**后面的**步骤上（go test / 探针 / 验收全说"连不上库"），
# 看起来像应用坏了。
#
# 所以覆盖走一个显式变量，dev.sh 与 perf.sh 都从这里取：
#
#     PG_COMPOSE_OVERRIDE=<覆盖文件> bash scripts/dev.sh
#     PG_COMPOSE_OVERRIDE=<覆盖文件> bash scripts/perf.sh
#
# 覆盖文件不入库（那是个人环境配置）。内容是：
#
#     services:
#       pg:
#         ports: !override
#           - "15432:5432"
#
# 不设这个变量时行为与从前完全一致（只有仓库里的那一份 compose 文件）。
# ---------------------------------------------------------------------------
#
# 用法：
#     . "$ROOT/scripts/lib/dev-compose.sh"
#     resolve_dev_compose || exit 1
#     docker compose "${DEV_COMPOSE_ARGS[@]}" up -d

resolve_dev_compose() {
  DEV_COMPOSE_ARGS=(-f deploy/docker-compose.dev.yml)

  if [ -n "${PG_COMPOSE_OVERRIDE:-}" ]; then
    if [ ! -f "$PG_COMPOSE_OVERRIDE" ]; then
      # 不静默忽略：写错路径时若当成"没设覆盖"，容器会被按仓库映射重建，
      # 于是又回到上面那个"本地库悄悄没了"的形态。
      echo "PG_COMPOSE_OVERRIDE 指向的文件不存在：$PG_COMPOSE_OVERRIDE" >&2
      return 1
    fi
    DEV_COMPOSE_ARGS+=(-f "$PG_COMPOSE_OVERRIDE")
  fi

  return 0
}
