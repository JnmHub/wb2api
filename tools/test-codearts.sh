#!/usr/bin/env bash
# 专项测试：华为云 CodeArts 渠道。
#
# 覆盖本渠道的全部入口——协议客户端、账号池接线、面板接口、调度器、续期——
# 但不跑全量 go test ./...：其余包与本渠道无关，全量只在发布前跑。
#
# 用法:
#   tools/test-codearts.sh          # 编译 + vet + 专项测试
#   tools/test-codearts.sh web      # 另外跑面板类型检查/单测/构建
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== go build =="
go build ./...

echo "== go vet =="
go vet ./internal/...

echo "== 专项测试 =="
go test ./internal/upstream/codearts \
        ./internal/codeartsync \
        ./internal/api \
        ./internal/scheduler \
        ./internal/refresh \
        ./internal/realm \
        ./internal/store/accounts \
        ./internal/store/settings \
        ./internal/realmswitch \
        ./internal/quota \
        -count=1

# 网关专项：只跑 CodeArts 相关用例，不跑该包其余一百多秒的回归
echo "== 网关专项 =="
go test ./internal/gateway/openai -run 'CodeArts|TestAccountLimitPerChannel' -count=1

if [ "${1:-}" = "web" ]; then
  echo "== 面板 =="
  cd web
  npm run typecheck
  npm test
  npm run build
fi

echo "== OK =="
