> 实现方：Claude（文档模式）

# TASKS.md：tool_reference 降级补齐 + znrx 400 试探开关

需求目录：`docs/2026-10-19-tool-reference-and-znrx-probe/`
创建日期：2026-10-19

## 背景一句话

两个独立问题：

1. **`tool_reference` 在 openai-responses 路径漏接降级** —— Claude Code 多轮历史里的 `tool_result.tool_reference` 占位块，OpenAI Chat 路径已降级成文本，openai-responses 路径仍直接报错，导致该候选被跳过（`ar-ld` / `shangtang` 等 responses 上游反复命中）。
2. **znrx 的 `glm-5.3` 稳定返回 400** —— 上游错误体是不透明的 `bad_response_status_code`，不指向字段；现有 100+ 条 400 样本覆盖了 `stream` 真假、长/短消息、有无 `context_management` 的组合全部失败，唯一成功样本未落盘，**根因未坐实**。本次只落地「让成功样本也落盘」的调试开关，znrx 的具体修复待试探结论后另行实施。

## 任务清单

- [x] T1 落地 znrx 试探开关（成功请求体也落盘） | P0 | 0.5h | 无
- [x] T2 修复 responses 路径的 tool_reference 降级 | P0 | 1h | 无
- [x] T3 用新开关抓 znrx 成功样本并对比 400 样本，定位真因 | P0 | 1h | T1
- [x] ~~T4 按 T3 结论实施 znrx 修复~~ **已关闭**：T3 证明 400 与请求体无关，无可修点 | P1 | — | T3

> T4 由 T3 结论关闭，详见 DESIGN.md 第四节。不做任何 znrx 请求体改写。

## 实施记录（2026-10-19）

### T1 已完成

- `cmd/gateway/main.go`：新增 `shouldCaptureUpstreamBody(upstreamStatus int) bool`，只按状态码与
  `AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL` 判定；两个调用点由 `>= 400` 改为走该 helper。
- 职责分离：判定层只管「要不要抓」，「抓到哪个文件」由 `captureUpstreamBody` 内的
  `AI_GATEWAY_DEBUG_UPSTREAM_BODY == ""` 提前返回兜底。
- `cmd/gateway/main_test.go`：`TestShouldCaptureUpstreamBodyGatesOnEnvAndStatus`（表驱动）+
  `TestCaptureUpstreamBodySkipsWhenNoOutputPath` + `TestCaptureUpstreamBodyAllEnvCapturesSuccess`（端到端）。

### T2 已完成

- `internal/converter/converter.go`：
  - `validateResponsesToolResultContent` 的 switch 增加 `case "tool_reference", "tool-reference":` 放行。
  - `responsesContentFromCanonical` 的 switch 增加同款分支，复用 `toolReferenceText` +
    `responsesTextPart` 降级成文本。
- `internal/converter/converter_test.go`：`TestResponsesToolResultToolReferenceDegradesToText`（表驱动，5 档）、
  `TestResponsesToolResultToolReferenceMixedWithText`、`TestResponsesToolResultUnknownBlockStillRejected`。
- 未改动 `responsesContentFromCanonical` 对未知块的「静默丢弃」语义（`default` 无分支），
  避免把现有的宽容行为变成硬失败。

### 验收实跑输出（2026-10-19）

```
ok  	ai-gateway/cmd/gateway	0.697s
ok  	ai-gateway/internal/balancer	0.008s
ok  	ai-gateway/internal/breaker	0.006s
ok  	ai-gateway/internal/config	0.119s
ok  	ai-gateway/internal/converter	0.080s
ok  	ai-gateway/internal/httpclient	0.004s
ok  	ai-gateway/internal/metrics	0.010s
ok  	ai-gateway/internal/providerhealth	0.105s
ok  	ai-gateway/internal/proxy	1.238s
ok  	ai-gateway/internal/queue	0.703s
ok  	ai-gateway/internal/router	0.005s
ok  	ai-gateway/internal/tokenest	0.003s
ok  	ai-gateway/internal/vision	0.257s
ok  	ai-gateway/internal/webbuild	0.011s
```

- `go vet ./...`：无输出（通过）。
- `gofmt -l ./cmd ./internal`：本次改动的 4 个文件无输出；唯一报出的
  `internal/breaker/breaker.go` 经 `git diff` 确认不在改动范围内，是既有格式差异。
- race 未跑：本地无 gcc（仅 alpine 镜像），且本次改动不涉及并发/共享状态，不适用该检查。

### 验证环境备注

本地无 `golang:1.27-alpine` 镜像（仅有 `golang:1.26-alpine`，`go.mod` 要求 `go 1.23`，满足）。
容器内默认 `proxy.golang.org` 不可达，需 `GOPROXY=https://goproxy.cn,direct` 才能取依赖。

