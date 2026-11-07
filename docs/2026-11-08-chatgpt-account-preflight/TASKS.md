> 实现方：Claude（文档模式）

# ChatGPT 账户独立预检、批量预检与自定义测试内容

创建日期：2026-11-08
核对基线：`7c746d0`，`dev` 分支及本会话未提交改动。
状态：用户于 2026-11-08 确认推荐方案并授权实现；T01–T10 已实现并通过本地验收，尚未提交。

设计见 [DESIGN.md](DESIGN.md)，接口草案见 [API_SPEC.md](API_SPEC.md)。新增能力已实现，完成情况和验证范围见任务及验收记录。

## 已确认需求

- 增加不要求绑定 Provider 的账户预检，保留现有 Provider 链路诊断。
- 增加对勾选账户的批量预检，支持可选检查步骤和实际模型调用。
- 实际调用可填写测试内容；未填写或仅有空白时使用现有默认值 `Reply with OK.`。
- 批量实际调用统一使用本批选定的模型和测试内容。
- 查询模型目录不代表调用成功；实际调用需明确确认消耗额度。
- 采用文档模式，规划已确认，由 Claude 实现，不委托 Codex、不自动提交。

## 已确认产品参数

用户以“开始实现”确认本表全部推荐取值。

| 编号 | 决策项 | 建议 | 理由 |
|---|---|---|---|
| D01 | 批量范围和预算 | 每批最多 50 个账户，并发 3；每账户从开始执行起最多 45 秒；整批最多 15 分钟 | 沿用既有批量账户上限和并发，避免照搬用量刷新 60 秒导致大量实际调用未开始即取消 |
| D02 | 自定义测试内容 | 最多 4096 UTF-8 字节；非空内容保留原始换行和首尾空白；不持久化、不回显模型输出；输出上限沿用 256 token | 支持实际测试内容，同时限制消耗并延续诊断隐私边界 |
| D03 | 批量模型来源 | 从用户明确选择的一个勾选账户查询目录，整批共用所选模型；保留手填，不自动遍历所有账户目录，不自动换模型重试 | 各账户权限可能不同，单个目录不能代表所有账户；减少额外出网 |
| D04 | 暂停账户和批量模式 | 批量仅做独立账户预检；暂停账户保留本地检查，跳过其余出网步骤；Provider 链路诊断继续单账户执行 | 避免自动选择绑定关系及绕过管理暂停 |
| D05 | 进度与取消 | 服务端执行批量，逐账户推送结果；取消停止排队及在途等待，保留已收到结果；不提供断线续跑和历史任务列表 | 明确取消语义，避免长时间只显示等待或浏览器离开后继续消耗 |

账户预检默认选本地检查，外部检查默认不选；模式切换、模型、测试内容、账户集合或来源变化均清除消费确认。上述交互纳入整体文档确认。

## 任务清单

估时是工作量粗估，不含用户确认、环境故障等待或真实上游验收。

- [x] T01 冻结 D01–D05、接口契约与验收矩阵 | P0 | 0.5–1 小时 | 用户确认
- [x] T02 扩展单账户诊断协议，支持独立账户模式和可选测试内容，保持旧调用方兼容 | P0 | 2–3 小时 | T01
- [x] T03 实现固定官方端点的账户级实际调用，复用凭据锁、协议适配、调用记录和取消处理 | P0 | 4–6 小时 | T02
- [x] T04 增加账户级远端模型目录入口，复用解析与身份隔离缓存，补齐删除和替换凭据的清理 | P0 | 3–5 小时 | T01
- [x] T05 实现受限批量执行、逐账户进度、消费确认及取消，协调单账户与批量互斥 | P0 | 4–6 小时 | T03
- [x] T06 更新单账户弹窗：模式选择、模型目录来源、可选测试内容、消费确认重置 | P1 | 3–4 小时 | T03、T04
- [x] T07 新增批量预检入口与结果界面：选择快照、目录来源账户、进度、取消、逐账户结果 | P1 | 4–6 小时 | T04、T05、T06
- [x] T08 补充后端契约、并发、凭据世代、脱敏、取消及真实协议假上游测试 | P0 | 4–6 小时 | T02–T05
- [x] T09 扩展浏览器验证，重建前端产物并运行容器验证、race 和本地假上游 smoke | P0 | 3–5 小时 | T06–T08
- [x] T10 更新操作说明、记录实际验收结果及未验证范围，提交前向用户汇报 | P1 | 1–2 小时 | T09

## 验收条件

