#!/usr/bin/env bash
# 构建可发布的静态二进制包 + 校验和。
#
# 本地与 GitHub Actions 共用这一份逻辑（工作流只负责把 dist/ 传上 release），
# 这样"发布物怎么打出来"只有一处定义。
#
# 用法：
#   VERSION=v0.4.0 ./scripts/build-release.sh
#   PLATFORMS="amd64" ./scripts/build-release.sh      # 只出一个平台，调试用
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST="${DIST:-$ROOT/dist}"
PLATFORMS="${PLATFORMS:-amd64 arm64 armv7}"

# -s -w 去符号表；-X 把版本写进 agent 上报的 agent_version 与 server 日志。
LDFLAGS="-s -w -X main.version=$VERSION"

host_arch=$(uname -m)
case "$host_arch" in
  x86_64) host_arch=amd64 ;;
  aarch64 | arm64) host_arch=arm64 ;;
  armv7l) host_arch=armv7 ;;
esac

rm -rf "$DIST"
mkdir -p "$DIST"

echo "版本：$VERSION"
echo "平台：$PLATFORMS"
echo

for p in $PLATFORMS; do
  case "$p" in
    amd64) goarch=amd64; goarm="" ;;
    arm64) goarch=arm64; goarm="" ;;
    armv7) goarch=arm; goarm=7 ;;
    *)
      echo "未知平台：$p（支持 amd64 / arm64 / armv7）" >&2
      exit 2
      ;;
  esac

  echo "== linux/$p =="
  stage=$(mktemp -d)
  trap 'rm -rf "$stage"' EXIT

  GOOS=linux GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/agent" ./cmd/agent
  GOOS=linux GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/server" ./cmd/server

  # 静态性必须成立：发布物要能在任何发行版上直接跑。
  if ldd "$stage/agent" >/dev/null 2>&1; then
    echo "  ✗ agent 不是静态链接的" >&2
    exit 1
  fi
  if ldd "$stage/server" >/dev/null 2>&1; then
    echo "  ✗ server 不是静态链接的" >&2
    exit 1
  fi

  # 本机架构的直接跑一次烟测：用 -check 让 server 解析一份临时配置后退出。
  # 跨架构的只做静态性检查，没法在这里执行。
  if [[ "$p" == "$host_arch" ]]; then
    cat >"$stage/smoke.yaml" <<'YAML'
db: /tmp/simpleprobe-smoke.db
nodes:
  - id: smoke
    token: smoke
YAML
    if ! "$stage/server" -check -config "$stage/smoke.yaml" >/dev/null; then
      echo "  ✗ 烟测失败：打包出来的 server 跑不起来" >&2
      exit 1
    fi
    rm -f "$stage/smoke.yaml"
    echo "  烟测通过（-check 解析配置成功）"
  fi

  # ---- agent 包 ----
  pkgdir="$stage/pkg-agent"
  mkdir -p "$pkgdir/deploy"
  cp "$stage/agent" "$pkgdir/agent"
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$pkgdir/"
  cp "$ROOT/deploy/simple-probe-agent.service" "$ROOT/deploy/agent.example.yaml" "$pkgdir/deploy/"
  agent_pkg="simpleprobe-agent_${VERSION}_linux_${p}.tar.gz"
  tar -czf "$DIST/$agent_pkg" -C "$pkgdir" .

  # ---- server 包 ----
  pkgdir="$stage/pkg-server"
  mkdir -p "$pkgdir/deploy"
  cp "$stage/server" "$pkgdir/server"
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$pkgdir/"
  cp "$ROOT/deploy/simple-probe-server.service" "$ROOT/deploy/nodes.example.yaml" "$pkgdir/deploy/"
  # 前端一起带上：server 加 -web 就能直接托管面板，不用再单独分发。
  cp -r "$ROOT/web" "$pkgdir/web"
  # 加节点的脚本必须一起带上：否则一键装完 server 之后，官方推荐的加节点方式
  # （add-node.sh）在这台机器上根本不存在。
  cp "$ROOT/scripts/add-node.sh" "$pkgdir/add-node.sh"
  server_pkg="simpleprobe-server_${VERSION}_linux_${p}.tar.gz"
  tar -czf "$DIST/$server_pkg" -C "$pkgdir" .

  printf "  %-46s %s\n" "$agent_pkg" "$(du -h "$DIST/$agent_pkg" | cut -f1)"
  printf "  %-46s %s\n" "$server_pkg" "$(du -h "$DIST/$server_pkg" | cut -f1)"

  rm -rf "$stage"
  trap - EXIT
done

# 安装脚本作为 release 资源一起发，这样一键安装可以直接从 release 取。
cp "$ROOT/scripts/install.sh" "$DIST/install.sh"
chmod 0755 "$DIST/install.sh"

echo
echo "== 校验和 =="
# 只对包做校验和；install.sh 自己列进去没有意义（它就是要被验证的对象之一）。
(cd "$DIST" && sha256sum ./*.tar.gz >SHA256SUMS)
cat "$DIST/SHA256SUMS"

echo
echo "产出在 $DIST/"
