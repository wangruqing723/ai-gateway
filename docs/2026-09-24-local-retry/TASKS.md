# TASKS.md — 本地重试（localRetry）

格式：`[ ] 任务名 | 优先级 | 估时 | 依赖`

- [ ] T1 config：新增 `LocalRetry` 结构体，`Provider`/`Route`/`Target` 各加 `LocalRetry *LocalRetry`，加 `defaultLocalRetryIntervalMs`/`maxLocalRetries`/`maxLocalRetryIntervalMs` 常量 | P0 | 0.5h | —
- [ ] T2 config：`validateLocalRetry(label, *LocalRetry)`，在 validateProvider / route 循环 / 每个 target 三处调用；`applyDefaults` 不物化 LocalRetry | P0 | 0.5h | T1
- [ ] T3 router：`resolveLocalRetry(target, route, provider)` 字段级三级解析，合成到 `Candidate.LocalRetryMax` / `Candidate.LocalRetryIntervalMs` | P0 | 0.5h | T1
- [ ] T4 failover：抽 `classifyFailure`（同一套码表、去掉 `f.Enabled` 门禁与 freeAttempt 语义保留），`failoverReason` 改为 enabled 门禁 + 调 classifyFailure | P0 | 0.5h | —
- [ ] T5 main：`forwardAttemptInput` 把 `allowRetry` 拆成 `allowFailoverTransfer` + `allowLocalRetry`；`ShouldRetry` 用 classifyFailure 组合 failoverOK/localOK | P0 | 0.5h | T4
- [ ] T6 main：候选循环内加本地重试内循环；`breaker.Report` 后移到每候选一次（用最终结果）；间隔等待用 `select`+ctx 可中断；每次转发产出一条 `AttemptDetail`（标 local_retry） | P0 | 1.5h | T3,T5
- [ ] T7 测试：config 校验/解析、router 三级解析、main 行为（第 N 次成功 / 不吃 maxAttempts / 熔断只报一次 / freeAttempt 不本地重试 / failover 关闭时仍生效 / 客户端断开中止重试）；并跑 race | P0 | 2h | T6
- [ ] T8 文档：`config.example.yaml` 加注释示例；`CLAUDE.md`「Key Mechanisms」补一条 localRetry 说明 | P1 | 0.5h | T6
- [x] T1–T8 已完成（Codex），后端行为测试全绿。
- [ ] T9 前端（**改判为必做，非可选**）：仓库护栏 `TestFrontendCoversAllProviderFields` / `TestFrontendCoversAllRouteFields` 强制每个后端 config 字段出现在前端所有重建点，`localRetry` 缺失会让这两个测试失败。需把 `localRetry` 串进 `cmd/gateway/web/src/*.js.part` 的 provider / route / target 全部重建点（`00-state` 空表单、`02-config-normalize`、`07-nav-payload`、`10-preview-yaml`、`11-providers` 的 editProvider/duplicate/save，以及 route 的 normalizeConfig + targets），并在对应编辑表单加 maxRetries/intervalMs 输入；改后**重建 `index.html` 产物**并确认 `webbuild -check` exit=0（`TestRepoArtifactMatchesSources`）。| P0 | 2h | T6

> 备注：DESIGN §10 原写「前端可选、YAML 编辑器够用」是错误结论——护栏测试使前端数据串接成为测试强制项，已在委托中纠正。

## 验收标准

1. 容器内 `go test ./...`（含 `cmd/gateway`、`internal/config`、`internal/router`）全绿，且新增用例覆盖 T7 列出的每个行为点。
2. `go vet ./...` 干净、`gofmt` 无 diff。
3. 涉及并发的 T6 改动通过 `go test -race ./cmd/gateway/`。
4. 若做 T9，`webbuild -check` exit=0（`TestRepoArtifactMatchesSources` 不失败）。
5. 未配置 localRetry 时行为与改动前逐字节一致（旧路径零回归）。
