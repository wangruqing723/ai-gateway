---
name: gateway-todo-tracker
description: 跟踪和管理 go/TODO-review.md 中的问题修复进度,支持按优先级查看和更新状态
user-invocable: true
---

# Gateway TODO 跟踪器

管理 `go/TODO-review.md` 中的问题列表,跟踪修复进度。

## 用途

- 查看当前待修复问题列表
- 按优先级分类显示(并发/超时/迁移/清理)
- 标记问题为已修复
- 生成进度报告

## 使用方式

### 查看所有问题状态

```bash
cat go/TODO-review.md | grep -E "^###|已修复|待修复"
```

### 按优先级查看

显示问题分类:
- **第一批(并发)**: #1 #2 #3 - 数据竞争问题
- **第二批(超时/流式)**: #4 #5 #6 #7 #8 - 超时语义对齐
- **第三批(迁移等价性)**: #9 #10 #11 #12 - 行为对齐 Node 版本
- **第四批(清理)**: #13 #14 #15 - 代码优化

### 检查相关文件

涉及的主要文件:
- `go/cmd/gateway/main.go` - 主入口,队列调度
- `go/internal/proxy/proxy.go` - 转发逻辑,流式处理
- `go/internal/vision/vision.go` - 图片识别,singleflight
- `go/internal/cache/cache.go` - SQLite 缓存
- `go/internal/router/router.go` - 路由匹配
- `go/internal/converter/` - 格式转换

### 标记问题为已修复

当修复某个问题后,在标题中添加 `✅` 和 `[已修复]` 标记:

```markdown
### ✅ 1. 共享 `*config.Provider` 被逐请求修改 [已修复]
```

### 生成进度报告

```bash
echo "=== TODO 修复进度 ==="
echo ""
echo "已修复问题:"
grep -c "✅.*已修复" go/TODO-review.md || echo "0"
echo ""
echo "待修复问题:"
grep -E "^### [0-9]" go/TODO-review.md | grep -v "✅" | wc -l || echo "0"
```

## 注意事项

- 本技能不执行 Go 代码编译或测试
- 仅管理文档和跟踪修复状态
- 如需验证修复效果,需要在 Go 环境中手动测试
- 建议在 Docker 容器中测试 Go 版本(不需要本地安装 Go)

## Docker 测试命令

即使本地没有 Go,也可以用 Docker 测试:

```bash
# 构建镜像
cd go && docker build -t ai-gateway-go .

# 运行测试(如果有测试文件)
docker run --rm ai-gateway-go go test -race ./...

# 运行服务验证
docker run -d -p 7789:7789 \
  -v "$PWD/../config.yaml:/app/config.yaml:ro" \
  ai-gateway-go
```
