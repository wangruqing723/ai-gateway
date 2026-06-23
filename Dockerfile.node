# ── 构建阶段 ──────────────────────────────────
FROM node:20-alpine AS builder

WORKDIR /app

# 先复制依赖清单，利用 Docker 层缓存
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

# 清理 sql.js 中不需要的文件（只保留 Node.js 需要的 WASM 版本）
RUN cd node_modules/sql.js/dist && \
    rm -f *-debug* *-browser* worker.* sql-asm*.js sql-asm*.js.map && \
    ls -la

# 复制源码
COPY gateway.js ./
COPY lib/ ./lib/

# ── 运行阶段 ──────────────────────────────────
FROM node:20-alpine

WORKDIR /app

# 从构建阶段复制
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./
COPY --from=builder /app/gateway.js ./
COPY --from=builder /app/lib ./lib

# 创建数据目录
RUN mkdir -p /app/data

EXPOSE 7789

# 健康检查
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://0.0.0.0:7789/health || exit 1

CMD ["node", "gateway.js"]
