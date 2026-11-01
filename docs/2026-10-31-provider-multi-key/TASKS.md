> 实现方：Codex（协作模式）

# Provider 多密钥与 Key 切换

设计见 [DESIGN.md](DESIGN.md)，接口见 [API_SPEC.md](API_SPEC.md)，术语见仓库根 `CONTEXT.md`「上游与凭据」。所有取舍已与使用者确认，实现时不得改动；冲突记 `KNOWN_ISSUES.md`。

## 任务

[x] T1 config：`ProviderKey` / `KeyCooldown` 类型、`Provider.APIKeys` / `KeyCooldown` 字段、accessor、`validateProvider` 全部校验规则（互斥、上限 10、身份格式与唯一、值非空无控制字符且不重复但跳过 sentinel、keyCooldown 范围与前置条件、OAuth 冲突）；导出 `ValidateProviderKeyValues`；`applyDefaults` 不物化新字段 | P0 | 1.5h | -
[x] T2 keypool：新包 `internal/keypool`，实现 DESIGN §4 全部语义（状态键、credHash / connFP、Reconcile、Ticket 版本校验、Select 与最早恢复时间、按状态码分类、冷却时长与 Retry-After、`max` 截止、Release、事件去重与 `abnormalSeq` 恢复判定、Snapshot、导出唯一的 `Fingerprint`）；可注入时钟 | P0 | 2.5h | T1
[x] T3 管理 API 机密：`RedactYAML` 只脱敏 `apiKeys[*].value`；`configViewSnapshot` / `handleGetConfig` 输出 sentinel、`valueConfigured`、`fingerprint`；`handlePutConfig` 按 Provider 名 + 身份回填（含旧单值转列表至多一条继承）并复查重复；启动横幅只打印数量与指纹；`cmd/gateway` 指纹改用 `keypool.Fingerprint` | P0 | 1.5h | T1, T2
[x] T4 主转发 Key 切换：候选检查顺序（无可选择密钥在熔断前）、候选内 Key 切换循环、ShouldRetry 判定次序、列表模式 401/402/403 不本地重试、只剩一个密钥的短暂 429 例外、冷却上报时机、Key 切换链共用超时（排队不扣、剩余 <1s 不再切换）、按候选汇总 freeAttempt、终态 503 `no_selectable_provider_key` 与优先级、`AttemptDetail` 新字段与 trail、绝不回退客户端密钥 | P0 | 4h | T2
[x] T5 server 接线：构造 keypool；`applyRuntimeConfig` 调 `Reconcile`；事件回调写 `eventLog`（`key_cooldown` / `key_recovered`，`EventLog.Add` 识别恢复）；`/health` 增 `providerKeys`；新增 `POST /api/providers/keys/cooldown/reset`（同源校验）；熔断重置不清冷却 | P0 | 1h | T2, T4
[x] T6 图片识别：`vision.Translator` 注入 keypool；列表模式不调 `ResolveAPIKey`；按 DESIGN §6 切换、冷却上报、每次换 Key 重新排队、共享 120s 预算；无可选择密钥归 `other` 并写明原因 | P1 | 1.5h | T2, T5
[x] T7 健康检测 / 模型查询 / 连接测试：`providerhealth` 多密钥（遵守冷却不更新、健康状态表、`providerFingerprint` 纳入密钥列表）；`fetchUpstreamModels` 多密钥与 503；`/api/providers/probe` 支持 `apiKeys` + `testKeyId`、sentinel 回填、错误消息脱敏全部密钥 | P1 | 2h | T2, T3
[x] T8 网页 Provider 编辑：密钥列表（增、替换、删、停用、拖拽排序、单行测试、冷却原因 / 剩余时间与解除冷却）、身份随机生成且只读、只显示指纹、冷却时长输入仅列表模式、全部停用提示；旧单值写法未改动保存仍为 `apiKey`，改动后转 `apiKeys` 并提示；`configPayload` 剔除只读字段；复制不复制密钥；切 OAuth 清空列表；表格徽标显示启用数 / 总数 / 冷却数 | P1 | 4h | T3, T5, T7
[x] T9 网页观测：日志详情展示每次尝试的密钥名称 / 身份 / 指纹与 `key_switched` / `key_skip`；运行异常区渲染两种新事件 | P1 | 1h | T4, T5
[x] T10 前端产物：`go run ./cmd/webbuild` 重建 `cmd/gateway/web/index.html`，`-check` exit 0；尽量复用既有 class，若必须新 class 记 KNOWN_ISSUES | P0 | 15m | T8, T9
[x] T11 文档：`config.example.yaml` 增 `apiKeys` / `keyCooldown` 示例与注释（`example_config_test` 须通过）；README 增多密钥说明与「合并重复 Provider」手工步骤；CLAUDE.md「Request Flow」第 8 步与「Key Mechanisms」补 Key 切换、密钥冷却、终态优先级 | P1 | 1h | T1-T9
[x] T12 验证：容器内 gofmt / vet / test / race / webbuild -check 全部通过并贴完整输出；`httptest` 假上游覆盖 TaBi 场景（主密钥 401、备用 200，一次请求成功且 Attempts=1） | P0 | 45m | T1-T11

