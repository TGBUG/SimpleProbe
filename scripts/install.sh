#!/usr/bin/env bash
# SimpleProbe 一键安装 / 升级。
#
# agent（装在被监控机上）：
#   curl -fsSL https://github.com/TGBUG/SimpleProbe/releases/latest/download/install.sh \
#     | bash -s -- agent --server https://probe.example.com --node web01 --token <token>
#
# server（装在一台有公网或内网的机器上）：
#   curl -fsSL https://github.com/TGBUG/SimpleProbe/releases/latest/download/install.sh \
#     | bash -s -- server --node web01
#
# 管道执行等于把远端代码直接交给 shell。脚本本身只做五件事——解析参数、下载
# release 包、校验 SHA256、装二进制与 systemd 单元、启动服务。要更稳妥就先
# 下载下来看一眼再跑：
#   curl -fsSLO .../install.sh && less install.sh && bash install.sh server
set -euo pipefail

REPO="TGBUG/SimpleProbe"

MODE=""
VERSION="latest"
PREFIX="/opt/probe"
CONF_DIR="/etc/probe"
FROM_DIR=""
NO_SYSTEMD=0
FORCE=0
DRY_RUN=0

# server 模式
NODE=""
LISTEN="127.0.0.1:8080"
DB="/var/lib/probe/probe.db"

# agent 模式
SERVER_URL=""
TOKEN=""
INTERVAL="30s"
MOUNTS='["/"]'

log() { printf '%s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }
die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}
# run 在 dry-run 下只打印，不执行。
run() {
  if ((DRY_RUN)); then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

usage() {
  cat <<'EOF'
用法：
  install.sh server [选项]
  install.sh agent  --server <URL> --node <id> --token <token> [选项]

server 选项：
  --node <id>        顺便生成第一个节点并打印 agent 侧配置
  --listen <addr>    监听地址，默认 127.0.0.1:8080
  --db <path>        数据库路径，默认 /var/lib/probe/probe.db

agent 选项：
  --server <URL>     server 地址，必填
  --node <id>        节点 id，必填，必须与 server 端配置一致
  --token <token>    server 端为该节点配置的 token，必填
  --interval <时长>  上报间隔，默认 30s（合法范围 5s ~ 1h）
  --mounts <JSON>    监控的挂载点，默认 ["/"]

通用选项：
  --version <tag>    release 版本，默认 latest
  --prefix <目录>    二进制安装位置，默认 /opt/probe
  --conf-dir <目录>  配置目录，默认 /etc/probe
  --from-dir <目录>  不从网络下载，改从这个本地目录取包（离线安装 / 本地测试）
  --no-systemd       只装文件，不碰 systemd（容器里用）
  --force            覆盖已存在的配置（默认保留，方便升级）
  --dry-run          只打印将要做什么
  -h, --help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    server | agent)
      [[ -z "$MODE" ]] || die "只能指定一个模式"
      MODE="$1"
      shift
      ;;
    --version) VERSION="${2:?--version 缺少值}"; shift 2 ;;
    --prefix) PREFIX="${2:?--prefix 缺少值}"; shift 2 ;;
    --conf-dir) CONF_DIR="${2:?--conf-dir 缺少值}"; shift 2 ;;
    --from-dir) FROM_DIR="${2:?--from-dir 缺少值}"; shift 2 ;;
    --no-systemd) NO_SYSTEMD=1; shift ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --node) NODE="${2:?--node 缺少值}"; shift 2 ;;
    --listen) LISTEN="${2:?--listen 缺少值}"; shift 2 ;;
    --db) DB="${2:?--db 缺少值}"; shift 2 ;;
    --server) SERVER_URL="${2:?--server 缺少值}"; shift 2 ;;
    --token) TOKEN="${2:?--token 缺少值}"; shift 2 ;;
    --interval) INTERVAL="${2:?--interval 缺少值}"; shift 2 ;;
    --mounts) MOUNTS="${2:?--mounts 缺少值}"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) die "未知参数：$1（用 --help 看用法）" ;;
  esac
done

[[ -n "$MODE" ]] || { usage >&2; exit 2; }

