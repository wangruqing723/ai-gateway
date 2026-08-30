# KNOWN_ISSUES：openai-responses 协议支持与尝试链观测

日期：2026-08-29
对应提交：`73e54ff`（代码）、`5cf098e`（文档）
发布版本：`v1.4.0`（镜像 digest `5d91439f39fe`，`revision=5cf098e`）

本文件只记「已知但未验证 / 已决策但有边界 / 需要人工确认」的事项。四道门（`gofmt` / `go build` / `go vet` / `go test ./...` 14 包）与 `go test -race ./...` 14 包均已全绿，不在此列。

---

## [2026-08-29] KI-1：`openai-responses` 从未对真实上游发过请求

- 发现于：`v1.4.0` 发布验收
- 问题描述：`format: openai-responses` 目前只验证到「配置能加载、格式被校验接受」——用独立探测容器（换名换端口、只读挂配置）启动，日志显示 `probe-resp: https://api.openai.com [openai-responses]`，`/health` 200，验完即删。三协议转换的正确性**全部来自单元测试**，没有任何端到端证据。
- 现网状况：线上 16 个 provider 中 15 个 `anthropic`、1 个 `openai`，**没有任何 `openai-responses` provider 在跑**，因此这条路径在生产流量里从未被执行。
- 影响面：请求体字段映射、响应体反向转换、错误语义透传三处都可能与真实 Responses 端点不一致，且单元测试的桩响应是按官方文档手写的，不是抓包。
- 状态：**已获端到端实证（2026-08-30），但未打通真实 Responses 上游**。
  - 已验证：用本地 echo 假上游（把网关实发 body 原样落盘）跑通四种组合，全部 `HTTP 200`：Anthropic 客户端→Responses 上游（非流式 + 流式）、Chat 客户端→Responses 上游、Responses 客户端→Responses 上游同格式透传。抓到的 body 确认网关打的是 `/v1/responses`（KI-1 最关心的第 1 项），`instructions` / `input[].type=message` / `max_output_tokens` / `tool_choice` 对象形式均符合协议。
  - 未验证：真实 `ar-gh`（`https://agentrouter.org`，glm-5.3）返回 `HTTP 401` + `unauthorized_client_error`（措辞为 `unauthorized client detected`）。请求已确实抵达上游（网关侧 `X-Ai-Gateway-Provider: ar-gh`、`Attempts: 1`），故**协议构建无误**。
  - 该 401 不能用于判断凭据状态：后续用**伪造 key** 复现出字节级相同的错误正文，说明上游在校验凭据之前就按客户端特征（IP / UA / 代理出口）拒绝了准入。因此既无法确认该 key 有效，也无法确认其失效——需要换一个可用的 Responses 上游才能收口本条。
  - 附带发现：容器内 `agentrouter.org` 被 DNS 污染（解析到 Meta 地址段 `31.13.95.35` / `199.96.58.177`），必须经代理才能出网——这暴露了 KI-6 记录的代理缺陷。

## [2026-08-29] KI-2：跨协议流式转换未经真实 SSE 长连接验证

- 发现于：`v1.4.0` 发布验收
- 问题描述：Responses 语义事件 ↔ Anthropic / Chat 的流式互转由 `converter.NewStreamTransformer` 逐行处理，只有单元测试覆盖（喂固定行、断言输出行）。真实上游的分块边界、事件乱序、半行到达、keep-alive 空行等情况没有实测。
- 关联未验证点：流式活跃超时后补发合规 SSE 收尾事件这条逻辑同样只有单测，没在真实长连接上触发过。
- 已有保障：发现 9 的保序修复（`textOrder` 替代 map 迭代）配 `-count=20` 跑过，能稳定复现并已修掉随机顺序；`go test -race` 覆盖 `internal/converter`。
- 状态：**事件序列已实测（2026-08-30），网络层边界仍未验证**。
  - 已验证：Responses SSE → Anthropic SSE 在真实 HTTP 长连接上跑通，客户端侧收到完整 7 事件序列 `message_start` → `content_block_start` → 2×`content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`，文本增量拼接结果正确（`1` + `7` = `17`），`stop_reason: end_turn` 正常收尾。上游侧 body 确认 `stream: true` 已透传。
  - 本轮由此发现并修掉了 KI-7 记录的 usage 丢弃缺陷——这正是「只有单测、没有真实 SSE」会漏掉的那类问题：单测此前只断言事件序列，没断言 usage 数值。
  - 未验证：真实上游的分块边界、半行到达、keep-alive 空行、事件乱序仍未实测（echo 上游一次性写完整个 SSE 载荷，不产生分块）。流式活跃超时补发收尾事件同样仍只有单测。

## [2026-08-29] KI-3：「已开始响应，未转移」标记未被真实流量触发

