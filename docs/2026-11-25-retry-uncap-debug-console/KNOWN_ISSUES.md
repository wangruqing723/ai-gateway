## [2026-11-25] 调试队列允许已停用 Provider 临时准入

- 发现于：B7 Provider 模式实现 `AcquireForDebug` / `AcquireGroupForDebug` 时。
- 问题描述：普通队列会按 `Reconcile` 的 active Provider 集拒绝已停用 Provider（`ErrProviderRemoved`），但设计要求 Provider 调试模式忽略启停状态、同时继续遵守并发、限速与共享账户队列限制。调试入口因此使用显式允许 inactive 的队列准入路径。若 Provider inactive，队列保持 `retired=true`。初次修复只看队列的 `requestTimes`，导致普通流量队列也会因为尚未过期的限速记录延迟回收，改变 `Reconcile` 对空闲删除 Provider 的原有行为。当前修正单独追踪调试准入的 `debugRequestTimes`：只有仍处于一秒窗口内的调试记录才会让空闲的 retired Provider 队列暂留，并由定时器在记录过期后回收；只有普通 `Acquire` 记录的删除队列仍立即回收。在途请求期间 `Reconcile` 不会中断已准入请求。Provider 重新启用时清除 `retired` 并复用队列，尚有效的调试时间戳继续参与 Provider 限速；重新启用后再次停用时，只按仍有效的调试时间戳决定是否暂留。共享组自己的 `requestTimes` 则在空闲后安排过期定时器，避免共享组队列长期留在组表中。共享组调试请求因 Provider 限速而等待时，`tryAdmitProvider` 会更新 Provider 限制并停止旧 retirement 定时器；若该组最后一个 waiter 随后取消或超时，Provider 队列也必须重新安排调试窗口回收。取消和超时目前都会经 `cancelGroupWaiter` 触发这一步，不要求后续流量或配置变更。
- 建议：由 Claude 结合队列回归测试与 race 验证结果决定是否接受这条仅为调试准入保留限速窗口的路径。
- 状态：Claude 已接受；相关回归与 race 检测通过，调试入口遵循 API_SPEC，普通推理入口保持兼容。

## [2026-11-25] 未命中路由的错误类型口径

- 发现于：B6 对照 API_SPEC §1.5 与现有 `server.handle()` 的未命中路由分支时。
- 问题描述：现有推理入口在未命中路由时返回 HTTP 400，正文消息为「没有匹配 model … 的路由规则」，JSON `type` 是 `gateway_error`；请求日志的 `errorCategory` 才是 `route_not_found`。API_SPEC 将 `route_not_found` 列为事件流内错误类型，同时要求状态码和正文与真实链路一致。
- 建议：确认 `route_not_found` 是 SSE 错误正文的 `type`，还是仅指日志分类。当前调试入口按 API_SPEC 使用 `type: route_not_found`，保留真实链路的 HTTP 400 与消息，未改真实推理入口。
- 状态：Claude 已接受；相关回归与 race 检测通过，调试入口遵循 API_SPEC，普通推理入口保持兼容。
