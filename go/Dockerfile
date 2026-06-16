# ── 构建阶段 ──────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# 使用国内模块代理，避免 proxy.golang.org 不可达导致下载超时
ENV GOPROXY=https://goproxy.cn,direct

# 复制源码，用 go mod tidy 自动解析并下载依赖（仓库未提交 go.sum）
COPY . .
RUN go mod tidy

# 静态编译，去除符号表减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ai-gateway ./cmd/gateway

# ── 运行阶段 ──────────────────────────────────
# 用 alpine 作为最小运行基础镜像（含证书与时区）。
# 注：distroless（gcr.io）攻击面更小，但国内拉取不便，生产可按需替换。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /ai-gateway /app/ai-gateway

EXPOSE 7789
ENTRYPOINT ["/app/ai-gateway"]
