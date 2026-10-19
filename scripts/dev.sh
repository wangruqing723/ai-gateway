#!/usr/bin/env bash
# 本地开发容器（docker-compose.dev.yml）的启停与调试日志清理。
#
# 背景：dev 容器把调试落盘单独放在 ./data/dev，与线上容器共享的 ./data 分开——
#       两套容器共写同一个 JSONL 会互相污染，且 reqID 各自从 r00001 计数会重复。
#       AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL 开启时会把所有请求体落盘，体积增长很快
#       （实测小时级可达 GB），所以 down 时顺带清理。
#
# 用法：
#   ./scripts/dev.sh up      # 构建并启动 dev 容器（等价 compose -f ... up -d --build）
#   ./scripts/dev.sh down    # 停止 dev 容器并清空 ./data/dev/*.jsonl
#   ./scripts/dev.sh clean   # 只清空调试日志，不动容器
#   ./scripts/dev.sh logs    # 跟踪 dev 容器日志
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILE="docker-compose.dev.yml"
DEV_DATA_DIR="data/dev"

# 调试产物的扩展名白名单：只删自己写的落盘文件，不碰目录下其它东西。
clear_debug_logs() {
	if [ ! -d "$DEV_DATA_DIR" ]; then
		echo "[dev] $DEV_DATA_DIR 不存在，跳过清理"
		return
	fi
	# nullglob：没有匹配时展开为空，避免 rm 收到字面量通配符。
	shopt -s nullglob
	local files=("$DEV_DATA_DIR"/*.jsonl)
	if [ "${#files[@]}" -eq 0 ]; then
		echo "[dev] $DEV_DATA_DIR 下无调试日志，跳过清理"
		return
	fi
	local total=0
	for f in "${files[@]}"; do
		local size
		size=$(stat -c '%s' "$f" 2>/dev/null || echo 0)
		total=$((total + size))
		rm -f "$f"
		echo "[dev] 已删除 $f"
	done
	echo "[dev] 调试日志清理完成，共释放 $((total / 1024 / 1024)) MB"
}

case "${1:-}" in
up)
	docker compose -f "$COMPOSE_FILE" up -d --build
	echo "[dev] 完成。管理页面: http://127.0.0.1:7790"
	;;
down)
	# 先停容器再清日志：容器运行时会持续写入，停掉才不会有半截文件。
	docker compose -f "$COMPOSE_FILE" down
	clear_debug_logs
	;;
clean)
	clear_debug_logs
	;;
logs)
	docker compose -f "$COMPOSE_FILE" logs -f
	;;
*)
	echo "用法: $0 {up|down|clean|logs}" >&2
	exit 1
	;;
esac
