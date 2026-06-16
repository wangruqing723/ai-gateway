# ── 构建阶段 ──────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# 先复制 go.mod 单独下载依赖，利用层缓存
COPY go.mod ./
RUN go mod download

COPY . .

# 静态编译，去除符号表减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ai-gateway ./cmd/gateway

# ── 运行阶段 ──────────────────────────────────
# 用 distroless static，仅含证书与时区，无 shell，攻击面最小
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /ai-gateway /app/ai-gateway

EXPOSE 7789
ENTRYPOINT ["/app/ai-gateway"]
