# 跨平台启动/停止封装。up 会以宿主用户 UID/GID 运行容器（见 scripts/up.sh）。
.PHONY: up down restart logs web-css

up:
	./scripts/up.sh

down:
	docker compose down

restart: down up

logs:
	docker compose logs -f gateway

# 生成前端样式。产物 cmd/gateway/web/vendor/tailwind.css 随仓库提交，
# 因为它要被 //go:embed 打进二进制，运行镜像里没有 node 工具链。
# 改动 index.html 里的 class 之后必须重跑这条，否则新类名没有对应样式。
web-css:
	cd cmd/gateway/web && npx --yes tailwindcss@3.4.17 \
		-c tailwind.config.js -i src/tailwind.css -o vendor/tailwind.css -m
