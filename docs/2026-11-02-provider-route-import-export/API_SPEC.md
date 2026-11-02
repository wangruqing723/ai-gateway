# 提供商与路由配置迁移：文件及 HTTP 契约

- 创建日期：2026-11-02
- 状态：2026-11-02 已确认并开始实施；验证记录见 TASKS.md。
- 设计：[DESIGN.md](DESIGN.md)
- 任务：[TASKS.md](TASKS.md)
- 安全决策：[ADR-0001](../adr/0001-local-sensitive-transfer-boundary.md)

本文定义管理页面配套 Interface，不承诺无 Origin 的脚本或第三方兼容。所有示例身份及凭据均为假数据，不是本地配置。

## 1. 版本化 YAML 迁移包

### 1.1 封装

```yaml
kind: ai-gateway.provider-route-transfer
version: 1
providers:
  example-provider:
    baseUrl: https://example.invalid/v1
    format: openai
    apiKey: example-fake-api-key
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: example-*
    provider: example-provider
    model: example-model
```

| 字段 | 规则 |
|---|---|
| `kind` | 必须精确为 `ai-gateway.provider-route-transfer`。 |
| `version` | 整数，当前仅支持 `1`；不接受字符串版本或未知版本。 |
| `providers` | Provider 名称到完整持久化字段的映射；支持为空以表示路由片段。 |
| `routes` | 有序路由数组；支持为空以表示提供商片段。 |

导出总是包含两个集合；至少一个非空。整体替换要求两者都非空。Provider／Route／Target 使用现有字段及默认值语义，不将 `name`、`path`、`revision`、`apiKeyConfigured`、`valueConfigured`、`fingerprint` 等管理元数据写入文件。

- Provider map 稳定按键排序；路由、候选、密钥列表保持原有顺序。
- 保留单目标与多目标写法、可选指针未配置／显式零值／false 的区别及 ExtraBody 原有数据类型；不归一化成另一种目标写法。
- 含真实 API Key、列表全部密钥、代理密码和全部自定义配置；没有的单值密钥仍为空。
- OAuth 只保存 `authType`、源本地 `accountId` 等提供商配置，不保存账户令牌或账户元数据；迁移账户使用既有独立功能。
- 不携带全局块、源实例地址、原文件路径、运行状态、注释或 YAML 排版。
- 严格单文档，未知字段、重复映射键和源路由重复 `match` 拒绝；大小按 UTF-8 字节计，不超过 `1048576`。
- 保留哨兵 `__AI_GATEWAY_KEEP_API_KEY__`、`__AI_GATEWAY_KEEP_PROXY_PASSWORD__` 不能充当实际迁移凭据。

版本化包不直接用于 `PUT /api/config`；应经本功能提取并构造完整目标配置。

### 1.2 原生配置兼容

没有 `kind`／`version` 的输入按现有原生配置字段严格解码。允许完整 `config.yaml`、只含 `providers` 或只含 `routes` 的片段；至少包含一个非空集合。

原生配置中其他已知顶层字段不导入，预览在 `ignoredGlobalFields` 中列出实际存在的字段名。它们只参与字段／类型识别，不覆盖目标全局值，不因被忽略块的数值设置改变目标。未知字段及形状错误仍拒绝；真正写入的完整目标配置须通过现有完整校验。

不支持第三方专用格式、ZIP、多个 YAML 文档、外部文件引用或动态执行。文件扩展名只用于页面筛选，实际内容由服务端严格解析；上传与粘贴走相同流程。

## 2. 所有新端点的共同规则

### 2.1 请求

| 项目 | 要求 |
|---|---|
| Method | 仅 `POST`，其他方法 `405` 并返回 `Allow: POST`。 |
| Origin | 必须非空，与当前请求的 scheme／host／port 同源，hostname 为 loopback；跨站、非 loopback、缺失 Origin 返回 `403 forbidden_origin`。 |
| Host | 沿用网关入口现有 Host 防护。 |
| Content-Type | `application/json`，允许合法 charset 参数；其他媒体类型 `415 unsupported_media_type`。 |
| If-Match | 必须为最近一次配置 GET 返回的精确强 ETag，不接受缺失、弱 ETag 或通配符。缺失 `428 precondition_required`；版本不匹配 `409 config_conflict`。 |
| JSON | 单个严格对象，未知字段、重复键、尾随文档、错误类型或选择项拒绝，不回显请求原文。 |
| 请求体 | 外层 JSON 上限 8 MiB，独立限制原文和实际配置体积；不是允许 8 MiB 配置文件。 |

