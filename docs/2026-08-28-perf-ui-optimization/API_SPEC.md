# API_SPEC — 观测与配置接口契约变更

需求：2026-08-28 性能与 UI 优化（17 条）
配套文档：[DESIGN.md](./DESIGN.md)、[TASKS.md](./TASKS.md)

本文件只记录**本次需求会触碰到的接口**。未列出的端点契约不变。

---

## 0. 总原则

- **响应体结构一律不变**，只改序列化形式（缩进）与访问控制（Host 白名单）。前端 `index.html` 现有的取值路径（`data.providers`、`data.data`、`health.queues` 等）必须继续可用。
- 新增字段一律 `omitempty` / 可缺省，前端读不到时按现有兜底逻辑走。
- 错误响应继续走 `internal/httputil.WriteJSONError`，形状 `{"error":{"type":"...","message":"..."}}`。前端 `errorText()`（`index.html:1869`）依赖这个形状逐级回退，不要改。

---

## 1. 序列化形式变更（`S1-1`）

涉及端点（当前全部使用 `json.MarshalIndent(x, "", "  ")`）：

| 端点 | Method | 处理函数 | 位置 |
|---|---|---|---|
| `/api/metrics` | GET | `handleMetrics` | `cmd/gateway/main.go:1269` |
| `/api/logs` | GET | `handleLogs` | `cmd/gateway/main.go:1277` |
| `/health` | GET / HEAD | `handleHealth` | `cmd/gateway/main.go:1027` |
| `/v1/models` | GET | `handleModels` | `cmd/gateway/main.go:1312` |
| `/api/providers/health` | POST | `handleProviderHealthCheck` | `cmd/gateway/main.go:1130` |
| `/api/providers/breaker/reset` | POST | `handleBreakerReset` | `cmd/gateway/main.go:1104` |

**变更**：`json.MarshalIndent(v, "", "  ")` → `json.Marshal(v)`。

**契约**：JSON 值语义完全等价，仅去掉缩进与换行。`content-length` 随之变小（各处已按 `len(out)` 计算，无需另改）。

**明确不改**：
- `/api/config` GET（`handleGetConfig`）与 `/api/config/raw` 保持现有形式。前者人会直接用浏览器打开看，后者是 YAML 原文。
- `HEAD /health` 继续只写头不写体，`content-length` 仍按完整 body 长度给出（现有行为，见 `main.go:1077-1081`）。

---

## 2. `/health` 语义与新增字段（`S1-2`、`S1-6`）

### 2.1 `cache` 字段可能是缓存值（`S1-2`）

```
GET /health → 200
{
  "cache": { "total": 128, "contentSize": 45210 }
}
```

**变更**：`total` / `contentSize` 允许最多 10 秒陈旧（`cacheStatsCache` TTL）。字段名、类型、层级不变。

**契约补充**：这两个值不再保证是「调用瞬间的精确值」。前端 `cacheSummary()`（`index.html:2199`）只做展示，不受影响。

`memory.heapAllocMB` / `memory.sysMB` 同样允许最多 10 秒陈旧（`S1-2` 的 `ReadMemStats` 降频）。字段不变。

### 2.2 新增 `metricsWindow`（`S1-6`，可选）

若 `S1-6` 选择走后端下发样本量，则 `/api/metrics` 的 `summary` 增字段（见 3.1）。`/health` 不新增。

---

## 3. `/api/metrics` 契约（`S1-3`、`S1-6`）

### 3.1 `summary` 新增样本量字段（`S1-6`）

```
GET /api/metrics → 200
{
  "summary": {
    "windowRequests": 12,
    "windowMinutes": 15,
    "totalRequests": 3401,
    "successRate": 0.9166,
    "errorRate": 0.0834,
    "p50LatencyMs": 500,
    "p95LatencyMs": 2000,
    "p99LatencyMs": 2000,
    "latencySamples": 12          // 新增
  },
  "providers": [ ... ],
  "statusCodes": { "200": 11, "500": 1 },
  "recentErrors": [ ... ]
}
```

**新增字段 `summary.latencySamples`**（`int`，`omitempty` 不加——0 是有意义的值）：
参与本次 P50/P95/P99 计算的样本总数，即窗口内所有桶 `latency.total` 之和（`metrics.go:31` 的 `latencyHistogram.total`）。

**用途**：前端据此决定是否显示 P95（低于阈值时显示样本量而非分位数，见 DESIGN.md 阶段一 `S1-6`）。

**为什么不复用 `windowRequests`**：两者在正常路径下相等，但 `addMetricLocked` 有两条提前 return（`record.Started.After(observedAt)` 的未来时间戳、迟到记录撞新桶），被丢弃的记录不进直方图。用 `windowRequests` 当样本量会在这些边界下高估。

**兼容**：旧前端读不到该字段时行为不变（现有 `metricValue()` 对缺失键返回 fallback）。

### 3.2 `providers` 数组语义不变（`S1-3`）

`S1-3` 只改 `Collector.Metrics` / `Logs` 的内部实现（避免全量拷贝），**输出必须逐字节等价**。特别注意保留这两个现有行为：

