#!/usr/bin/env bash
# ai-gateway Mac 自启动安装脚本（launchd）
# 用法：
#   ./scripts/install-mac.sh install   安装并启动
#   ./scripts/install-mac.sh uninstall 停止并卸载
#   ./scripts/install-mac.sh status    查看状态

set -e

LABEL="com.ai-gateway"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
NODE_BIN="$(which node)"
LOG_DIR="$HOME/.local/share/ai-gateway"

install() {
  mkdir -p "$LOG_DIR"

  cat > "$PLIST" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${NODE_BIN}</string>
    <string>${SCRIPT_DIR}/gateway.js</string>
  </array>
  <key>WorkingDirectory</key>  <string>${SCRIPT_DIR}</string>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>StandardOutPath</key>   <string>${LOG_DIR}/stdout.log</string>
  <key>StandardErrorPath</key> <string>${LOG_DIR}/stderr.log</string>
</dict>
</plist>
EOF

  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load   "$PLIST"
  echo "✓ ai-gateway 已安装并启动"
  echo "  日志目录: $LOG_DIR"
  echo "  停止命令: ./scripts/install-mac.sh uninstall"
}

uninstall() {
  if [ -f "$PLIST" ]; then
    launchctl unload "$PLIST" 2>/dev/null || true
    rm -f "$PLIST"
    echo "✓ ai-gateway 已停止并卸载"
  else
    echo "未找到 plist 文件，可能未安装"
  fi
}

status() {
  if launchctl list | grep -q "$LABEL"; then
    echo "✓ ai-gateway 正在运行"
    launchctl list "$LABEL" 2>/dev/null || true
  else
    echo "✗ ai-gateway 未运行"
  fi
}

case "${1:-}" in
  install)   install   ;;
  uninstall) uninstall ;;
  status)    status    ;;
  *)
    echo "用法: $0 install | uninstall | status"
    exit 1
    ;;
esac
