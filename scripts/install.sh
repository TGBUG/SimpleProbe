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
  --listen <addr>    监听地址，默认 127.0.0.1:8080
  --db <path>        数据库路径，默认 /var/lib/probe/probe.db

  服务端装完就是空的（nodes 为空列表），加节点用随包安装的 add-node.sh——
  它一次加一个、可以反复用，比安装参数里塞一个节点方便。

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

# server 模式不再收 --node。它只能加一个节点，装第二台机器时还得改配置重装，
# 而 add-node.sh 可以反复用。给了就明确说清楚，别默默忽略。
if [[ "$MODE" == "server" && -n "$NODE" ]]; then
  die "server 模式不再接受 --node。装完 server 后用 add-node.sh 加节点（可反复用）：
    $PREFIX/bin/add-node.sh --id <节点 id> --name \"<显示名>\"
或者手写节点块后 systemctl reload simple-probe-server。"
fi

# 需要的不是 root 这个身份，而是"能写到目标位置"——所以按可写性判断，
# 而不是按 EUID。真正只有 root 才能做的，是装 systemd 单元。
#
# 这条是被 CI 逼出来的：本地开发沙箱是 root，GitHub runner 不是，同一段脚本
# 在两边行为不同（本地全绿、CI 直接 die）。改判可写性之后，CI 的非 root 环境
# 反而成了这段逻辑的回归哨兵。
can_write() {
  local d="$1"
  while [[ ! -e "$d" && "$d" != "/" ]]; do d="$(dirname "$d")"; done
  [[ -w "$d" ]]
}

if ((DRY_RUN == 0)); then
  if ((NO_SYSTEMD == 1)) && can_write "$PREFIX" && can_write "$CONF_DIR"; then
    : # 只装文件、目标可写，谁跑都行
  elif [[ $EUID -ne 0 ]]; then
    die "需要 root：要么要装 systemd 单元，要么 $PREFIX / $CONF_DIR 对当前用户不可写。
      · 只看计划：     加 --dry-run
      · 无 root 安装： 加 --no-systemd，并把 --prefix 与 --conf-dir 指到可写目录"
  fi
fi

# ---- 识别架构 ----
ARCH=""
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  armv7l | armv7) ARCH=armv7 ;;
  *) die "不支持的架构 $(uname -m)（目前提供 amd64 / arm64 / armv7）" ;;
esac

# ---- 取包工具 ----
if command -v curl >/dev/null 2>&1; then
  fetch_to() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }
  fetch_stdout() { curl -fsSL --retry 3 --connect-timeout 15 "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch_to() { wget -qO "$2" "$1"; }
  fetch_stdout() { wget -qO- "$1"; }
else
  [[ -n "$FROM_DIR" ]] || die "需要 curl 或 wget"
  fetch_to() { die "需要 curl 或 wget"; }
  fetch_stdout() { die "需要 curl 或 wget"; }
fi

# ---- 解析 latest ----
#
# GitHub 的 /releases/latest/download/<file> 只在**文件名固定**时才有用；
# 我们的包名里带版本号，所以必须先把真实的 tag 问出来，否则会去下载
# simpleprobe-server_latest_linux_amd64.tar.gz 这种不存在的名字（404）。
if [[ -z "$FROM_DIR" && "$VERSION" == "latest" ]]; then
  log "查询最新版本…"
  api_json="$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest")" ||
    die "查询最新版本失败（可能被限流）。用 --version <tag> 显式指定即可绕开。"
  VERSION="$(printf '%s' "$api_json" |
    grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | cut -d'"' -f4)"
  [[ -n "$VERSION" ]] ||
    die "没能从 API 响应里解析出 tag_name。用 --version <tag> 显式指定即可绕开。"
  log "最新版本是 $VERSION"
fi

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
  # VERSION 已在上面解析成真实 tag（latest 不是文件名的一部分）。
  BASE="https://github.com/$REPO/releases/download/$VERSION"

  log "  下载 $ASSET"
  fetch_to "$BASE/$ASSET" "$WORK/$ASSET" || die "下载失败：$BASE/$ASSET"
  fetch_to "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" || die "下载失败：$BASE/SHA256SUMS"
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
[[ -f "$WORK/deploy/simple-probe-$MODE.service" ]] || die "包里没有 systemd 单元"

step "安装"
run install -d -m 0755 "$PREFIX/bin" "$CONF_DIR"
run install -m 0755 "$WORK/$MODE" "$PREFIX/bin/$MODE"

