# DESIGN · [1M] 后缀透传与 provider extraHeaders

## 背景

用户经网关接入第三方中转（类 Anthropic Messages 协议），选 1M 上下文档位时上游报「1m 上下文已经全量可用，请启用 1m 上下文后重试」。根因不在 `contextWindow` 配置，也不在 `anthropic-beta` 头，而在：**网关 `StripOneMSuffix` 剥掉了 model 名尾部的 `[1m]` 标记，上游收不到它赖以识别 1M 的信号。**

## 事实依据（已交叉核验）

| 事实 | 来源 |
|---|---|
| 官方 Anthropic API：支持 1M 的模型 1M 为默认，不需要 beta 头 | platform.claude.com/docs context-windows |
| 官方 API 不认字面 `[1m]` 后缀，model 名带后缀会 404 | anthropics/claude-code#67650、#60913 |
| 第三方中转要求 model 名带 `[1m]` 后缀才识别 1M | cc-haha#814（commit 54a8f31）、CLIProxyAPI#2525 |
| `[1m]` 是 Claude Code 客户端本地档位标记 | Bankr docs、claude-code#61068 |

结论：官方 API 必须剥后缀（否则 404），第三方中转必须保留后缀（否则报「请启用 1m」）。故不能无脑全剥或全留，必须按 provider 可控。

## 架构方案

**核心洞察**：`Candidate.TargetModel` 是发给上游的 model 名唯一出口。
- 透传路径 `main.go:1001` `upstreamMap["model"] = targetModel`
- 跨格式路径 `main.go:1016-1022` `ToXxxBodyChecked(in.internal, targetModel)`

只要在 `router.MatchRoute` 组装 `Candidate.TargetModel` 时按 provider 决定是否拼回后缀，`forwardAttempt` 及下游全部自动生效，一行不用改。

## 数据流

```
客户端请求 model="claude-sonnet-4-6[1M]"
  ↓
router.MatchRoute:
  stripped, marker, has := splitOneMSuffix(model)   // marker="[1M]" 原始形态
  route match 用 stripped                           // 路由匹配不受影响
  for each target:
    targetModel := target.Model 或 stripped          // 现行逻辑
    if has && provider.OneMContext=="preserve":
        targetModel += marker                        // ← 新增：拼回原始后缀
    Candidate{TargetModel: targetModel, ...}
  ↓
forwardAttempt:
  isPassthrough: upstreamMap["model"] = targetModel   // 自动带后缀
  跨格式:    ToAnthropicBody(in, targetModel)        // 自动带后缀
  ↓
setUpstreamHeaders + extraHeaders
  ↓
上游收到 model="claude-sonnet-4-6[1M]"  ✓
```

## 配置契约

### Provider.OneMContext（string，omitempty）
- `""` / `"strip"`：现行行为，剥后缀不拼回（默认，向后兼容）
- `"preserve"`：拼回客户端原始后缀形态到 TargetModel
- 校验：其他值报错，字段路径 `providers.<name>.oneMContext`

### Provider.ExtraHeaders（map[string]string，omitempty）
- 应用顺序：在 `setUpstreamHeaders` 现有五个 `Set` 之后遍历写入
- 黑名单（不区分大小写，双保险：validate + ApplyExtraHeaders）：`x-api-key`/`authorization`/`content-type`/`user-agent`
- 允许覆盖：`anthropic-version`/`accept`/任意自定义头（如 `anthropic-beta`）
- 限制：key 非空；value ≤ 1024 rune；条目数 ≤ 20
- 应用范围：三条出网路径（转发 `setUpstreamHeaders`、`fetchUpstreamModels`、`providerhealth.checkOne`）共用 helper `proxy.ApplyExtraHeaders`

## 关键设计决策

1. **后缀原始形态保留**：`splitOneMSuffix` 返回客户端原始书写的 marker（`[1m]` 或 `[1M]`），不归一化。原因：中转站识别逻辑未知，最大保真。
2. **contextWindow 覆盖与 preserve 解耦**：`hasOneM` 时仍把 contextWindow 覆盖为 1_000_000（现行逻辑），与 preserve 无关。本地预算裁决与上游后缀识别是两件事，不耦合。
3. **粘性键影响**：`candidateKey`（main.go:916）用 `Provider.Name + "/" + TargetModel`。preserve 下带后缀的请求与不带后缀的会算出不同粘性键——这是期望行为（1M 会话与普通会话分别粘性），无需处理。
4. **extraHeaders 不脱敏**：`/api/config` 的 `configViewSnapshot` 不对 extraHeaders 脱敏。非密钥字段（密钥走 apiKey）。前端 placeholder 明确提示「不要放密钥」。**已知风险**：用户若误把 token 写进 extraHeaders，`/api/config` GET 会明文返回。缓解：placeholder + 文档提示。可接受，因 `/api/config` 仅 loopback Origin。
5. **热重载无新代码**：`oneMContext`/`extraHeaders` 随 YAML 解码 + `applyRuntimeConfig` 自动生效，不需 Reconcile（每次请求读 Provider 字段）。

## 不做（明确排除）

- 不透传客户端 `anthropic-beta` 头：核验结论官方 1M 已 GA 不需要 beta 头，中转要的是 model 后缀。透传不对症。
- 不改 `contextWindow` 预算裁决逻辑。
- 不为 `extraHeaders` 做密钥脱敏。

## 向后兼容

- `oneMContext` 未配置/`strip`：行为与改动前逐字节一致（后缀剥掉不拼回）。
- `extraHeaders` 未配置/空：`setUpstreamHeaders` 行为不变（nil map 遍历为空操作）。
- `StripOneMSuffix` 旧契约不破（改为基于 `splitOneMSuffix` 实现，返回值不变）。