# ---- 参数校验：先挡住明显写错的东西，别装到一半才失败 ----
if [[ "$MODE" == "agent" ]]; then
  [[ -n "$SERVER_URL" ]] || die "agent 模式必须给 --server"
  [[ -n "$NODE" ]] || die "agent 模式必须给 --node"
  [[ -n "$TOKEN" ]] || die "agent 模式必须给 --token"
  [[ "$NODE" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "节点 id 只允许字母、数字、点、下划线、连字符：$NODE"
  [[ "$INTERVAL" =~ ^[0-9]+(s|m|h)$ ]] || die "--interval 形如 30s / 5m / 1h：$INTERVAL"
fi

if ((DRY_RUN == 0)) && [[ $EUID -ne 0 ]]; then
  die "需要 root（要写 $PREFIX、$CONF_DIR 和 systemd 单元）。只是看看的话加 --dry-run。"
fi

# ---- 识别架构 ----
ARCH=""
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  armv7l | armv7) ARCH=armv7 ;;
  *) die "不支持的架构 $(uname -m)（目前提供 amd64 / arm64 / armv7）" ;;
esac

ASSET="simpleprobe-${MODE}_${VERSION}_linux_${ARCH}.tar.gz"

step "计划"
log "  模式      $MODE"
log "  版本      $VERSION"
log "  平台      linux/$ARCH"
log "  包        $ASSET"
log "  二进制    $PREFIX/bin/"
log "  配置      $CONF_DIR/"
if ((NO_SYSTEMD)); then
  log "  systemd   跳过（--no-systemd）"
else
  log "  systemd   probe-$MODE.service"
fi
if ((DRY_RUN)); then
  log "  （--dry-run：不会真的改动系统）"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

step "取包"
if [[ -n "$FROM_DIR" ]]; then
  [[ -f "$FROM_DIR/$ASSET" ]] || die "$FROM_DIR 里没有 $ASSET"
  cp "$FROM_DIR/$ASSET" "$WORK/"
  if [[ -f "$FROM_DIR/SHA256SUMS" ]]; then
    cp "$FROM_DIR/SHA256SUMS" "$WORK/"
  else
    log "  警告：$FROM_DIR 里没有 SHA256SUMS，跳过校验"
  fi
else
  if [[ "$VERSION" == "latest" ]]; then
    BASE="https://github.com/$REPO/releases/latest/download"
  else
    BASE="https://github.com/$REPO/releases/download/$VERSION"
  fi

  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO "$2" "$1"; }
  else
    die "需要 curl 或 wget"
  fi

  log "  下载 $ASSET"
  fetch "$BASE/$ASSET" "$WORK/$ASSET" || die "下载失败：$BASE/$ASSET"
  fetch "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" || die "下载失败：$BASE/SHA256SUMS"
fi

step "校验"
if [[ -f "$WORK/SHA256SUMS" ]]; then
  # 精确匹配文件名，避免把别的包的行也选进来。
  awk -v f="$ASSET" '$2 == f || $2 == "./" f { print }' "$WORK/SHA256SUMS" >"$WORK/.want"
  [[ -s "$WORK/.want" ]] || die "SHA256SUMS 里没有 $ASSET 这一条，拒绝安装"
  (cd "$WORK" && sha256sum -c .want) || die "校验和不匹配，拒绝安装"
  log "  SHA256 校验通过"
else
  log "  跳过（没有 SHA256SUMS）"
fi

step "解包"
tar -xzf "$WORK/$ASSET" -C "$WORK"
[[ -f "$WORK/$MODE" ]] || die "包里没有 $MODE 这个二进制"
[[ -f "$WORK/deploy/probe-$MODE.service" ]] || die "包里没有 systemd 单元"

step "安装"
run install -d -m 0755 "$PREFIX/bin" "$CONF_DIR"
run install -m 0755 "$WORK/$MODE" "$PREFIX/bin/$MODE"

