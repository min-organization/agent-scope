#!/usr/bin/env bash
# dev-build.sh — 一键构建前端 + 后端 + 重启服务
# 用法: ./dev-build.sh
#
# 设计:
# - 前端构建(vite build) 直接输出到 backend/internal/server/web/dist/
# - 后端编译为静态二进制
# - 通过 deploy/agent-scope.sh restart 管理(pid/log/db 在 run/)
set -e

ScriptDir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ProjectDir="$ScriptDir"

echo "=== [1/4] 检查前端依赖 ==="
cd "$ProjectDir/frontend"
if [[ ! -d node_modules ]]; then
  echo "  安装前端依赖..."
  npm install --silent
fi

echo ""
echo "=== [2/4] 构建前端(vite build) ==="
# 显式清除可能残留的 VITE_APP_VERSION(否则会与模板前缀 'v' 叠加成双 v);
# 版本号由 vite.config.js 的 git describe 自动注入。
unset VITE_APP_VERSION
npm run build
echo "  前端构建完成, 输出: backend/internal/server/web/dist/"

echo ""
echo "=== [3/4] 编译后端(Go, 零 CGO 单二进制) ==="
cd "$ProjectDir/backend"
binName="agent-scope"
CGO_ENABLED=0 /usr/local/go/bin/go build -ldflags="-s -w" -o "$binName" .
echo "  后端编译完成: backend/$binName"

echo ""
echo "=== [4/4] 重启服务 ==="
cd "$ProjectDir"
if [[ -x ./deploy/agent-scope.sh ]]; then
  ./deploy/agent-scope.sh restart
else
  echo "  未找到 deploy/agent-scope.sh, 跳过服务重启"
  echo "  请手动重启(如: systemctl restart agent-scope)"
fi

echo ""
echo "✅ dev-build 完成"
# 端口提取与 deploy/agent-scope.sh 保持一致(取 addr 行首个数字序列, 兼容 ::18092 双冒号)
_port="$(grep -E '^\s*addr:' backend/agent-scope.yaml | head -1 | sed -E 's/#.*$//' | grep -oE '[0-9]+' | head -1)"
echo "   浏览: http://localhost:${_port:-8090}"