# 跨平台启动/停止封装。up 会以宿主用户 UID/GID 运行容器（见 scripts/up.sh）。
.PHONY: up down restart logs web-css web-html web

up:
	./scripts/up.sh

down:
	docker compose down

restart: down up

logs:
	docker compose logs -f gateway

# 生成前端样式。产物 cmd/gateway/web/vendor/tailwind.css 随仓库提交，
# 因为它要被 //go:embed 打进二进制，运行镜像里没有 node 工具链。
# 改动 src/ 里的 class 之后必须重跑这条，否则新类名没有对应样式。
# 扫描范围见 tailwind.config.js 的 content：只扫 src/，与本目标的执行顺序无关。
web-css:
	cd cmd/gateway/web && npx --yes tailwindcss@3.4.17 \
		-c tailwind.config.js -i src/tailwind.css -o vendor/tailwind.css -m

# 把 web/src 下的模板与 app/*.js.part 片段拼回单个 web/index.html。
# 产物同样随仓库提交（//go:embed 要它，构建镜像里不跑这一步）。
# 开发模式（AI_GATEWAY_WEB_DIR）下服务会实时拼装，改片段无需手工跑这条；
# 提交前跑一次，让产物与片段一致。
web-html:
	go run ./cmd/webbuild

# 两个前端产物一起重建。
web: web-css web-html