## 验收要点（每项须有对应测试）

- **T1**：table-driven 覆盖每条校验错误；旧单值写法与 `apiKeys: []` 解析结果与改动前一致；YAML 往返不新增字段。
- **T2**：冷却开始前发出、之后成功的 ticket 不清冷却也不记恢复；旧 credHash ticket 上报被丢弃；connFP 变化清空、改名 / 排序 / 停用保留；`Retry-After` 长于默认值时采用上游值；配置 0 关闭默认冷却但仍遵守 `Retry-After`；改冷却时长不回改已有截止；`go test -race` 通过。
- **T3**：raw YAML 中 `extraBody` 里名为 `value` 的键不被替换；sentinel 指向不存在身份返回 400；旧单值转列表时两条 sentinel 返回 400；回填后重复值返回 400。
- **T4**（`cmd/gateway` 集成测试，`httptest` 假上游）：主 401 → 备用 200 成功且 `Attempts=1`、明细 2 条、主密钥进入冷却；全局 failover 关闭仍切换；5xx 不切换；全部密钥 401 后故障转移到下一个候选；单候选全部失败返回最后一次原始响应；只剩一个密钥的短暂 429 走本地重试且重试期间不冷却；长 `Retry-After` 429 切换且按候选汇总 freeAttempt；全部冷却跳过返回 503 + `retry-after` 且不调用熔断 `Allow`；流式预读阶段等效 429 切换、提交后不切换；Key 切换链超时共享；不读取客户端 `x-api-key`。
- **T6 / T7**：图片识别主密钥 429 换备用成功并冷却主密钥；健康检测四种情形；模型查询全部冷却返回 503；probe 带 `testKeyId` 只请求该密钥且不改冷却。
- **T8 / T9**：前端改动后 `webbuild -check` exit 0，`TestRepoArtifactMatchesSources` 通过。

## 委托约束

