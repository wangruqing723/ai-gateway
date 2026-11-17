# 配置与管理 API：全局按格式请求扩展

状态：已实现。

## 配置字段

`Config` 顶层新增按格式选择的专用 `globalUserAgent`，以及可选字段 `globalUpstreamExtras`：

```yaml
globalUserAgent:
  anthropic: "gateway-anthropic/1.0"
  openai: "gateway-openai/1.0"
  openai-responses: "gateway-responses/1.0"
```

每种格式分别按 Provider `userAgent` → 对应格式的全局值 → 客户端 `User-Agent` → Go 默认值选择。早期开发版保存的单字符串会迁移为三种格式相同的值。

```yaml
globalUpstreamExtras:
  anthropic:
    headers: {X-Shared-Header: value}
    body: {enable_thinking: true}
  openai:
    headers: {}
    body: {}
  openai-responses:
    headers: {}
    body: {}
```

JSON 形状相同；`openai-responses` 保持连字符，与 Provider 的 `format` 枚举完全一致。

| 路径 | 方法 | 变化 |
| --- | --- | --- |
| `/api/config` | `GET` | 返回可选 `globalUserAgent` 与 `globalUpstreamExtras`。 |
| `/api/config` | `PUT` | 接受并严格校验这两个字段；这是全量替换，调用方必须回传已有值。 |
| `/api/config/raw` | `GET` | 原始 YAML 视图包含这两个字段。 |

不新增独立端点，也不改变 ETag / `If-Match`、热重载或原有配置 API 的媒体类型规则。

## 校验契约

| 项目 | 规则 |
| --- | --- |
| 格式键 | 仅 `anthropic`、`openai`、`openai-responses`；未知字段拒绝。 |
| `headers` | 复用 `extraHeaders` 的条数、长度、HTTP 头 token 和控制字符限制；认证、内容类型和 UA 头拒绝。 |
| `body` | 复用 `extraBody` 的条数、字段长度、JSON 编码大小及受保护字段限制。 |
| 空值 | 缺省、`null`、空 map 均代表不注入；落盘尽量省略空项。 |
| 冲突 | 同名局部项不会使 PUT 失败；实际请求中局部项覆盖全局格式项。 |

错误信息应带完整配置路径，例如：`globalUpstreamExtras.openai.headers[X-Example]`，方便在 YAML 和可视化页面中定位。

## 请求行为契约

在实际模型推理 POST 即将发出前，按目标 Provider 格式选择并附加配置：

```text
client request
  → protocol conversion / model routing / existing managed fields
  → globalUpstreamExtras[upstream format]（可选兜底）
  → existing Provider / Route / Target local extras（同名优先、原有顺序不变）
  → existing OAuth cleanup / protocol handling
  → HTTP request to upstream
```

请求头同样在既有协议头、认证与 User-Agent 写入之后、既有 Provider `extraHeaders` 写入之前添加全局兜底。每一次重试或转移都重新走这条规则。未配置时不增加任何 header 或 JSON 字段。全局项严格复用既有的字段放行和禁止覆盖规则，不扩大也不收窄其范围。管理/探测 GET 不接收 body 注入。
