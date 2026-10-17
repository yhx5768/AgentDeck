#!/usr/bin/env bash
# 前端回归测试的唯一入口。第一次跑会装 jsdom（唯一依赖）。
#   ./scripts/frontend-tests/run.sh
# pre-commit 守卫（scripts/hooks/pre-commit）会在动了 web/ 时调它。
set -euo pipefail
cd "$(dirname "$0")"
[ -d node_modules ] || npm install --silent --no-audit --no-fund
npm test --silent