`Sec-Fetch-Site: cross-site` 拒绝。不新增 CORS 放行。该访问约束不是身份鉴权；只适用于可信本机或 SSH 本地转发，不影响原有客户端 API 的无 Origin 兼容。

### 2.2 响应与敏感性

- 所有响应 `Cache-Control: no-store`、`Pragma: no-cache`、`X-Content-Type-Options: nosniff`。
- JSON 响应使用 `application/json`，不得包含明文配置、密钥、代理密码、私有请求头／请求体或凭据指纹。
- 名称、`match`、提供商依赖、账户引用、固定字段路径和密钥数量属于预览所需身份／元信息，可返回；不返回其他字段原值。
- 错误兼容现有 `error.type`／`error.message` 结构，可追加安全问题列表；禁止直接返回解析器或校验器的未脱敏原始错误。
- 只有明确导出操作的附件响应允许携带真实配置值。请求体、附件字节、完整候选配置和凭证不得进入日志。

## 3. `POST /api/config/export`

### 3.1 请求

全部导出：

```json
{
  "scope": "all"
}
```

选择导出：

```json
{
  "scope": "selected",
  "selection": {
    "providers": ["example-provider"],
    "routes": [0, 2]
  }
}
```

- `scope` 必填，为 `all` 或 `selected`。
- `all` 不接受 `selection`；取该 revision 的全部提供商和路由。
- `selected` 必须提供 `selection`；两个数组均显式提供，可单独为空，但不能都为空。
- `providers` 是配置 map 的原始名称；`routes` 是该 revision 的原始路由数组下标，零起始，不是页面筛选／排序后的显示下标。
- 负数、非整数、越界、重复或未知选择项拒绝。
- 实际路由输出按源配置相对顺序，不以请求中的下标排列重排。
- 路由隐含依赖全部自动并入，包括候选与路由视觉提供商；没有反向路由扩展。
- 仅对本次选中路由检查忽略大小写重复 `match`，发现即拒绝；不因其他未选路由重复而阻止合法子集。
- 导出快照取后端已保存且已加载配置，不读取页面草稿或未重载磁盘修改。前端在请求前检查未保存修改并逐次确认明文下载风险。

### 3.2 成功

返回 `200`，响应体为第 1 节 YAML 包，头部包括：

```http
Content-Type: application/yaml; charset=utf-8
Content-Disposition: attachment; filename="ai-gateway-provider-routes-YYYYMMDDTHHMMSSZ.yaml"
Content-Length: <实际字节数>
ETag: "<导出所用配置revision>"
```

文件名使用生成时间的 UTC 标记，不含提供商名、源地址或本地文件路径；用标准 MIME 文件名编码，不拼接未信任身份。

导出整体成功后才发送附件头；大于 1 MiB 时返回错误，不返回半个文件。服务器不生成临时导出文件，浏览器拿 blob 下载后释放对象 URL。

### 3.3 特有错误

| HTTP | `error.type` | 情况 |
|---|---|---|
| 400 | `invalid_export` | 选择错误、为空或与 scope 不符。 |
| 400 | `duplicate_route_match` | 本次导出路由内部存在忽略大小写相同模式。 |
| 413 | `transfer_too_large` | 生成的 YAML 超过 1 MiB。 |
| 500 | `export_failed` | 快照／序列化失败；安全消息，不回显配置原值。 |

## 4. `POST /api/config/import/preview`

该端点完成文件解析、选择和计划校验，但永不写磁盘、热重载或测试上游。首轮请求默认全部条目；返回的安全目录用于构建选择／冲突／账户映射界面。

### 4.1 请求

