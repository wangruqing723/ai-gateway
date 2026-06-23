# ── 构建阶段 ──────────────────────────────────
FROM golang:1.23-alpine AS builder

WORKDIR /build

# 使用国内模块代理，避免 proxy.golang.org 不可达导致下载超时
ENV GOPROXY=https://goproxy.cn,direct

# 复制 Go 源码（假设 Dockerfile 在项目根目录，需要复制 go/ 子目录）
COPY go/ .
RUN go mod tidy

# 静态编译，去除符号表减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ai-gateway ./cmd/gateway

# ── 运行阶段 ──────────────────────────────────
# distroless static：仅含 CA 证书，无 shell/包管理器，攻击面最小、体积最小。
# 时区数据已通过 `import _ "time/tzdata"` 嵌入二进制，无需基础镜像提供 tzdata。
# 注：gcr.io 国内不可达，这里用 daocloud 镜像加速；若网络可直连 gcr.io 可改回原地址。
FROM m.daocloud.io/gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /ai-gateway /app/ai-gateway

EXPOSE 7789
ENTRYPOINT ["/app/ai-gateway"]