# 加节点的脚本对 server 是必需品：装完 nodes 是空的，没有它就没法加机器。
# 它默认从自己所在目录的 ../bin/ 找 server 二进制，放在 $PREFIX/bin/ 下刚好对上。
if [[ "$MODE" == "server" && -f "$WORK/add-node.sh" ]]; then
  # 与单元文件同样处理：--conf-dir 改过路径时，把脚本里默认的 /etc/probe
  # 一并换掉，否则装到自定义位置的用户每次都得自己传 --nodes。
  sed "s|/etc/probe|$CONF_DIR|g" "$WORK/add-node.sh" >"$WORK/add-node.sh.rewritten"
  run install -m 0755 "$WORK/add-node.sh.rewritten" "$PREFIX/bin/add-node.sh"
fi

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
  write_conf <<EOF
# 由 install.sh 生成。
#
# nodes 一开始是空的，这是正常的：装完 server 再用 add-node.sh 逐个加节点。
# 写作 "nodes:"（而不是 "nodes: []"）是有意的——add-node.sh 的策略是往文件
# 末尾追加列表项，"[]" 后面跟块序列是非法 YAML。
#
# 加节点：$PREFIX/bin/add-node.sh --id <节点 id> --name "<显示名>"
# 加完不用重启：systemctl reload simple-probe-server
listen: "$LISTEN"
db: "$DB"

nodes:
EOF
else
  write_conf <<EOF
# 由 install.sh 生成。
#
# 必须是 0644：DynamicUser 让服务跑在临时 uid 上，读不了 0600 的 root 文件。
# 见 README 里关于配置权限的说明与更严的替代方案。
server: "$SERVER_URL"
node: "$NODE"
token: "$TOKEN"
interval: $INTERVAL
mounts: $MOUNTS
EOF
fi

# ---- 单元文件：prefix/conf-dir 被改写时，同步替换单元里的路径 ----
# 服务名带 simple- 前缀，避免与系统上别的 probe 服务撞名。
UNIT_SRC="$WORK/deploy/simple-probe-$MODE.service"
UNIT_DST="/etc/systemd/system/simple-probe-$MODE.service"
UNIT_TMP="$WORK/simple-probe-$MODE.service.rewritten"
UNIT_OLD="/etc/systemd/system/probe-$MODE.service"
sed -e "s|/opt/probe|$PREFIX|g" -e "s|/etc/probe|$CONF_DIR|g" "$UNIT_SRC" >"$UNIT_TMP"

if ((NO_SYSTEMD)); then
  log "  跳过 systemd 安装（--no-systemd）"
  # 不指向临时目录——那个目录马上就没了。要装 systemd 的话模板在 release 包里。
  if [[ "$PREFIX" != "/opt/probe" || "$CONF_DIR" != "/etc/probe" ]]; then
    log "  提示：你的路径与默认值不同，装 systemd 时要把单元模板里的"
    log "        /opt/probe 与 /etc/probe 替换成 $PREFIX 与 $CONF_DIR。"
  fi
else
  # v0.4.x 装过的话旧单元还叫 probe-*。两个服务抢同一个端口，新的会起不来，
  # 所以先停掉再删掉旧的。
  if [[ -e "$UNIT_OLD" ]]; then
    log "  发现旧单元 probe-$MODE.service（v0.4.x 的服务名），先停掉并移除"
    run systemctl disable --now "probe-$MODE" 2>/dev/null || true
    run rm -f "$UNIT_OLD"
  fi
  run install -m 0644 "$UNIT_TMP" "$UNIT_DST"
  run systemctl daemon-reload
  run systemctl enable --now "simple-probe-$MODE"
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
  cat <<EOF

  现在配置里还没有节点。加一台机器（可以反复执行，一次加一个）：

    $PREFIX/bin/add-node.sh --id <节点 id> --name "<显示名>"

  它会生成 token、写进 $CONF_FILE，并打印 agent 侧那条一键安装命令。

EOF
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status simple-probe-server"
    log "  日志  journalctl -u simple-probe-server -f"
    log "  加完节点不用重启：systemctl reload simple-probe-server"
  fi
else
  log "  节点  $NODE → $SERVER_URL"
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status simple-probe-agent"
    log "  日志  journalctl -u simple-probe-agent -f"
    log "  看是否上线  curl -s $SERVER_URL/api/v1/nodes"
  else
    log "  手动启动  $PREFIX/bin/agent -config $CONF_FILE"
  fi
fi
