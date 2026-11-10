#!/usr/bin/env sh
# 构建 Vue 版管理界面，产物落到 internal/httpapi/webdist 供 go:embed 打包。
#
# 用法：
#   scripts/web-build.sh          # 一次性构建
#   scripts/web-build.sh --dev    # 起 vite dev server（/api 代理到 :6060）
#
# 只改 Go 代码时不需要跑这个；改了 web/ 下的东西才需要。
set -eu

cd "$(dirname "$0")/.."

if [ ! -d web/node_modules ]; then
  echo "==> 安装前端依赖"
  (cd web && npm install --no-audit --no-fund)
fi

if [ "${1:-}" = "--dev" ]; then
  echo "==> vite dev server：http://127.0.0.1:5273（/api 代理到 :6060）"
  cd web && exec npm run dev
fi

echo "==> 构建前端 → internal/httpapi/webdist"
(cd web && npm run build)

echo "==> 完成。接着 go build ./cmd/server 就会把界面打进去。"