- **兜底分支**：`len(resp.Providers) == 0` 时从 `records` 重算 per-provider 统计（`metrics.go:401-413`）。桶内无 provider 数据时（例如窗口刚重建）靠它兜底，不能删。
- **排序**：`resp.Providers` 按 `P95LatencyMs` 降序（`metrics.go:414`）。
- `recentErrors` 固定取最近 8 条（`metrics.go:418`）。

### 3.3 `/api/logs` 契约不变（`S1-3`）

```
GET /api/logs?limit=100&status=&provider=&stream=&model=&q=&offset=&since= → 200
{ "data": [ RequestLog... ], "limit": 100 }
```

`limit` 回显的是**规范化前**的 `filter.Limit`（`main.go:1301` 传的是 `filter.Limit`，而 `Logs()` 内部对 `<=0 || >200` 的值改成 100 只作用于局部副本）。这是既有行为，`S1-3` 改实现时不要顺手"修正"它——前端没用这个字段，改动只会引入无谓差异。

`RequestLog` 全部字段见 `internal/metrics/metrics.go:110-138`，本次不增删。UI 侧 `S2-4` 只是把已有字段显示出来。

---

## 4. 访问控制变更（`S1-5`）

### 4.1 新增 Host 白名单校验

**适用范围**：所有非静态端点，即 `/health`、`/v1/models`、`/api/*`（含 `/api/config*`、`/api/metrics`、`/api/logs`、`/api/providers/*`）以及推理端点。

**不适用**：`GET /` 与 `/vendor/*`（静态资源，无敏感数据；且 DNS rebinding 拿到 HTML 本身无意义）。

**判据**（新增 `helpers.go` 函数，与 `isLoopbackHostname` 同风格）：

```
allowRequestHost(r *http.Request, cfg *config.Config) bool
```

按序判断，任一命中即放行：

1. `r.Host` 的 hostname 是 loopback（`127.0.0.0/8`、`::1`）或字面 `localhost` → 放行。复用现有 `isLoopbackHostname`（`helpers.go:112`）。
2. `r.Host` 的 hostname 等于 `cfg.Host`（忽略大小写）→ 放行。覆盖 `host: 0.0.0.0` 下用局域网 IP 访问的既有用法。
3. `cfg.Host` 是 `0.0.0.0` / `::` 且 `r.Host` 的 hostname 是 IP 字面量 → 放行。绑定全网卡时任何 IP 直连都是用户自己配出来的。
4. 其余 → 拒绝。

**关键点**：判据只认 IP 字面量与配置里的 host，**不认任意域名**。DNS rebinding 攻击必须让浏览器把某个攻击者域名解析到 127.0.0.1，此时 `Host` 头是那个域名 → 落到第 4 条被拒。

**拒绝响应**：

```
403 Forbidden
{"error":{"type":"forbidden_host","message":"拒绝非本机 Host 的请求"}}
```

新增错误类型 `forbidden_host`，与现有 `forbidden_origin`（`helpers.go:132`）区分开——两者判据不同（一个看 `Host`，一个看 `Origin`/`Sec-Fetch-Site`），混用会让排查时分不清是哪一层拦的。

**与现有 `isCrossSiteRequest` 的关系**：两层都保留，串联。`isCrossSiteRequest` 挡的是正常浏览器的跨站请求（有 `Origin` 头），`allowRequestHost` 挡的是 rebinding（`Origin` 与 `Host` 同源、但都是攻击者域名）。前者对无 `Origin` 的 CLI 请求放行（`helpers.go:96` 显式返回 false），后者对它们同样放行（CLI 直连 `127.0.0.1` 走第 1 条）。

**反向代理场景**：CLAUDE.md 已明确「远程访问必须由前置代理提供认证、TLS 和访问控制」。若代理转发时改写了 `Host` 为外部域名，该请求会被本层拒绝——这是有意的收紧。需要那种部署的用户应让代理保留 `Host: 127.0.0.1:7789`，或把域名写进 `cfg.Host`。**此行为变更必须写进 README 与 `config.example.yaml` 注释**（见 TASKS.md `S1-5` 验收标准）。

---

## 5. 前端资源与 CSP（`S3-2`、`S3-3`）

### 5.1 CSP 收紧

`setSecurityHeaders`（`helpers.go:120-125`）当前值：

```
default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval';
style-src 'self' 'unsafe-inline' https://fonts.googleapis.com;
font-src 'self' https://fonts.gstatic.com data:;
img-src 'self' data:; connect-src 'self'; object-src 'none';
base-uri 'none'; frame-ancestors 'none'
```

**`S3-3`（字体本地化）完成后**：删掉 `https://fonts.googleapis.com` 与 `https://fonts.gstatic.com` 两处白名单。

**`S3-2`（Tailwind 构建期化）完成后**：`script-src` 去掉 `'unsafe-eval'`（Play CDN 的运行时编译需要它，预生成 CSS 不需要）。`'unsafe-inline'` 暂时保留——Alpine 的 `x-data` / `@click` 内联表达式依赖它，去掉需要改写整个前端，不在本次范围。

目标值：