- 发现于：`v1.4.0` 发布验收
- 问题描述：`cmd/gateway/web/src/index.template.html:1165` 的 `x-show="detail.responseStarted && detail.outcome !== 'success'"` 标记，已确认存在于模板与构建产物（`webbuild -check` 判定一致，230720 字节），服务返回的页面里也各命中 1 次。但**没有一次真实的「响应头已写出后失败」流量**把它渲染出来看过。
- 需要的触发条件：上游 2xx 响应头之后才失败——流中途断开、流式活跃超时、或响应转换失败。这类场景无法靠正常流量等到，需要构造。
- 状态：**待人工验收**。可用一个故意在写出若干字节后断开的假上游触发；同时验证 CLAUDE.md 记录的「2xx 之后失败也要计入熔断」是否真的上报了 `breaker.Report`。

## [2026-08-29] KI-4：`isMeaningfulExtra` 收窄清单只有单测保证，真实 Codex 字段集未核实

- 发现于：发现 1–3 的修复（`internal/converter/converter.go`）
- 背景：原谓词 `exists && value != nil` 分不清「显式写了默认值」与「字段缺失」，把 `store:false`、`n:1`、`parallel_tool_calls:true` 这类等价默认值误判成不兼容而拒绝请求。修复引入 `isMeaningfulExtra`，只拦截**偏离 Responses 默认值**的字段。
- 边界：清单是按官方文档默认值手写的（`store` 默认 false、`n` 默认 1、`parallel_tool_calls` 默认 true、`stream_options` 仅 `{include_usage:true}` 视为等价）。若官方默认值变更，或某上游默认值不同，判定会失准。
- 反向保护：`TestCheckedBodiesApplyExtraFieldPolicy` 的 5 个负向子测试（`previous_response_id`、`conversation`、`background`、`store_true`、`n_大于一` 仍须拒绝）是防止**清单被过度收窄**的唯一防线，改动该清单时不得删除这组断言。
- 状态：**抓取机制已就绪，待发一次真实 Codex 请求**。
  - 机制：本轮为验证 KI-1/KI-2 搭的 echo 假上游（把收到的 body 原样落盘，脱敏 `Authorization`）就是抓取工具，不需要任何真实凭据、不出网。把 Codex 的 base URL 指向该网关、路由指向 echo provider，发一次请求即可拿到 Codex 经网关后发往上游的完整字段集。
  - 仍需人工配合的一步：由使用者用 Codex CLI 真实发一次请求（Claude 侧无法代替触发）。拿到字段集后即可把 `isMeaningfulExtra` 的清单从「按文档推断」升级为「按实测比对」。
  - 注意：抓到的是**网关出口**的字段集，不等于 Codex 原始请求体。若要看 Codex 原始形状，应看网关入口日志而非 echo dump。

## [2026-08-29] KI-5：错误正文 8 KiB 上限与 UI 复制能力的边界

- 发现于：发现 7 / 8 / 14 / 15 的修复（`internal/proxy/proxy.go`）
- 已实现的安全边界：写入请求日志的上游错误正文按 `maxObservedErrorBodyBytes = 8 << 10` 截断（`proxy.go:34`），放弃路径用该上限、保留路径用完整上限；`truncateUTF8` 改用 `strings.ToValidUTF8` 兜住半个 UTF-8 序列；脱敏已生效（`proxy_test.go:389` 断言含 `[REDACTED]` 且不含 `secret-token`）；正文只存内存环形日志，不落盘。
- 当前 UI 行为：`index.template.html:1170-1172` 用 `<pre>`（`max-h-48 overflow-auto`）展示，截断时标题显示「上游错误正文（已截断）」。**没有复制按钮**，即计划文档 §10.3 的「是否允许在 UI 中复制完整正文」保持为「不允许」。
- 状态：**已决策维持现状**，作为默认安全边界。若后续要加复制能力，需一并考虑正文可能含内部路径、提示词片段、账号标识。

## [2026-08-30] KI-6：自建 Transport 静默忽略代理环境变量（已修）

- 发现于：KI-1 端到端验证途中——容器内 `agentrouter.org` 被 DNS 污染，配了 `https_proxy` 仍拨不通
- 根因：`cmd/gateway/main.go:62` 自建 `http.Transport` 但没设 `Proxy` 字段。Go 的零值 `Transport` 语义是**完全不走代理**，只有 `http.DefaultTransport` 才默认带 `ProxyFromEnvironment`。因此 `HTTPS_PROXY` / `HTTP_PROXY` 被静默忽略。
- 影响面：该 client 是**所有出网路径的唯一出口**——请求转发（`main.go:914`）、vision 翻译（`89`）、Provider 健康检测（`1361`/`1376`）、上游模型查询（`1408`）。凡是需要代理才能出网的部署（公司网络、被污染的上游域名）全部拨不通，且报错只显示 `dial tcp ...: connection refused`，完全看不出是代理没生效。修复前全仓库零代理相关配置或文档。
- 修复：补 `Proxy: http.ProxyFromEnvironment`。不想走代理时用 `NO_PROXY` 排除，语义与 curl、docker 一致。
- 验证：探针容器带 `https_proxy` 启动后，同一请求从「拨号被拒」变为上游真实回复（`ar-gh` 返回 401 业务错误而非传输错误）。
- 残留边界：**未加单元测试**。代理行为依赖进程级环境变量，`http.ProxyFromEnvironment` 在 Go 内部对结果做了缓存，测试里改 env 不保证生效；要稳定测得起一个真实代理服务器。当前靠「一行标准库惯例 + 端到端实证」保证，改动此行时请重新做端到端验证。