## T5 附加项：dev 调试落盘隔离（2026-10-19 追加）

T3 实测时发现 dev 容器与线上容器**共写同一份 `./data`**，导致：

- 两套容器的调试记录混在同一 JSONL；
- 两个容器的 `reqID` 各自从 `r00001` 计数、**会重复**，按 reqID 取样本会拿到另一个容器的
  请求体（实测踩过，改用 `time` 时间窗才对准）。

改动：

- `docker-compose.dev.yml`：新增 `./data/dev:/app/data-dev` 挂载；三个
  `AI_GATEWAY_DEBUG_UPSTREAM_*` 全部改指 `/app/data-dev`。
  **注意基础配置继承来的 `AI_GATEWAY_DEBUG_UPSTREAM_BODY` 也必须显式覆盖**——它原本指向
  `/app/data`，漏改的话 dev 的 4xx/5xx 仍会写进线上那份文件。
  `./data:/app/data` 保持不变，ChatGPT OAuth 账户数据继续共享（有意设计）。
- `docker-compose.dev.yml`：顶部补「与线上镜像的区别」对照表（镜像名 / 容器名 / 端口 /
  compose 项目名 / 前端加载方式 / 调试落盘位置）。
- `scripts/dev.sh`（新增）：`up` / `down` / `clean` / `logs` 四个子命令。
  `down` = 停容器 + 清空 `./data/dev/*.jsonl`；`clean` 只清日志。
  只删 `.jsonl`，不碰目录下其它文件。

验收实跑：

- 隔离验证：dev 发一个请求后 `data/dev/upstream_debug.jsonl` 新建（257 B），
  线上 `data/upstream_debug.jsonl` 保持 2057833553 B、mtime 未变。
- 清理验证：`down` 后 `data/dev` 为空，线上两个文件未被触碰。
- 边界验证：目录空 / 目录不存在时跳过清理并 exit 0；用法错误 exit 1。
- 运行态：dev 容器挂载含 `data/dev -> /app/data-dev`，三个 debug 变量全指向 `/app/data-dev`；
  线上容器变量仍全指向 `/app/data`。

## 验收标准

### T1
- 新增环境变量（暂定 `AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL`），非空时**所有**转发尝试的请求体都落盘，不限于 4xx/5xx。
- 默认（未设置）时行为与现在**逐字节一致**：只抓 4xx/5xx，性能与磁盘占用零变化。
- 落盘记录结构复用 `captureUpstreamBody` 现有 JSONL 字段（`time/reqID/provider/targetModel/upstreamStatus/body`），保证同一文件里新旧样本可一起 grep。
- 单元测试覆盖：开关开启时 2xx 也落盘、关闭时 2xx 不落盘（在 `cmd/gateway/main_test.go` 现有的 `AI_GATEWAY_DEBUG_UPSTREAM_BODY` 测试旁补）。
- 文档记录：开关用途、开启方式、用完必须关闭（会写爆磁盘）。

### T2
- `validateResponsesToolResultContent` 放行 `tool_reference` / `tool-reference`，与 `validateOpenAIChatToolResultContent` 的既有放行口径一致。
- `responsesContentFromCanonical` 增加 `tool_reference` 分支，降级成等价文本（复用 `toolReferenceText`），不再静默丢弃。
- 校验与构建两侧**必须同时改**：只放行校验会让 `tool_reference` 被静默丢弃（内容丢失），只改构建则校验先报错、构建根本跑不到。
- 单元测试覆盖：`tool_reference` 单块、与文本混排、带 `toolName`/`tool_name`/`name`/无名字段、连字符形式 `tool-reference`；并锁定「非 tool_reference 的未知块仍然报错」不回退。
- 现有 `TestToolResultToolReferenceDegradesToText` 等 chat 路径测试保持通过（不得为改 responses 路径而改动 chat 路径行为）。
- `go test ./...` 全绿；`go vet ./...` 无输出；`gofmt -l` 无输出。

### T3
- 至少抓到 1 条 znrx `glm-5.3` 的 **2xx 成功**请求体，与已落盘的 400 请求体做字段级 diff。
- 结论必须落在具体字段或明确给出「非请求体因素」的判断，不得只给猜测。
- 若 diff 无差异（请求体完全相同却一成一败），结论要明确写出「400 由上游侧状态决定，网关侧无可修点」，并据此关闭 T4。

## 约束

- 全程不改 znrx 的请求体构造逻辑——在 T3 结论出来前，任何针对 znrx 的字段删除/过滤都是猜测。
- 不提交 `config.yaml`、不把 API Key 写进任何文档或提交。
- 现有 `AI_GATEWAY_DEBUG_UPSTREAM_BODY` / `AI_GATEWAY_DEBUG_UPSTREAM_STREAM` 的语义不变。
- 前端无改动，不涉及 `webbuild`。