```
default-src 'self'; script-src 'self' 'unsafe-inline';
style-src 'self' 'unsafe-inline'; font-src 'self' data:;
img-src 'self' data:; connect-src 'self'; object-src 'none';
base-uri 'none'; frame-ancestors 'none'
```

### 5.2 新增静态资源路径

字体子集落在 `cmd/gateway/web/vendor/fonts/`，由现有 `//go:embed web/index.html web/vendor/*` 覆盖。

**注意**：`go:embed` 的 `web/vendor/*` 是单层通配，**不递归进子目录**。新增 `vendor/fonts/` 子目录后必须把 embed 指令改成 `web/vendor/...`（`all:` 前缀或显式加一行 `web/vendor/fonts/*`），否则字体文件不会进二进制、生产环境 404。这一点必须在 `S3-3` 里验证（构建产物里能取到字体）。

`handleStatic`（`main.go:1394`）已能服务任意 `vendor/` 下路径，`staticContentType` 走 `mime.TypeByExtension`——`.woff2` 在 Go 标准库的内置表里**没有**，会落到 `application/octet-stream`。需要显式补一条映射，否则部分浏览器拒绝加载。

### 5.3 图标方案

Material Symbols 是 ligature 字体：DOM 里写 `<span class="material-symbols-outlined">monitoring</span>`，靠字体把文本 `monitoring` 渲染成图标。字体加载失败时**直接显示英文单词**——这是 `S3-3` 要解决的核心问题。

两种落地方式，`S3-3` 需二选一并在 KNOWN_ISSUES.md 记录理由：

| 方式 | 做法 | 代价 |
|---|---|---|
| A. 字体子集本地化 | 用 `pyftsubset` 按实际用到的图标名生成 woff2，落到 `vendor/fonts/` | DOM 完全不用改；需引入字体子集工具链；新增图标要重新生成 |
| B. 内联 SVG sprite | 把用到的图标导成 SVG symbol，`<span>` 改 `<svg><use>` | 无字体依赖、无 FOIT；需改约 60 处 DOM |

**推荐 A**：本次是优化需求，DOM 零改动意味着不会碰坏 阶段二（`S2-1` ~ `S2-8`）正在改的那些布局。

**实际用到的图标名**必须从 `index.html` 里穷举（`material-symbols-outlined` 类名的元素文本 + `x-text` 表达式里出现的图标名字符串）。特别注意动态的两个：`progress_activity` 与 `restart_alt` / `health_and_safety` 只出现在 `x-text` 三元表达式里（如 `index.html:759`、`848`、`883`），静态扫 DOM 扫不到，漏掉会让加载中状态显示成英文。

---

## 6. 前端轮询行为（`S1-4`）

不是 HTTP 契约变更，但影响服务端观测到的请求模式，记录在此备查。

**当前**：`init()` 里 `window.setInterval(() => this.refreshMonitor(false), 5000)`（`index.html:1413`），无条件每 5 秒触发。`refreshMonitor` 在 `activeTab === 'logs'` 时只拉 `/api/logs`，否则并发拉 `/health` + `/api/metrics` + `/api/logs`（`index.html:1616-1631`）。

**目标**：
- 标签页 `document.hidden` 为真时暂停轮询，`visibilitychange` 转可见时立即拉一次再恢复。
- `activeTab` 属于 `providers` / `routes` / `settings`（即 `isConfigTab()` 为真）时暂停轮询。切回 `monitor` / `logs` 时立即拉一次。

**必须保留**：`refreshMonitor(false)` 的 `showErrors=false` 语义——轮询失败不弹 toast、不转刷新按钮图标（`index.html:1614-1617` 的注释说明了原因）。恢复轮询时的那次「立即拉取」也走 `false`，否则切标签页会无故弹错误提示。

---

## 7. 不变更清单（防止实施时顺手改坏）

| 项 | 位置 | 为什么不能动 |
|---|---|---|
| `{"error":{"type","message"}}` 错误形状 | `internal/httputil` | 前端 `errorText()` 逐级回退依赖它 |
| `breakers: null` 表示熔断未启用 | `main.go:1093` `breakerSnapshot()` | 与「全部闭合」是两种语义，前端 `breakerLabel(null)` 显示「未启用」 |
| `POST /api/providers/breaker/reset?provider=` 返回 `reset` 为**布尔** | `main.go:1113` | 不带 provider 时返回**条数**（`ResetAll()`）。前端两处分别处理（`index.html:1706`、`1730`），统一成一种会让「无需重置」和「重置了 0 个」混淆 |
| `POST /api/providers/health?provider=` 响应仍包 `providers` 映射 | `main.go:1141` | 与整表检测同构，前端合并逻辑只写一份 |
| `ETag` / `If-Match` 并发控制 | `main.go:1547`、`1638` | 配置保存的乐观锁 |
| `restartRequired` 字段 | `main.go` `restartRequiredFields` | `host`/`port` 改动的提示来源 |
| 推理端点的 `x-ai-gateway-provider` / `x-ai-gateway-attempts` 响应头 | 候选循环 | 客户端据此知道实际命中哪个上游 |
