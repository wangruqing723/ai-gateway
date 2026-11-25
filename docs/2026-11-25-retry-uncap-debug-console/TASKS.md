> 实现方：Codex（协作模式）

# 去掉本地重试次数上限 + 调试控制台

创建日期：2026-11-25

状态：A/B/C 三组均已验收；按使用者授权提交到 dev，不合并、不推送。。

设计见 [DESIGN.md](./DESIGN.md)，接口契约见 [API_SPEC.md](./API_SPEC.md)。

格式：`[ ] 任务名 | 优先级 | 估时 | 依赖`

## A 组：去掉本地重试次数上限

[x] A1 config：删除 `maxLocalRetries` 与上界判断，只保留非负校验，错误信息改为「不能为负数」；更新 `internal/config/local_retry_test.go`（原「11 报错」用例改为合法，补一个大数值合法用例、负数仍报错） | P0 | 0.5h | —
[x] A2 日志折叠：`RequestLog` 新增 `AttemptDetailsOmitted`；尝试明细统一走一个追加入口，头 20 + 尾 20 增量折叠；轨迹同样折叠（DESIGN §A3） | P0 | 1.5h | —
[x] A3 A2 的测试：不超过 40 条时日志 JSON 与改动前一致；超过时条数、省略计数、保留的序号、轨迹文本都正确；用一个高重试次数的候选做端到端用例 | P0 | 1h | A2
[x] A4 前端：三处输入框去掉 `max="10"`，更新提示文案；请求日志详情在省略处插入「已省略 N 条尝试明细」；重建 `index.html` | P0 | 0.5h | A2
[x] A5 文档：`config.example.yaml` 注释、`CLAUDE.md`「本地重试」条目、README 中如有相关说明一并更新 | P1 | 0.5h | A1,A2

> A 组验收记录（2026-11-25，Claude 容器内实跑）：`webbuild -check` exit=0；`go vet ./...` 干净；`go test ./...` 全绿；
> 隔离实例 + 本机 Chrome 实测 61 次尝试的请求，日志详情第 20 条后显示「已省略 21 条尝试明细」，轨迹含「…省略 21 段…」。
> 首轮交付被打回一次：新类型与既有函数 `attemptTrail` 重名导致编译失败；省略行写成 `x-for` 的第二个根节点不会渲染。
> race 检测待 `golang:1.27` 镜像就绪后与 B9 一并执行。`gofmt -l` 仅报改动前已存在的 `internal/breaker/breaker.go`。

## B 组：调试控制台后端

[x] B1 抽出推理链路：把 `server.handle()` 的推理段抽成可复用函数，带三个可选注入点（路由解析、调试标记、上游请求采集）；真实流量行为不变，现有测试不改断言全部通过（DESIGN §B2） | P0 | 2h | —
[x] B2 指标与日志：`RequestLog` 新增 `Debug` / `DebugMode`；`Collector.Add` 对调试记录只写日志不进指标；最近错误与最近图片识别失败跳过调试记录；`LogFilter.Debug` 与 `/api/logs?debug=`（DESIGN §B7、API_SPEC §2–3） | P0 | 1.5h | —
[x] B3 上游请求采集：`proxy.Options` 新增发送前回调，`forwardAttemptInput` 透传；脱敏函数与其单测（DESIGN §B6） | P0 | 1.5h | —
[x] B4 信封写入器：SSE 事件封装、UTF-8 分片安全、Flush；单测覆盖多字节字符被切断的情况（DESIGN §B3、API_SPEC §1.4） | P0 | 1.5h | —
[x] B5 入口与校验：`POST /api/debug/run` 的 HTTP 边界、严格解码、进入事件流前的三类错误（API_SPEC §1.1–1.3） | P0 | 1h | B4
[x] B6 路由模式与指定候选模式：路由不符校验、候选收窄、不轮转不写粘性（DESIGN §B4） | P0 | 1.5h | B1,B5
[x] B7 Provider 模式：隔离单发的独立执行路径；`internal/router` 导出按 Provider 构造单候选的辅助函数（DESIGN §B5） | P0 | 2.5h | B3,B5
[x] B8 后端测试（新文件 `cmd/gateway/debug_console_test.go`），至少覆盖：
    - 三种模式的成功路径，流式与非流式各一
    - 拼接全部 `response_chunk` 等于真实链路同一请求的响应正文
    - `provider` 模式：停用的 Provider 可测；熔断开路时可测且熔断状态不变；上游 5xx 不改变熔断计数；列表模式下 401/429 会换密钥但不产生冷却；没有可选密钥返回 503
    - `route` 模式：故障转移与本地重试照常；熔断照常上报
    - `target` 模式：只试所选候选；候选级覆盖值生效；不推进轮转计数器、不写粘性
    - 路由不符返回 422 且消息含实际命中的路由
    - 调试记录带标记进入 `/api/logs`，`debug=only` / `exclude` 正确；`/api/metrics` 的总数与 Provider 统计不变
    - `upstream_request` 中凭据头已脱敏，且响应里搜不到密钥原文
    - 边界：非 POST、跨站 Origin、错误的 Content-Type、未知字段、配置已变化
  | P0 | 3h | B6,B7
