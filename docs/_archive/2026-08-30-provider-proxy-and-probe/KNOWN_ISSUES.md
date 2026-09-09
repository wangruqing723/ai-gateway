## [2026-08-30] socks5h scheme 的 Go Transport 实测受环境限制

- 发现于:T9 / `internal/httpclient`
- 问题描述:Codex 侧后台环境没有宿主机 Go 工具链，且 Docker socket 拒绝访问（`permission denied while trying to connect to the docker API at unix:///Users/wyong/.colima/default/docker.sock`），无法按任务要求在 `golang:1.23` 容器中实测 `http.Transport` 对 `socks5h://` 的支持。
- 建议:在可访问 Docker 的环境执行 `go test` 前，用最小代理请求验证 `socks5h://`，确认后再决定是否纳入白名单。
- 状态:**已解决（2026-08-30，验收阶段实测）**

实测方法:在 `golang:1.23-alpine` 容器内对 `socks5://127.0.0.1:9` 与 `socks5h://127.0.0.1:9`
各发一次请求（9 端口无人监听），对比错误形态。

```
socks5   err = Get "http://example.com": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused
socks5h  err = Get "http://example.com": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused
```

结论:两者行为一致,都走到了拨号阶段,而不是报 `unsupported protocol scheme`——
说明 Go 1.23 的 `http.Transport` **接受** `socks5h`。据此按 T9 的既定口径纳入白名单
（`internal/config/config.go` 的 `validateProvider` 与 `internal/httpclient` 的
`normalizeProxyURL` 两处,以及 README、弹窗提示、`config.example.yaml` 三处文案）。

未验证的部分:只确认了 scheme 被接受,**没有**验证 Go 对 `socks5h` 的 DNS 解析语义
是否与 curl 一致（curl 里 `socks5h` 表示由代理解析域名）。需要该语义保证的场景应自行实测。
