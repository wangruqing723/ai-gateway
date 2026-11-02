# DESIGN · [1M] 后缀透传与 provider extraHeaders

> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 主实现：2026-09-10 的 `feed01b`；本次以 `46a72c6` 源码与现有测试为准。实现提交当时明确记载尚未本地验证，不篡改其历史记录；本会话主会话 Claude 的通用容器验证已补齐，见 TASKS.md。

## 背景（2026-09-10 历史记录）

用户经网关接入第三方 Anthropic Messages 兼容中转，选 1M 上下文档位时收到「1m 上下文已经全量可用，请启用 1m 上下文后重试」。当时根因在于网关剥掉 model 名尾部的 `[1m]` 标记，上游收不到它使用的 1M 识别信号，而不是本地预算未覆盖。

## 历史外部依据（原文记载已交叉核验，本次未重新核验）

下表保留 2026-09-10 的设计依据与原来源名称，**不是 2026-11-01 对外部 API 的新核验结果**。当前外部官方/中转支持情况无法仅靠仓库确认；若调整实际接入，应先对照目标上游的现行文档并验证请求。

| 当时记录的事实 | 原来源 |
|---|---|
| 官方 Anthropic API：支持 1M 的模型 1M 为默认，不需要 beta 头 | platform.claude.com/docs context-windows |
| 官方 API 不认字面 `[1m]` 后缀，model 名带后缀会 404 | anthropics/claude-code#67650、#60913 |
| 第三方中转要求 model 名带 `[1m]` 后缀才识别 1M | cc-haha#814（commit 54a8f31）、CLIProxyAPI#2525 |
| `[1m]` 是 Claude Code 客户端本地档位标记 | Bankr docs、claude-code#61068 |

原设计因此选择按 provider 声明 strip/preserve，不全局保留或全局剥除；这项网关配置契约保持不变，不将其推广为所有第三方上游的统一要求。

## 现行架构

`Candidate.TargetModel` 是候选最终发给上游的 model 名：

- 同格式透传时，`forwardAttempt` 对原 body 的拷贝写入 `upstreamMap["model"] = targetModel`。
- 跨格式时，目标格式先由 `ValidateTargetCompatibility` 预检，`forwardAttempt` 再调用对应 body builder 并传入 `targetModel`；未预检的调用仍使用 checked API。
- `router.MatchRoute` 在形成 `Candidate.TargetModel` 时按 provider 决定是否拼回 marker，所以后缀不需要在 `forwardAttempt` 再次加工。

以上是源码确认的构建链路；不能代替 preserve 档真实出网专项测试，缺口见最终验收记录。

## 数据流

```text
客户端 model="claude-sonnet-4-6[1M]"
  ↓
router.SplitOneMSuffix(model) → stripped="claude-sonnet-4-6", marker="[1M]", has=true
  ↓
router.MatchRoute 用 stripped 匹配 route
  targetModel = 显式 target.Model 或 stripped
  provider.OneMContext == "preserve" 时 targetModel += marker
  有标记时本地 ContextWindow = OneMContextWindow（1000000）
  ↓
Candidate.TargetModel → forwardAttempt
  同格式：写入 model 字段
  跨格式：格式兼容性预检通过后，body builder 使用 targetModel
  ↓
proxy.setUpstreamHeaders → proxy.ApplyExtraHeaders
  ↓
上游请求 model 使用候选最终值；extraHeaders 在网关自设头之后应用
```

## 配置契约

### Provider.OneMContext（string，omitempty）

- `""` / `"strip"`：剥后缀，不拼回；默认行为，保持向后兼容。
- `"preserve"`：将客户端原始 `[1m]` / `[1M]` marker 拼到最终 `TargetModel` 尾部。
- 其他取值由 `validateOneMContext` 拒绝，错误路径为 `providers.<name>.oneMContext`。
- `applyDefaults` 不把空串物化为 strip；这是 string 零值语义，不是 nil 指针语义。

### Provider.ExtraHeaders（map[string]string，omitempty）

