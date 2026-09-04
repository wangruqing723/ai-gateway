## [2026-09-04] 半开熔断探针可能被 context skip 占用
- 发现于: T4 / `cmd/gateway/main.go` 候选循环、`internal/breaker/breaker.go`
- 问题描述: 任务要求在 `breaker.Allow` 之后做 contextWindow 裁决，并要求 context skip 不调用 `breaker.Report`，理由是尚未进入 `forwardAttempt`、没有借出探针额度。但当前 `breaker.Allow` 在半开状态本身会递增 `ProbesInFlight`；若该候选随后因预算不足被跳过，探针额度不会归还，可能使该 provider 后续持续被判为探针额度已满。
- 建议: 后续单独决定是否调整熔断准入与预算裁决的接口边界；本次按既定契约保留裁决位置，context skip 不调用 `breaker.Report`。
- 状态: 待架构决策