```json
{
  "content": "kind: ai-gateway.provider-route-transfer\nversion: 1\nproviders: {}\nroutes: []\n",
  "options": {
    "mode": "merge",
    "selection": {
      "providers": ["example-provider"],
      "routes": [0]
    },
    "conflicts": {
      "providers": {
        "example-provider": "keep"
      },
      "routes": {
        "0": "overwrite"
      }
    },
    "insertNewRoutes": "after",
    "accountMappings": {
      "acc_source_example": "acc_target_example"
    }
  }
}
```

上例只说明字段形状，`content` 为空集合时本身不构成有效导入；测试和实际请求必须使用存在条目的假配置或迁移文件。

| 字段 | 规则 |
|---|---|
| `content` | 必填，非空 YAML 字符串，UTF-8 不超过 1 MiB；原始字节用于凭证内容绑定。 |
| `options` | 可省略，等同合并、全部选中、冲突保留、新路由后插、无手动账户映射。 |
| `options.mode` | `merge` 或 `replace`；缺省 `merge`。 |
| `options.selection` | 仅合并可用，省略代表全部；显式时两个数组均提供且不可同时为空。身份为源 Provider 名及源路由下标。 |
| `options.conflicts.providers` | 名称到 `keep`／`overwrite`；未给出的冲突默认 `keep`。 |
| `options.conflicts.routes` | 源路由下标的十进制字符串到 `keep`／`overwrite`；未给出的冲突默认 `keep`。 |
| `options.insertNewRoutes` | 仅合并，`before`／`after`，默认 `after`；只控制新增路由整批位置。 |
| `options.accountMappings` | 非空源本地账户 ID 到目标已有本地账户 ID；同一源 ID 共用一次映射。无手动项时尝试相同 ID，不按邮箱猜测。 |

控制选项身份必须存在，不能借无关选项绕过校验；冲突动作只用于已选且真实冲突项。输入选择顺序不控制候选、路由或密钥顺序。

`replace` 要求源两集合都非空，并拒绝 `selection`、`conflicts` 和 `insertNewRoutes`；账户映射仍允许。目标其余全局配置及 `Path` 原样保留。

### 4.2 校验分层

- 文件内全部条目严格校验，包括未选择项的结构、配置数值、凭据哨兵和内部重复模式；错误以 `400` 返回，无可应用凭证。
- 依赖／账户／目标重复冲突等可通过操作选择修正的问题，以正常预览中的 `blockers` 返回，`canApply: false`；不提前写入。
- 实际 `keep` 的源提供商不进入候选配置，不要求解决其未使用的源账户依赖；导入路由若引用它则使用目标保留版本并提示。
- 实际导入项和最终配置引用必须完整，OAuth 未解决不可应用；不主动访问上游验证凭据真实性。
- 没有变更时 `canApply: false`、`previewToken: null`，说明无需应用，不增加 revision。

### 4.3 成功响应

返回 `200`，结构如下；数组字段可为空，不用省略来表达未知状态：

```json
{
  "data": {
    "baseRevision": "example-revision",
    "sourceFormat": "transfer-v1",
    "ignoredGlobalFields": [],
    "canReplace": true,
    "canApply": true,
    "catalog": {
      "providers": [],
      "routes": [],
      "sourceAccountIds": []
    },
    "summary": {
      "providers": {"add": 1, "overwrite": 0, "delete": 0, "keep": 0, "unchanged": 0},
      "routes": {"add": 1, "overwrite": 0, "delete": 0, "keep": 0, "unchanged": 0}
    },
    "changes": {
      "providers": [],
      "routes": []
    },
    "finalRouteOrder": [],
    "warnings": [],
    "blockers": [],
    "previewToken": "<不含配置原文的签名凭证>",
    "expiresAt": "<UTC RFC3339到期时间>"
  }
}
```

`sourceFormat` 为 `transfer-v1` 或 `config-yaml`。返回 `ETag` 对应 `baseRevision`。

安全字段目录：

