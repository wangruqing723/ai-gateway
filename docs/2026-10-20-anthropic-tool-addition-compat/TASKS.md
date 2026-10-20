> 实现方：Codex

# TASKS.md：Anthropic 工具变更块的跨协议兼容

需求目录：`docs/2026-10-20-anthropic-tool-addition-compat/`
创建日期：2026-10-20

## 背景

`r00148` 对应的 Claude Code 请求启用了 `mid-conversation-tool-changes-2026-07-01`，历史消息中含有 `tool_addition` 块；它引用的 Firecrawl 工具定义已经在请求顶层 `tools` 中。当前 Responses 目标校验器把该块视为不支持的内容类型。转换发生在每个候选的 `forwardAttempt` 内，因此相同协议不兼容会被重复记成多个候选构建失败，实际没有发出上游请求。

本需求需要同时解决两个层面：按协议语义处理工具变更元数据；把与候选上游无关的转换兼容性检查前移，避免同一输入错误被重复尝试。

## 任务清单

- [x] T1 将 `tool_addition` 识别为工具可用性元数据，校验其引用的工具名存在于规范化后的顶层工具定义，并在 OpenAI Chat / Responses 请求中省略该消息块、保留顶层工具定义 | P0 | 1h | 无
- [x] T2 建立统一的目标格式兼容性预检：按候选目标格式检查一次；不兼容的格式跳过且不消耗上游尝试额度；若没有兼容候选，返回一次 `conversion_error`，不进入上游转发 | P1 | 2h | T1
- [x] T3 为已支持的元数据、未声明的工具引用、未知语义块和混合格式候选补回归覆盖，确保未知块仍明确失败且不被静默丢弃 | P0 | 1.5h | T1、T2
- [x] T4 检查候选日志和最终错误分类，确认本地转换失败不会被记录成上游失败或重复消耗故障转移额度 | P1 | 1h | T2

## 验收标准

- `tool_addition` 的工具名不写死：任何引用只要能匹配请求顶层工具定义，就按同一规则处理。
- OpenAI Responses 请求保留顶层完整工具 schema，生成的 `input` 不包含 `tool_addition` 内容块。
- 未声明的引用返回可定位的转换错误；未知且可能承载语义的块仍报错，不做通用丢弃。
- 候选格式兼容性只需检查一次；若有兼容格式，仍可按既有候选顺序和故障转移规则尝试；若所有可用格式均不兼容，则只返回一次请求级 `conversion_error`，不发送上游请求。
- 同格式透传行为保持不变。
- 新增针对性回归测试并运行相关 converter / gateway 测试。

## 约束

- 不按 `mcp__firecrawl__firecrawl_scrape` 或 `mcp__firecrawl__firecrawl_search` 逐个加特例。
- 不把未知块统一转成文本或静默删除；只有语义已明确且目标映射已实现的块才能转换或省略。
- 不更改故障转移对真实上游状态码的既有判定。
- 不提交真实 API Key、Claude 会话日志或完整私有提示词。

## 实现与验证记录

- 2026-10-21：已实现按唯一目标格式兼容性预检、`tool_addition` 引用校验与跨协议投影；补充多工具引用、未声明/结构未知引用、混合候选、全格式失败、429 终态和同格式透传回归。
- 通过：`docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.27-alpine go test ./internal/converter -count=1`。
- 网关定向测试未能完成：容器无法从 `https://proxy.golang.org` 下载 `modernc.org/sqlite@v1.34.4` 与 `gopkg.in/yaml.v3@v3.0.1`，连接超时（`dial tcp ...:443: i/o timeout`）。恢复 Go module proxy 网络后，可复跑：
  `docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.27-alpine go test ./internal/converter ./cmd/gateway -run 'TestAnthropicToolAddition|TestCompatibilityPreflight|TestIncompatibleLaterCandidate|TestToolAdditionSameFormatPassthrough|TestInferenceRejectsRequestConversionError|TestInferenceRejectsTargetSpecificToolOutput|TestSameFormatRequestPreservesNativeExtensions' -count=1`。
