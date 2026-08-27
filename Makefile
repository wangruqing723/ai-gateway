# 跨平台启动/停止封装。up 会以宿主用户 UID/GID 运行容器（见 scripts/up.sh）。
.PHONY: up down restart logs

up:
	./scripts/up.sh

down:
	docker compose down

restart: down up

logs:
	docker compose logs -f gateway