| 位置 | 可返回的字段 |
|---|---|
| `catalog.providers[]` | `name`、密钥条目数量／单值是否配置、OAuth `sourceAccountId`、引用它的源路由下标。 |
| `catalog.routes[]` | `index`、`match`、全部 `providerDependencies`、是否选中及冲突目标身份。 |
| `catalog.sourceAccountIds` | 需要账户映射的非空源本地 ID 集合，不含令牌、源邮箱或账户状态。目标可选账户由现有安全列表提供。 |
| `changes.providers[]` | `name`、`action`、`changedFields`、前后密钥数量、受影响的目标路由身份。 |
| `changes.routes[]` | 源／目标下标、`match`、`action`、`changedFields`、依赖名称。 |
| `finalRouteOrder[]` | 最终 `position`、`match`、来源 `existing`／`imported`／`overwritten` 及对应下标。 |
| `warnings[]`／`blockers[]` | 固定 `code`、安全中文 `message`、相关名称／路由下标／已知字段路径。 |

`action` 使用 `add`、`overwrite`、`delete`、`keep`、`unchanged`。动作比较实际配置语义；不将管理哨兵、内部 `Name` 或 map 排序误判为配置变更。`finalRouteOrder[].position` 为零起始下标，页面显示时加一。整体替换的旧重复模式只用首项作差异锚点，其余被移除旧项单独记入删除摘要；不改变源集合的唯一规则。

`changedFields` 只列固定配置字段名，嵌套私有内容统一列为 `extraHeaders`／`extraBody` 等父字段。密钥只计数量／存在性，不显示具体 ID 列表、值、前缀或指纹；代理只标记字段发生变化。

典型 warning：

- `globals_ignored`：原生全局字段不导入，目标全局值保留。
- `oauth_accounts_external`：需先单独迁移账户并解决绑定。
- `existing_provider_retained`：导入路由将引用冲突后保留的目标提供商。
- `route_shadowed`：明显被前置启用 `*` 遮挡；不声明完成所有 glob 分析。
- `manual_backup_recommended`：覆盖／替换前建议先手动导出。

典型 blocker：

- `missing_provider_reference`：最终选定配置无法满足提供商依赖。
- `unresolved_account_mapping`：实际导入 OAuth 项缺少有效目标账户。
- `ambiguous_target_route`：合并涉及目标重复模式，不能猜测覆盖项。
- `replace_requires_complete_bundle`：片段不满足完整替换要求。
- `final_config_invalid`：完整候选配置不能通过现有校验；只返回安全字段说明。

存在 blocker 时 `previewToken`、`expiresAt` 为 `null`。最终配置超过 1 MiB 返回 `413`，不签发凭证。

### 4.4 凭证约定

有效计划签发 10 分钟凭证，绑定 revision、原文字节、规范化选项、最终完整配置的 HMAC 摘要及到期时间，使用仅在进程内存在的密钥签名。输入和配置摘要均带密钥，避免暴露可离线猜测弱凭据的公开哈希；token 不含 YAML／候选配置，只作操作凭据，不是身份登录 token。

前端修改文件、选择、动作、映射、模式或插入位置立即丢弃旧 token。关闭弹窗清空原文／文件引用／预览／token；服务端请求结束后不存明文导入会话。服务重启后旧 token 无效。

## 5. `POST /api/config/import/apply`

### 5.1 请求

```json
{
  "previewToken": "<有效预览凭证>",
  "content": "<与预览时完全相同的YAML原文>",
  "options": {
    "mode": "merge"
  }
}
```

`previewToken` 必填，`content` 及 `options` 的规则与预览相同。可省略默认 options，但规范化后的内容必须和已签发计划相同；改原文字节、选择或策略均须重新预览。

`If-Match`、凭证基础 revision、当前配置 revision 必须一致。后端重新解析并重建计划，校验签名、到期、摘要、目标账户存在性及最终配置；不得相信前端提供的预览内容或用 token 跳过校验。

在既有配置事务锁内生成新 revision、一次保存完整候选，再热重载。保存失败不发布部分计划；成功后 revision 变化使旧 token 不能重放。

### 5.2 成功