# server 顺带把前端装上，这样 -web 开箱即用。
if [[ "$MODE" == "server" && -d "$WORK/web" ]]; then
  run install -d -m 0755 "$PREFIX/web"
  for f in "$WORK"/web/*; do
    run install -m 0644 "$f" "$PREFIX/web/"
  done
fi

# ---- 配置：默认不覆盖已有文件，这样重跑就是升级 ----
CONF_FILE="$CONF_DIR/$([[ "$MODE" == "server" ]] && echo nodes.yaml || echo agent.yaml)"
WROTE_CONF=0

write_conf() {
  if [[ -e "$CONF_FILE" ]] && ((FORCE == 0)); then
    log "  已存在 $CONF_FILE，保留不动（要覆盖加 --force）"
    return
  fi
  if ((DRY_RUN)); then
    printf '  [dry-run] 写入 %s\n' "$CONF_FILE"
    WROTE_CONF=1
    return
  fi
  cat >"$CONF_FILE"
  # 0644 是有意的：DynamicUser 让服务跑在临时 uid 上，读不了 0600 的
  # root 文件；而 SIGHUP 热加载要求服务能直接读这个路径（LoadCredential
  # 给的是启动时的快照，reload 会读到旧内容）。详见 README 的说明与更严的
  # 替代方案。
  chmod 0644 "$CONF_FILE"
  WROTE_CONF=1
  log "  写入 $CONF_FILE（0644）"
}

if [[ "$MODE" == "server" ]]; then
  if [[ -n "$NODE" ]]; then
    [[ "$NODE" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "节点 id 只允许字母、数字、点、下划线、连字符：$NODE"
    FIRST_TOKEN="$(openssl rand -hex 32)"
    write_conf <<EOF
# 由 install.sh 生成。改完不用重启：systemctl reload probe-server
listen: "$LISTEN"
db: "$DB"

nodes:
  - id: $NODE
    display_name: "$NODE"
    token: "$FIRST_TOKEN"
EOF
  else
    write_conf <<EOF
# 由 install.sh 生成。加节点推荐用 release 包里的 add-node.sh，
# 或者手写一个节点块后 systemctl reload probe-server。
listen: "$LISTEN"
db: "$DB"

nodes: []
EOF
    log "  提示：nodes 还是空的。加第一个节点："
    log "    $PREFIX/bin/server 需要一份带节点的配置——用 --node 重装，或手改上面的文件。"
  fi
else
  write_conf <<EOF
# 由 install.sh 生成。
server: "$SERVER_URL"
node: "$NODE"
token: "$TOKEN"
interval: $INTERVAL
mounts: $MOUNTS
EOF
fi

# ---- 单元文件：prefix/conf-dir 被改写时，同步替换单元里的路径 ----
UNIT_SRC="$WORK/deploy/probe-$MODE.service"
UNIT_DST="/etc/systemd/system/probe-$MODE.service"
UNIT_TMP="$WORK/probe-$MODE.service.rewritten"
sed -e "s|/opt/probe|$PREFIX|g" -e "s|/etc/probe|$CONF_DIR|g" "$UNIT_SRC" >"$UNIT_TMP"

if ((NO_SYSTEMD)); then
  log "  跳过 systemd 安装（--no-systemd）"
  # 不指向临时目录——那个目录马上就没了。要装 systemd 的话模板在 release 包里。
  if [[ "$PREFIX" != "/opt/probe" || "$CONF_DIR" != "/etc/probe" ]]; then
    log "  提示：你的路径与默认值不同，装 systemd 时要把单元模板里的"
    log "        /opt/probe 与 /etc/probe 替换成 $PREFIX 与 $CONF_DIR。"
  fi
else
  run install -m 0644 "$UNIT_TMP" "$UNIT_DST"
  run systemctl daemon-reload
  run systemctl enable --now "probe-$MODE"
fi

# ---- 装完自检：让刚装上的二进制自己解析一次配置 ----
# agent 与 server 都有 -check，所以这一步对两者是同一条路径——不会出现
# 「agent 的自检被静默跳过」这种不对称。
if ((DRY_RUN == 0)); then
  if "$PREFIX/bin/$MODE" -check -config "$CONF_FILE" >/dev/null; then
    log "  自检通过：$MODE -check 能解析 $CONF_FILE"
  else
    die "自检失败：$PREFIX/bin/$MODE -check -config $CONF_FILE 报错，配置没写对"
  fi
fi

step "完成"
if [[ "$MODE" == "server" ]]; then
  log "  面板  http://$LISTEN"
  log "  数据  $DB（首次启动自动创建）"
  if [[ -n "$NODE" ]] && ((DRY_RUN == 0)); then
    TOKEN_SHOWN="$(awk '/token:/{gsub(/[" ]/,"",$2); print $2; exit}' "$CONF_FILE")"
    cat <<EOF

  把这个节点接到被监控机上（token 只显示这一次，也已经写进 $CONF_FILE）：

    curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh \\
      | bash -s -- agent --server http://<server 的地址>:$(
      echo "$LISTEN" | awk -F: '{print $NF}'
    ) --node $NODE --token $TOKEN_SHOWN

EOF
  fi
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status probe-server"
    log "  日志  journalctl -u probe-server -f"
    log "  加机器后不用重启：systemctl reload probe-server"
  fi
else
  log "  节点  $NODE → $SERVER_URL"
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status probe-agent"
    log "  日志  journalctl -u probe-agent -f"
    log "  看是否上线  curl -s $SERVER_URL/api/v1/nodes"
  else
    log "  手动启动  $PREFIX/bin/agent -config $CONF_FILE"
  fi
fi
