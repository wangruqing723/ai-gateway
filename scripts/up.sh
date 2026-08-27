#!/usr/bin/env bash
# 跨平台（macOS / Linux·WSL）启动脚本。
#
# 目的：让容器以「当前宿主用户」的 UID/GID 运行，并保证挂载的 config.yaml / data
#       属主与之一致，从而管理页面保存配置时具备写权限。
#
# 背景：docker-compose.yml 里 user 默认 501:20（面向 macOS）。macOS Docker Desktop
#       的文件共享层会自动映射属主，任何 UID 都能写；而原生 Linux/WSL 的 bind mount
#       严格穿透宿主属主，容器 UID 与文件属主不一致就会「permission denied」。
#
# 用法：./scripts/up.sh            # 等价 docker compose up -d
#       ./scripts/up.sh --build   # 额外参数透传给 docker compose up
set -euo pipefail

cd "$(dirname "$0")/.."

UID_VAL="$(id -u)"
GID_VAL="$(id -g)"

# 1) 写入 .env，供 docker compose 变量替换（compose 自动读取仓库根目录 .env）
printf 'AI_GATEWAY_UID=%s\nAI_GATEWAY_GID=%s\n' "$UID_VAL" "$GID_VAL" >.env
echo "[up] .env => AI_GATEWAY_UID=${UID_VAL} AI_GATEWAY_GID=${GID_VAL}"

# 2) 首次运行时用模板初始化 config.yaml，避免 compose 把不存在的文件当目录挂载
if [ ! -f config.yaml ]; then
	cp config.example.yaml config.yaml
	echo "[up] 已从模板生成 config.yaml"
fi
mkdir -p data

# 3) 仅原生 Linux/WSL 需要对齐属主（macOS Docker Desktop 会自行映射，无需 chown）
if [ "$(uname -s)" = "Linux" ]; then
	mismatch=0
	for p in config.yaml data; do
		[ "$(stat -c '%u' "$p" 2>/dev/null || echo -1)" = "$UID_VAL" ] || mismatch=1
	done
	if [ "$mismatch" = "1" ]; then
		echo "[up] 挂载文件属主与当前用户(${UID_VAL})不一致，尝试对齐属主…"
		if [ "$UID_VAL" = "0" ]; then
			chown -R "${UID_VAL}:${GID_VAL}" config.yaml data
		elif command -v sudo >/dev/null 2>&1; then
			sudo chown -R "${UID_VAL}:${GID_VAL}" config.yaml data || {
				echo "[up] 自动 chown 失败，请手动执行: sudo chown -R ${UID_VAL}:${GID_VAL} config.yaml data" >&2
				exit 1
			}
		else
			echo "[up] 无 sudo，请手动执行: chown -R ${UID_VAL}:${GID_VAL} config.yaml data" >&2
			exit 1
		fi
	fi
fi

# 4) 启动
docker compose up -d "$@"
echo "[up] 完成。管理页面: http://127.0.0.1:7789"