[x] B9 并发：`go test -race ./cmd/gateway/ ./internal/metrics/ ./internal/proxy/` 通过 | P0 | 0.5h | B8

> B 组验收：Claude 独立执行 webbuild -check、go vet ./...、go test ./... 均通过；go test -race ./cmd/gateway/ ./internal/queue/ ./internal/metrics/ ./internal/proxy/ 均通过。修复了调试队列限速窗口丢失、普通删除队列回收回归及共享组取消/超时后的回收问题。调试专用准入扩展获评审接受；调试未命中路由使用 API_SPEC 的 route_not_found，真实入口保持 gateway_error。

## C 组：调试控制台前端

[x] C1 页签与骨架：`navItems` 新增「调试」（图标 `bolt`），补齐按页签分支的标题、副标题、滚动容器；新片段 `18-debug-console.js.part` 与模板区块（DESIGN §B8） | P0 | 1h | —
[x] C2 表单：模式切换、对象选择（来自已保存配置）、客户端格式、简易表单与原始 JSON 双视图、三种格式的请求体生成、未保存修改提示 | P0 | 2.5h | C1
[x] C3 执行与事件解析：`fetch` 流式读取 SSE 信封，按事件更新状态；中止；同一时间只允许一个请求；进入事件流前的 JSON 错误展示 | P0 | 2h | C2,B5
[x] C4 响应区：概要、五个视图（渲染文本、原始响应、上游请求、尝试明细、响应头）、三种客户端格式的文本与用量提取；一律纯文本渲染 | P0 | 2.5h | C3
[x] C5 入口与日志：提供商卡片与路由行的「调试」按钮及预填；请求日志的「调试」徽标与筛选 | P1 | 1h | C2,B2
[x] C6 产物：重建 `index.html`；有新 class 时 `make web-css` 重建 `vendor/tailwind.css`；不新增图标名 | P0 | 0.5h | A4,C1–C5
[x] C7 文档：`CLAUDE.md` 的观测 API 列表与 Key Mechanisms 增加调试控制台说明；README 如有管理接口清单一并更新 | P1 | 0.5h | B8

## 验收标准

Claude 在容器内亲自执行，不采信实现方自述：

1. `go run ./cmd/webbuild -check` 退出码为 0。
2. `gofmt -l ./cmd ./internal` 无输出，`go vet ./...` 干净。
3. `go test ./...` 全绿。
4. `go test -race ./cmd/gateway/ ./internal/metrics/ ./internal/proxy/` 通过（Debian 镜像）。
5. B1 之前已存在的测试没有被修改断言（除 A1 明确要求改动的本地重试上限用例）。
6. 真实流量路径在未使用调试入口、明细不超过 40 条时，日志 JSON 与改动前逐字节一致。
7. 浏览器实测：三种模式各跑通一次流式请求，中止可用，新图标没有退化成英文文字。
8. 没有提交任何真实密钥或本地 `config.yaml`。

## 约束

- 默认不提交，改动留在工作区，由使用者决定 `git commit`。当前分支 `dev`，不 merge、不 push。
- 设计层面的疑问记入本目录的 `KNOWN_ISSUES.md`，不擅自改架构。
- Go 验证必须在容器内执行，命令见委托说明。

## 最终验收（2026-11-26）

Claude 独立重建 HTML/CSS，webbuild -check、gofmt 全目录无差异、go vet ./...、go test ./... 全通过；前端 11 项单测通过；正式产物 Chrome 验证全部通过（假接口 8 次、日志查询 5 次），覆盖三协议、草稿保护、响应五视图、日志筛选、错误、中止、桌面/移动端及无脚本异常/外部请求。B 组已完成网关、队列、指标、代理 race 验收。按使用者额外批准修复 UA 初始状态及 breaker 注释缩进。静态界面检测报告若干全页对比度/旧样式警告，未扩展为全站重设计；不将静态检测声明为全绿。
