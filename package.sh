#!/usr/bin/env bash
# package.sh — 一键打包 agent-scope 发布压缩包
# 用法: ./package.sh [版本号]
#
# 依赖:
#   - 宿主机: bash, curl, npm, node
#   - Docker: golang:1.25 镜像(用于后端交叉编译)
#
# 生成 output/agent-scope-{version}-linux-{arch}.tar.gz
# 支持架构: amd64, arm64
set -euo pipefail

ScriptDir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ProjectDir="$ScriptDir"
OutputDir="$ProjectDir/output"

# ---- 版本号 ----
Version="${1:-}"
if [[ -z "$Version" ]]; then
  Version="$(git -C "$ProjectDir" describe --tags --always 2>/dev/null || echo "dev")"
fi
Version="${Version#v}"
echo "📦 agent-scope v$Version 打包开始"

rm -rf "$OutputDir"
mkdir -p "$OutputDir"

# ---- 1. 构建前端 ----
echo ""
echo "=== [1/4] 构建前端 ==="
cd "$ProjectDir/frontend"
if [[ ! -d node_modules ]]; then
  echo "  安装前端依赖..."
  npm install --silent
fi
# 显式清除可能残留的 VITE_APP_VERSION(否则会与模板前缀 'v' 叠加成双 v);
# 版本号由 vite.config.js 的 git describe 自动注入。
unset VITE_APP_VERSION
npm run build --silent
echo "  ✓ 前端构建完成 → backend/internal/server/web/dist/"

# ---- 2. Docker 交叉编译后端 ----
echo ""
echo "=== [2/4] Docker 交叉编译(amd64 + arm64) ==="
cd "$ProjectDir"

docker run --rm \
  -v "$ProjectDir:/app" \
  -w /app/backend \
  golang:1.25 \
  bash -c '
set -euo pipefail
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -buildvcs=false -ldflags="-s -w" -o "/app/output/bin/agent-scope-linux-$arch" .
  echo "  → linux/$arch: $(du -h "/app/output/bin/agent-scope-linux-$arch" | cut -f1)"
done
echo "ALL_BUILD_OK"
'
echo "  ✓ 交叉编译完成"

# ---- 3. 打包各架构压缩包 ----
echo ""
echo "=== [3/4] 组装压缩包 ==="
cd "$ProjectDir"

dist_file() {
  local arch=$1
  local pkg_name="agent-scope-${Version}-linux-${arch}"
  local dist_dir="$OutputDir/$pkg_name"

  mkdir -p "$dist_dir"

  # 二进制
  cp "$OutputDir/bin/agent-scope-linux-${arch}" "$dist_dir/agent-scope"

  # 服务管理脚本
  cp "deploy/agent-scope.sh" "$dist_dir/"

  # 配置(example → 默认配置)
  cp "backend/agent-scope.yaml.example" "$dist_dir/agent-scope.yaml"

  # 文档
  cp "README.md" "$dist_dir/"
  cp "LICENSE" "$dist_dir/"
  cp "OPTIMIZATION_PLAN.md" "$dist_dir/" 2>/dev/null || true
  [[ -d docs ]] && cp -r docs "$dist_dir/"

  # 打包
  cd "$OutputDir"
  tar czf "${pkg_name}.tar.gz" "$pkg_name"
  rm -rf "$pkg_name"
  cd "$ProjectDir"

  echo "  ✓ ${pkg_name}.tar.gz ($(du -h "$OutputDir/${pkg_name}.tar.gz" | cut -f1))"
}

dist_file "amd64"
dist_file "arm64"

# ---- 4. 校验和 ----
echo ""
echo "=== [4/4] 生成校验和 ==="
cd "$OutputDir"
sha256sum *.tar.gz > SHA256SUMS
cat SHA256SUMS
cd "$ProjectDir"

rm -rf "$OutputDir/bin"

echo ""
echo "✅ 打包完成"
echo "   输出: $OutputDir"
ls -lh "$OutputDir/"*.tar.gz 2>/dev/null