```json
{
  "status": "success",
  "message": "配置已导入、保存并热重载",
  "revision": "example-new-revision",
  "restartRequired": [],
  "summary": {
    "providers": {"add": 1, "overwrite": 0, "delete": 0, "keep": 0, "unchanged": 0},
    "routes": {"add": 1, "overwrite": 0, "delete": 0, "keep": 0, "unchanged": 0}
  }
}
```

返回 `200` 和新 `ETag`。本功能不改变 host／port；`restartRequired` 沿用现有语义，可能反映此前已存在的待重启设置，不声称导入修改了监听。

成功后前端重新加载普通脱敏配置及账户列表并清空操作状态。如果 HTTP 响应丢失，应报告结果未确认并重新读取配置，不自动重复提交或把“未收到响应”当成确定未保存。

### 5.3 特有错误

| HTTP | `error.type` | 情况 |
|---|---|---|
| 400 | `invalid_preview_token` | 缺失、伪造、格式错误、签名错误或重启后的旧凭证。 |
| 409 | `preview_expired` | 已超过 10 分钟有效期。 |
| 409 | `preview_changed` | 源文档／规范化选项／实际计划与凭证不符。 |
| 409 | `config_conflict` | 当前 revision 与预览基础不符；成功应用后的重放也属于此类。 |
| 409 | `import_dependency_changed` | 应用前目标 OAuth 账户等引用不再满足。 |
| 413 | `config_too_large` | 完整候选配置 YAML 超过 1 MiB。 |
| 500 | `import_save_failed` | 保存失败且未发布运行时计划，沿用已有文件恢复机制。 |
| 500 | `import_storage_uncertain` | 极端 I/O 导致恢复原文件失败；明确磁盘状态需人工确认，不承诺原文件已恢复。 |
| 503 | `transfer_unavailable` | 安全凭证依赖不可用；不降级成无凭证直接应用。 |

## 6. 共同错误示例

```json
{
  "error": {
    "type": "invalid_import",
    "message": "导入配置存在非法字段，请修正后重新预览",
    "issues": [
      {
        "code": "invalid_field",
        "field": "providers.example-provider.maxTokens",
        "line": 8
      }
    ]
  }
}
```

`issues` 可省略；字段路径只含允许显示的身份和已知字段，未知字段名、无效原值、YAML 片段或上游响应原文不回显。

共同状态：

| HTTP | `error.type` | 情况 |
|---|---|---|
| 400 | `invalid_import` | 解析、字段、数值、选择、封装或占位符错误。 |
| 400 | `unsupported_transfer_version` | kind／version 不支持。 |
| 400 | `duplicate_route_match` | 源文件忽略大小写重复模式。 |
| 403 | `forbidden_origin`／`forbidden_host` | 不满足本机访问约束。 |
| 405 | `method_not_allowed` | Method 不支持。 |
| 409 | `config_conflict` | revision 前置条件不满足。 |
| 413 | `request_too_large`／`transfer_too_large` | 外层请求／源文件超限，均不截断。 |
| 415 | `unsupported_media_type` | 非 JSON 控制请求。 |
| 428 | `precondition_required` | 缺少精确 `If-Match` 前置版本。 |
| 500 | `gateway_error` | 不泄露原文的内部失败。 |

失败不生成自动备份、不存历史文件，也不发送凭据到上游测试。实际 I/O 或权限故障按工具／运行环境的真实结果报告，不以接口示例作为已实现或已验证的证据。

## 7. 页面集成约束

- 两页共用弹窗，导出默认全部；选择身份取未过滤源数组下标，不能取 DOM 行号。
- 点击入口和最终提交前检查可视配置／YAML／编辑表单草稿，提示先保存或取消，不自动处理。
- 导入默认合并、全部选中、冲突保留、后插；缺依赖返回 blocker 后继续调整，而不是部分导入。
- 替换隐藏局部选择并独立确认删除范围；引用目标账户只经已有安全列表选择。
- token 过期、版本变化或选项改变后重新预览，确认按钮不得沿用旧计划。
- 全部预览与错误以文本安全渲染；不显示真实配置原值，不把原文或 token 存入浏览器持久存储。
- 关闭／取消释放当次敏感引用；已经发送的保存请求不能靠关闭弹窗撤销，状态不明时先重新读取。
