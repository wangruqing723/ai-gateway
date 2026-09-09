# KNOWN_ISSUES：跨需求统一跟踪

> 本文件只收**未确认或未完成**的已知问题,来自已归档需求目录的遗留项。
> 已修复、已决策维持现状的条目不进本文件,留在各自归档目录里即可。
> 归档于 2026-09-09,整理自 `docs/_archive/` 下各需求目录。

## [2026-08-29] openai-responses 协议未经真实上游验证(原 KI-1)

- 来源:`_archive/2026-08-29-attempt-observability-review-fixes/`
- 现状:四组协议组合(Anthropic/Chat/Responses 客户端 × Responses 上游)已用本地 echo 假上游跑通端到端,请求体字段映射符合协议;但真实 `openai-responses` 上游从未跑通(验证时 agentrouter.org 返回 401,系凭据问题而非协议构建问题,网关侧请求确实抵达上游)。
- 待办:接入一个真实 Responses 上游(如官方 API 或可用中转)跑一次非流式 + 流式请求,确认字段映射与错误语义透传。
- 现网状况:线上 provider 全部为 anthropic/openai 格式,该路径无生产流量,风险敞口为零。

## [2026-08-29] 跨协议流式转换的网络层边界未实测(原 KI-2)

- 来源:`_archive/2026-08-29-attempt-observability-review-fixes/`
- 现状:Responses SSE → Anthropic SSE 已在真实 HTTP 长连接上跑通完整 7 事件序列;但真实上游的分块边界、半行到达、keep-alive 空行、事件乱序仍未实测(echo 上游一次性写完载荷,不产生分块)。流式活跃超时补发收尾事件同样只有单测。
- 待办:如遇跨格式流式请求出现解析异常,优先怀疑这两个边界;可构造分块/半行的假上游补测。

## [2026-08-29] 「已开始响应,未转移」标记未被真实流量触发过(原 KI-3)

- 来源:`_archive/2026-08-29-attempt-observability-review-fixes/`
- 现状:前端 `responseStarted && outcome !== 'success'` 标记存在且产物一致,但没有一次真实「2xx 响应头之后才失败」的流量渲染过它。
- 待办:可用「故意写出若干字节后断开」的假上游触发一次,同时验证「2xx 之后失败计入熔断」确实调了 `breaker.Report`(CLAUDE.md 已有此约定,`forwardErr` 在任何状态码下都上报)。

## [2026-08-29] `isMeaningfulExtra` 收窄清单未与真实 Codex 字段集比对(原 KI-4)

- 来源:`_archive/2026-08-29-attempt-observability-review-fixes/`
- 现状:清单按官方文档默认值手写(`store` false / `n` 1 / `parallel_tool_calls` true 等);抓取机制已就绪(echo 假上游落盘网关出口 body),但还没有发过一次真实 Codex 请求做比对。
- 待办:使用者用 Codex CLI 经网关发一次请求,把清单从「按文档推断」升级为「按实测比对」。
- 保护:负向断言(`previous_response_id`/`conversation`/`background`/`store_true`/`n>1` 仍须拒绝)是防止清单被过度收窄的唯一防线,改清单不得删这组断言。

## [2026-08-29] 配置页 1024–1280px 窄窗口肉眼验收未做(原 S2-8)

- 来源:`_archive/2026-08-28-perf-ui-optimization/`
- 现状:静态判断结论是「预览改抽屉后配置区只剩单主栏,`lg` 内滚动可覆盖全部内容,无需额外改动」,但没有实机肉眼确认过。
- 待办:把浏览器窗口拖到 1024–1280px 宽看一眼配置页滚动是否正常;正常即可关闭本条。
