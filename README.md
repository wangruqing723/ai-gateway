# ai-gateway

轻量本地 API Gateway，统一接入 Claude Code、Claude Desktop 和 Codex CLI。

## 特性

- **多格式输入**：同时支持 Anthropic、OpenAI Chat、OpenAI Responses 三种格式
- **模型名映射**：将客户端发来的模型名（如 `claude-3-5-sonnet`）映射到实际模型
- **多 Provider 路由**：不同模型走不同 provider，支持 Anthropic / OpenAI 兼容格式
- **图片翻译**：对纯文本模型自动将图片转换为文字描述（可选）
- **格式自动转换**：请求/响应在不同格式之间自动转换，流式响应完整支持

## 架构

```
Claude Code / Desktop  →  /v1/messages          (Anthropic)      ─┐
OpenAI Chat 工具        →  /v1/chat/completions  (OpenAI Chat)    ─┼→ 路由 → Provider
Codex CLI              →  /v1/responses          (OpenAI Responses)┘
```

## 安装

```bash
git clone <repo>
cd ai-gateway
npm install
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入 provider 配置
```

## 配置

编辑 `config.yaml`：

```yaml
port: 7789

providers:
  mimo:
    baseUrl: "https://token-plan-cn.xiaomimimo.com"
    apiKey: "sk-xxx"      # 可选，留空则从请求头自动获取
    format: anthropic     # anthropic | openai

  mimo_vision:
    baseUrl: "https://api.xiaomimimo.com"
    apiKey: "sk-xxx"
    format: openai

routes:
  - match: "claude-3-5-sonnet*"
    provider: mimo
    model: "mimo-v2.5-pro"
    vision:               # 可选：配置后自动翻译图片
      provider: mimo_vision
      model: "mimo-v2.5"

  - match: "*"
    provider: mimo
    model: "mimo-v2.5-pro"
```

## 启动

```bash
node gateway.js
```

## 接入客户端

### Claude Code

`~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:7789",
    "ANTHROPIC_API_KEY":  "any"
  },
  "model": "claude-3-5-sonnet-20241022"
}
```

### Claude Desktop

**Mac**：`~/Library/Application Support/Claude/claude_desktop_config.json`

```json
{
  "apiBaseUrl": "http://127.0.0.1:7789"
}
```

**Windows**：`%APPDATA%\Claude\claude_desktop_config.json`（同上）

### Codex CLI

```bash
export OPENAI_BASE_URL="http://127.0.0.1:7789"
export OPENAI_API_KEY="any"
codex
```

## 开机自启动

### Mac

```bash
chmod +x scripts/install-mac.sh
./scripts/install-mac.sh install    # 安装
./scripts/install-mac.sh status     # 查看状态
./scripts/install-mac.sh uninstall  # 卸载
```

### Windows（管理员 PowerShell）

```powershell
.\scripts\install-win.ps1 install    # 安装
.\scripts\install-win.ps1 status     # 查看状态
.\scripts\install-win.ps1 uninstall  # 卸载
```

## 路由规则说明

- `match` 支持通配符 `*`（任意字符）和 `?`（单个字符）
- 按顺序匹配，第一条命中的生效
- `vision` 为可选，不配置则不处理图片

## 许可

MIT