- 未绑定账户可显式查询目录和测试模型，整个过程不新增或写入 Provider、路由和 config.yaml。
- 旧的带 Provider 诊断请求保持原语义，绑定校验、停用跳过、代理和排队不退化。
- 三种入口都支持测试内容：独立账户、Provider 链路、批量账户预检；缺省值与空白值一致。
- 自定义内容仅发给明确选择的账户或 Provider；不写请求日志、指标、运行事件、浏览器持久存储或诊断响应。
- 无消费确认不能调用；单个模型失败不推导整个账户不可用；不自动替换模型和账户。
- 无效批量请求在任何出网前拒绝；合法批次中单账户失败、忙碌或执行中消失不阻断其他账户。
- 批量在途任务数、账户预算、整批预算受控；取消不误报成账户失效，不撤销已完成的令牌轮换。
- 目录结果按账户和凭据身份隔离；删除重建同 ID、重新授权和迟到响应不会污染当前结果。
- 前端桌面和窄屏可用，键盘可操作，无控制台错误；查询不自动消费，参数变化重置确认。
- 测试使用假账户与假上游；没有用户另行授权不发起真实付费模型调用。

## 验证计划

在容器 `ai-gateway-dev-verify` 内执行，使用已有 module cache 与 `GOPROXY=https://goproxy.cn,direct`。

- Alpine 镜像：`go run ./cmd/webbuild -check`、`go test ./...`、`go vet ./...`。
- Debian 镜像：`go test -race ./...`。
- 前端：`make web-css`、容器内 `go run ./cmd/webbuild`，运行扩展后的浏览器验证脚本。
- 假上游 smoke：验证实际请求携带默认/自定义文本、协议适配、成功终态、流内失败与取消。记录最终脚本和完整执行命令。
- 真实上游、真实账户额度与外部模型可用性不作为本地假数据测试已验证项。

## 工作区边界

规划开始时已有未提交改动：提供商表格列宽、诊断弹窗复用 Provider 模型选择器及其浏览器测试。实现时保留这些改动，不回滚、不重复记为本需求已完成。此目录不修改或归档既有需求文档。

## 验收记录

实现与本地验收已完成。最终全量 Go 测试、vet、race、webbuild 一致性与实际生成产物的浏览器验证均退出 0。未创建提交。

### 2026-11-08 实跑验证

- 容器 Alpine 全量 `go test ./...`、`go vet ./...` 退出 0；`webbuild -check` 退出 0，前端 HTML 与源码一致。
- 主会话独立运行 `scripts/verify-chatgpt-diagnose.cjs` 退出 0，读取实际生成的 index.html，不以子代理自述或内存拼装代替产物验收。
- 浏览器覆盖独立账户/Provider 模式、默认仅本地检查、目录查询/刷新、UTF-8 内容上限、确认重置、批量快照、逐项结果、断流、取消、迟到结果、409、跨块 SSE、窄屏、键盘与控制台错误。
- Go 专项假上游测试验证默认与自定义文本、实际请求预算、无绑定调用、共享槽、取消、批量并发 3、整批预算、目录缓存与删除期间迟到响应、流内失败、记录来源及隐私。
- 首次 race 检测各运行时代码包通过，但前端源码集成期间生成产物尚未同步，`TestRepoArtifactMatchesSources` 失败。重建产物后的最终全量 race 结果单独记录，不将首次结果描述为全部通过。
- 机械界面扫描有页面级对比度、字体、圆角等建议；本次未改全站设计系统。诊断区域没有定位到新增行的扫描告警，浏览器实测无横向溢出或表达式错误。
- 未测试真实账户和官方上游服务，未发生真实额度消费。未启动或重启正式服务，未提交、合并、推送或归档。

实际执行命令：

```bash
make web-css
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct golang:1.27-alpine \
  sh -c 'go run ./cmd/webbuild && go run ./cmd/webbuild -check && go test ./... && go vet ./...'
PLAYWRIGHT_MODULE=/Users/wyong/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/.pnpm/playwright@1.61.1/node_modules/playwright \
PLAYWRIGHT_EXECUTABLE_PATH=/Users/wyong/Library/Caches/ms-playwright/chromium_headless_shell-1234/chrome-headless-shell-mac-arm64/chrome-headless-shell \
node scripts/verify-chatgpt-diagnose.cjs
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod-deb:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct golang:1.27 go test -race ./...
```

可单独重复假上游 smoke：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct golang:1.27-alpine \
  go test ./cmd/gateway -run 'TestChatGPTPreflight|TestChatGPTAccountCatalog' -count=1 -v
```

最终结果：同源 GET 边界修正后再次执行 `go test ./...`、`go vet ./...`、`go test -race ./...`，均退出 0。完整竞态检测包括 `internal/webbuild` 产物一致性。全部任务已勾选；提交和归档仍须用户明确授权。