- `setUpstreamHeaders` 在设置网关协议头、鉴权头与 UA 后调用 `proxy.ApplyExtraHeaders`；不能按固定「五个 Set」描述现行实现。
- helper 去头名首尾空白并跳过空名；`config.ExtraHeaderBlocked` 为配置校验和运行时 helper 共用黑名单，大小写不敏感：`x-api-key` / `authorization` / `content-type` / `user-agent`。
- 可以覆盖 `anthropic-version` / `accept`，也可以增加其他合法自定义头。
- 校验限制：条目 ≤20；头名去首尾空白后非空、≤128 rune、符合 HTTP token；值 ≤1024 rune，拒绝除 HTAB 外的 ASCII 控制字符（包括换行与 DEL）。
- 未配置 map 保持 nil；空 map 由 `omitempty` 省略，表单空行保存为 undefined，不新增空配置。
- 转发、模型查询和健康检测共用 helper。后续多密钥重构中，实际写头的位置为 `fetchUpstreamModelsOnce`、`providerhealth.checkOneRequest`；`fetchUpstreamModels` / `checkOne` 是其上层入口。

## 关键设计决策

1. **原始后缀形态保留**：实际函数为导出的 `router.SplitOneMSuffix(model string) (stripped, marker string, has bool)`。`StripOneMSuffix` 复用它，旧返回契约不变。标记前的空白仅用于容错，会被去掉，不拼回。
2. **本地窗口与 preserve 解耦**：有标记时本地预算按 1_000_000 窗口，是否给上游拼回后缀独立裁决；不改变 maxTokens 的优先级链。
3. **候选身份包含最终模型名**：`candidateKey` 使用 provider/model；preserve 档与无标记请求形成不同候选身份，可缓存会话不会被错误绑定到另一档模型。
4. **extraHeaders 不脱敏**：配置视图会明文返回它，密钥应放 `apiKey` / `apiKeys` 专用字段。表单使用独立警告文案（不是强制 placeholder），示例也提示风险。默认仅本地发布、Host/Origin 防护不能替代鉴权；无 Origin 客户端仍需外围认证与访问控制，不能以「只允许 loopback Origin」承诺该字段安全。
5. **热重载读取新 Provider 值**：YAML/JSON 解码经 `applyRuntimeConfig` 替换配置，新请求取新字段；不为这两个字段新增 Reconcile。现有健康结果缓存与 HTTP client 复用是独立机制，这不等于字段级热重载专项已覆盖。
6. **前端全量保存不丢字段**：normalize、Provider 表单重建、`configPayload()` 与 YAML 预览均保留 `oneMContext` / `extraHeaders`。

## 向后兼容

- 未配置/strip：继续剥客户端标记，不拼回；不带标记的模型名不会因本功能改变。
- 未配置/空 extraHeaders：helper 不修改 Header；nil/空 map 单测已锁定。
- 本功能的兼容承诺只针对后缀与 extraHeaders 本身。后续鉴权头、多密钥与协议演进按各自实现处理，不把当前整个请求与 `feed01b` 之前描述为逐字节完全一致。

## 最终验收与遗留迁移（2026-11-01）

- 主会话 Claude 在 `golang:1.27-alpine` 容器确认 `webbuild -check`、`go vet ./...` 退出 0，`go test -count=1 ./...` 全部通过。router / proxy 及相关包 race 通过，实际范围见 TASKS.md；本次文档子任务未重复运行。
- 已有 router 后缀/preserve/原始大小写/窗口覆盖测试、config 枚举/头安全边界/omitempty 测试、proxy header helper 测试。gateway 真实出网测试覆盖的是默认 strip + 1M 本地预算，不是 preserve。
- 未找到 preserve 真出网 body 的原始 marker 专项断言、models/health 实际收到 extraHeaders 的边界测试、两字段经配置 API 保存/重载后生效的专项回归。这些集成覆盖已迁移；`internal/config` 已于 2026-11-02 随全量竞态检测通过，不再记为待补。
- 上述测试遗留已迁移到 `docs/KNOWN_ISSUES.md` 的「历史观测与后缀功能专项回归覆盖」。任务清单中 `[x]（已迁移）` 只指移交完成，不表示测试已补或已跑；本目录无剩余归档门槛。
- 格式检查唯一既有例外 `internal/breaker/breaker.go` 与本需求及 Markdown 维护无关，不以全仓库 gofmt 无输出作完成声明。

## 不做（历史范围决策保留）

- 不新增客户端 `anthropic-beta` 透传：原设计认为该中转问题应由后缀策略解决。外部 API 当前 beta 要求本次未重新核验，不将历史理由当成现行官方结论。
- 不改 `contextWindow` 预算裁决逻辑。
- 不为 `extraHeaders` 做密钥脱敏，保留明确的明文风险提醒。