- 命令一律在容器内跑，验证容器名 `ai-gateway-dev-verify`，需带 `GOPROXY=https://goproxy.cn,direct` 与 module cache 卷：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work -e GOPROXY=https://goproxy.cn,direct golang:1.27-alpine sh -c 'gofmt -l ./cmd ./internal; go vet ./... && go test ./... && go run ./cmd/webbuild -check'
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -v ai-gateway-gomod-deb:/go/pkg/mod -w /work -e GOPROXY=https://goproxy.cn,direct golang:1.27 go test -race ./internal/keypool/ ./cmd/gateway/ ./internal/vision/ ./internal/providerhealth/
```

- 不提交、不 push、不改本地 `config.yaml` / `.env`；不读取或输出真实密钥。
- 不改 DESIGN / API_SPEC 的取舍；发现设计问题写 `KNOWN_ISSUES.md`。
- 新注释、日志、错误信息用中文，遵循 CLAUDE.md 代码风格。

## 验证记录

### 2026-10-31 第一批（T1–T5）验收：未通过，待修

Codex 第一轮完成 T1–T5 主体后因上游 504 中断；续接修复的第二轮同样 504，未产生改动。Codex 沙箱无 docker，以下结果均为 Claude 在容器内实跑。

- `go vet ./...`：通过。
- `go test ./...`：除 `cmd/gateway` 外全部 ok；`cmd/gateway` 卡满 600s 超时。
- `go test ./cmd/gateway/ -skip "TestKeySwitchChainSharesUpstreamTimeout|TestFrontendCoversAllProviderFields"`：ok。
- `TestFrontendCoversAllProviderFields` 失败：前端尚未同步 `apiKeys` / `keyCooldown`，属 T8，预期内。
- `gofmt -l`：本需求新增未格式化文件 5 个（`internal/breaker/breaker.go` 为既有问题，不在本需求内）。

待修（T1–T5 勾选前必须完成）：

1. `TestKeySwitchChainSharesUpstreamTimeout` 挂死：假上游不读请求体就阻塞在 `<-r.Context().Done()`，感知不到客户端断开，`defer upstream.Close()` 永久等待。handler 先读完请求体，再用 `select` 加 5s 兜底；断言以总耗时 < 3s 与 `hits == 2` 为主。
2. `forwardAttempt` 的链预算覆盖误伤旧单值写法：直通流式的 `TimeoutMs` 被改为 `DirectTimeoutStreamHeader`。只在列表模式覆盖；流式只覆盖 `headerTimeoutMs`，非流式只覆盖 `timeoutMs`。
3. `keypool.Report`：冷却时长为 0 时不冷却也不记 `key_cooldown`，但仍设置 `abnormalSeq`，之后成功会记出不成对的 `key_recovered`。只有实际进入冷却才更新 `abnormalSeq`。
4. Key 切换分支的 `abandonFree` 只能在 429 且超过 `maxRetryAfterMs` 时为 true，401 / 402 / 403 带 `Retry-After` 不得免额。
5. 候选内切换后 `Select` 因竞态返回 nil 时，循环外又追加一次 `outcome.trail`，轨迹重复。
6. `gofmt -w`：`cmd/gateway/main.go`、`cmd/gateway/provider_keys_test.go`、`internal/config/config.go`、`internal/config/provider_keys_test.go`、`internal/keypool/keypool.go`。

### 2026-10-31 第一批（T1–T5）复验：通过

第三轮（新线程）Codex 完成第 1–5 条；Codex 本机无 gofmt、沙箱无 docker，第 6 条由 Claude 在容器内执行 `gofmt -w`（只改格式）。以下均为 Claude 在容器内实跑：

- `gofmt -l ./cmd ./internal`：仅剩 `internal/breaker/breaker.go`（既有问题）。
- `go vet ./...`：通过。
- `go test ./...`：除 `TestFrontendCoversAllProviderFields`（T8 预期内）外全部 ok，`cmd/gateway` 3.1s，不再挂死。
- 新增测试逐条 PASS，含 `TestKeySwitchChainSharesUpstreamTimeout`（2.50s）、`TestKeySwitchRetryAfterOnlyFreesLong429`、`TestZeroAuthCooldownDoesNotCreateRecoverySequence`、旧写法超时参数表测试。
- `golang:1.27` 下 `go test -race ./internal/keypool/ ./internal/config/ ./internal/metrics/ ./cmd/gateway/ -skip TestFrontendCoversAllProviderFields`：全部 ok。

### 2026-10-31 第二批（T6–T7）验收：通过

Codex 一次完成，未运行测试；Claude 在容器内 `gofmt -w` 本批新增/改动文件（只改格式）后实跑：

- `gofmt -l`：仅剩 `internal/breaker/breaker.go`（既有问题）。
- `go vet ./...`：通过。
- `go test ./...`：除 `TestFrontendCoversAllProviderFields`（T8 预期内）外全部 ok。
- 新增 12 个测试全部 PASS（图片识别 2、健康检测 4、模型查询 2、连接测试 4）。
- `golang:1.27` 下 `go test -race ./internal/vision/ ./internal/providerhealth/ ./internal/keypool/ ./cmd/gateway/ -skip TestFrontendCoversAllProviderFields`：全部 ok。
- 代码审查：图片识别每次换 Key 重新排队、列表模式不解析客户端密钥；健康检测与模型查询遵守冷却不调用 Report；连接测试 testKeyId 只测该密钥且不读写冷却；错误消息替换全部密钥值。

### 2026-10-31 第三批（T8–T11）验收：待修 2 项

Codex 完成前端 T8–T9 与文档 T11，未新增 Tailwind class；Claude 在容器内执行 `go run ./cmd/webbuild` 重建产物（T10）后实跑：

- `go run ./cmd/webbuild -check`：exit 0。
- `go vet ./...`：通过；`go test ./...`：全部 ok，含 `TestFrontendCoversAllProviderFields`、`TestFrontendSentinelLiteralsMatchBackend`、`TestRepoArtifactMatchesSources`。
- `golang:1.27` 下 `go test -race ./...`：全部 ok。
- 产物内联脚本语法检查：0 错误。
- `TestExampleConfigPassesStrictDecode`：通过。
- 冒烟（容器内假上游 + 假配置，端口 17789）：主密钥 401、备用 200，一次请求成功且 `x-ai-gateway-attempts: 1`；第二次请求直接走备用；`/health` 显示主密钥 `cooling / auth / 401`；运行事件记 `key_cooldown`；模型查询跳过冷却密钥；网关日志无密钥明文。
- 浏览器检查（本机 Chrome for Testing 经 CDP，不装第三方包）：表格徽标、抽屉指纹与冷却原因、解除冷却按钮、停用后保存、保存后值保留、旧写法未改动保存仍为 `apiKey`、改动后转 `apiKeys` 并继承旧值、日志详情密钥名称 / 身份 / 指纹、运行异常区「密钥冷却」均通过，页面无 JS 异常。

待修：

1. 本来就是列表模式的 Provider，编辑抽屉也显示「保存后将使用 apiKeys 列表，并启用密钥冷却」。按 DESIGN §8，该提示只在旧单值写法（或新建 Provider）转为列表时出现。
2. README「功能实现状态」的 Provider 多密钥一行仍写「开发中 / 网页管理正在实现」，应改为已实现。

### 2026-11-01 最后收尾复验：源码已验收，产物重建受权限阻断

- Codex 已修复第三批的两项问题：原本就是列表模式的 Provider 不再显示转换提示；README 功能状态已更新为「已实现」。
- 在独立临时实例 17790 上用开发模式实时拼装最新前端源码复验：已有列表 Provider 打开、改名、新增密钥均不显示转换提示；旧单值、新建、复制的 Provider 改成列表后才显示提示。九种情形全部通过，页面无 JS 异常。
- 最新源码浏览器复验全部通过：密钥指纹、冷却原因、解除冷却、启停后保存、旧单值保存兼容、转换后继承旧密钥、日志密钥明细、运行异常区。
- 最新源码的 `go vet ./...` 退出码为 0。
- 最新源码的 `go test -count=1 -timeout 300s ./... -skip TestRepoArtifactMatchesSources` 全部通过。只跳过了被产物重建阻断的那一项，没有跳过业务测试。
- `CGO_ENABLED=0 go build` 通过；前端字段覆盖与 sentinel 守护测试再次通过。
- `gofmt -l ./cmd ./internal` 只列出未改动的既有文件 `internal/breaker/breaker.go`，本需求的所有改动都已格式化。
- 全量竞态检测在第三批收尾前已通过；最后两项只改了前端转换提示和 README，Go 源码未变。
- 临时测试容器 17789、17790 已停止；正式服务 7789 未重启，真实配置未修改。

**流程阻断**：权限层拒绝了用 `go run ./cmd/webbuild` 覆盖 `cmd/gateway/web/index.html`。没有改用其他工具覆盖该文件，也没有委托其他会话绕过这次拒绝。

当前 `webbuild -check` 的真实输出：

```text
cmd/gateway/web/index.html 与 src/ 下的源码不一致（产物 673272 字节，重新拼装 673511 字节）。
请运行 `make web-html` 后提交产物。
exit status 1
```

T8、T9、T11 已勾选。T10、T12 保持未勾选，等权限问题处理后重建最终产物，再完整复跑测试与产物检查。

### 2026-11-01 最终验收：通过

使用者亲自运行 `go run ./cmd/webbuild` 重建了产物（673511 字节）。随后 Claude 在容器内实跑：

- `go run ./cmd/webbuild -check`：与源码一致，exit 0；产物中包含 `keyListOriginallyConfigured` 修复。
- `gofmt -l ./cmd ./internal`：只列出既有文件 `internal/breaker/breaker.go`。
- `go vet ./...`：exit 0。
- `go test -count=1 -timeout 300s ./...`：全部 ok，含 `TestRepoArtifactMatchesSources`，无跳过项。
- `golang:1.27` 下 `go test -race -count=1 ./...`：全部 ok，无数据竞争。
- TaBi 场景 `TestKeySwitchIgnoresFailoverAndClientAPIKey`：PASS，主密钥 401、备用 200，一次请求成功且 Attempts=1。

T1–T12 全部完成。