## [2026-08-30] KI-7：Responses 流式丢弃上游 usage（已修）

- 发现于：KI-2 流式端到端验证——echo 上游在 `response.completed` 里给了 `output_tokens: 2`，客户端侧收到的 `message_delta` 却是 `output_tokens: 0`
- 根因：`internal/converter/stream.go` 处理 `response.completed` / `response.incomplete` 时只调 `t.finish(...)`，没把事件里的 `response.usage` 传进去，`message_delta` 的 usage 因此硬编码为 0。
- 影响面：Anthropic 协议的 `message_delta.usage` 是必有字段，客户端据此做成本核算与配额统计。恒为 0 意味着**所有经 Responses 上游的流式请求，产出 token 统计全部失真**。非流式路径不受影响（`responsesToAnthropicResponse` 正确读取了 usage）。
- 修复：新增 `responsesEventUsage`（从事件的 `response.usage` 取值）与 `anthropicStreamUsage`（折成 Anthropic 形状，只报 `output_tokens`，缺失时退回 0 保持字段恒在）。
- 验证：新增 `TestResponsesStreamCarriesUpstreamUsageToAnthropic` 三个子测试（completed 带 usage / incomplete 带 usage / 上游未给时退回 0）全绿；端到端确认客户端侧收到 `"usage":{"output_tokens":2}`。

## [2026-08-30] KI-8：两处 usage 残留边界（有意不修）

- **Chat 上游的流式 usage 仍不透传**：`stream.go` 的 `openAIToAnthropic` 同样把 `output_tokens` 写成 0。但性质与 KI-7 不同——Chat 协议里 usage 是**可选**的，只在客户端主动要 `stream_options.include_usage` 时才出现，且它到达时（空 `choices` 尾包）`message_delta` 早已发出。要修得把 `message_delta` 缓冲到流末，改动面远大于 KI-7；且网关未把客户端的 `include_usage` 意图传入转换器，无条件补一个 usage chunk 可能让严格客户端解析出错。**判断：这是「加功能」而非「修错值」，本轮不做。**
- **Responses 路径的 `message_start.input_tokens` 恒为 0**：Responses 的 `response.created` 事件本身不带 usage（输入 token 要到终态才知道），网关没有可填的真值。Anthropic 原生流式在 `message_start` 里是给 `input_tokens` 的，此处存在协议能力差异。**判断：不编造数值。**若要补，只能等到终态再回填，但 `message_start` 已经写给客户端，无法追改。

---

## 附：本次已确认无缺口的项

- 计划文档 §10.4 要求「新增 format 后配置校验、`/api/config` 脱敏回显、前端选择项、热重载、运行时 switch 同步更新」：前端三处已补 `openai-responses`（`index.template.html:780` 徽章 `OAI-R`、`1021` 注释、`1227` 下拉选项）；`internal/config/config.go` 校验接受三值并给中文错误；旧配置不写新字段时行为不变（`config_test.go` 覆盖）。
- 前端构建产物与源码一致：`webbuild -check` exit=0。此前一次「产物缺标记」的判断是本机 `grep` 对中文模式计数的行为所致，产物并未过期。

### 2026-08-30 补充（本轮端到端实证）

- **跨厂商思考块处理**：Anthropic 客户端多轮历史里的 `thinking` / `redacted_thinking` 块，转 Responses 与 Chat 上游时均被正确剥离，`signature` 一并清除；同时 `text` / `tool_use` / `tool_result` 语义完整保留（Chat 上游 body 实测：`tool_calls` 正常、`tool_result` 折成 `role: "tool"` + `tool_call_id`）。
- **客户端 `system` 角色映射**：Claude Code 直接放在 `messages` 数组里的 `role: "system"` 消息，转 Responses 时正确映射为 `role: "developer"`，指令内容完整保留（实测 body 已确认）。
- **校验网未被撕开**：放宽校验只放行思考块，未知块仍被拒——实测未知 `type` 返回 `HTTP 400` + `conversion_error`，错误信息含具体块名。
- **Chat 客户端 → Responses 上游**：`system` 折成顶层 `instructions`，tools 从 Chat 的嵌套 `function` 形状摊平成 Responses 形状，`tool_choice: "auto"` 透传。
- **Responses 客户端同格式透传**：强制 `tool_choice` 对象 `{"type":"function","name":"calc"}` 与 `developer` 角色均原样保留，未被规范化改写。
