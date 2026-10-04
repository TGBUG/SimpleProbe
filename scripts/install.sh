#!/usr/bin/env bash
# SimpleProbe 一键安装 / 升级 / 卸载。
#
# 布局：除了 systemd 单元（它只能待在 /etc/systemd/system），所有文件都在同一个
# 目录里——二进制、配置、数据、前端。那个目录默认就是当前目录，可用 --dir 指定。
#
#   $DIR/
#   ├── bin/    server 或 agent，外加 add-node.sh（仅 server）
#   ├── etc/    nodes.yaml / agent.yaml
#   ├── data/   probe.db（仅 server）
#   └── web/    前端（仅 server）
#
# 服务以「安装目录的属主」身份运行，所以不用新建账户，配置也能保持 0600。
#
# agent：
#   curl -fsSL https://github.com/TGBUG/SimpleProbe/releases/latest/download/install.sh \
#     | sudo bash -s -- agent --server https://probe.example.com --node web01 --token <token>
#
# server（先 cd 到想装的地方，或者用 --dir）：
#   curl -fsSL .../install.sh | sudo bash -s -- server --dir /opt/simpleprobe
#
# 卸载（只清 systemd 配置，不删文件）：
#   curl -fsSL .../install.sh | sudo bash -s -- uninstall server
set -euo pipefail

REPO="TGBUG/SimpleProbe"
# systemd 单元只能待在这儿（这是唯一一个不在安装目录里的文件）。
# 允许用环境变量覆盖，方便在容器里试装、也让 e2e 能验证卸载逻辑。
UNIT_DIR="${SIMPLEPROBE_UNIT_DIR:-/etc/systemd/system}"

MODE=""
DIR=""
VERSION="latest"
FROM_DIR=""
NO_SYSTEMD=0
FORCE=0
DRY_RUN=0

# server
LISTEN="127.0.0.1:8080"

# agent
SERVER_URL=""
NODE=""
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
# 需要的不是 root 身份，而是"能写到目标位置"。
can_write() {
  local d="$1"
  while [[ ! -e "$d" && "$d" != "/" ]]; do d="$(dirname "$d")"; done
  [[ -w "$d" ]]
}

usage() {
  cat <<'EOF'
用法：
  install.sh server    [选项]
  install.sh agent     --server <URL> --node <id> --token <token> [选项]
  install.sh uninstall <server|agent> [选项]

通用选项：
  --dir <目录>       安装目录，默认当前目录。二进制/配置/数据/前端都在这里面，
                     systemd 单元仍然装在 /etc/systemd/system（挪不了）。
  --version <tag>    release 版本，默认 latest
  --from-dir <目录>  不从网络下载，改从这个本地目录取包（离线安装 / 本地测试）
  --no-systemd       只装文件，不碰 systemd（容器里用）
  --force            覆盖已存在的配置（默认保留，方便升级）
  --dry-run          只打印将要做什么
  -h, --help

server 选项：
  --listen <addr>    监听地址，默认 127.0.0.1:8080
                     服务端装完 nodes 是空的，加节点用安装目录里的 bin/add-node.sh。

agent 选项：
  --server <URL>     server 地址，必填
  --node <id>        节点 id，必填，必须与 server 端配置一致
  --token <token>    server 端为该节点配置的 token，必填
  --interval <时长>  上报间隔，默认 30s（合法范围 5s ~ 1h）
  --mounts <JSON>    监控的挂载点，默认 ["/"]

uninstall：
  停掉并删除对应的 systemd 单元，**不删任何数据或配置文件**。
  跑完会告诉你安装目录在哪，想删自己删。
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    server | agent | uninstall)
      if [[ "$1" == "uninstall" ]]; then
        MODE="uninstall"
        UNINSTALL_TARGET="${2:?uninstall 后面要跟 server 或 agent}"
        [[ "$UNINSTALL_TARGET" == "server" || "$UNINSTALL_TARGET" == "agent" ]] ||
          die "uninstall 只支持 server 或 agent：$UNINSTALL_TARGET"
        shift 2
      else
        [[ -z "$MODE" || "$MODE" == "uninstall" ]] || die "只能指定一个模式"
        MODE="$1"
        shift
      fi
      ;;
    --dir) DIR="${2:?--dir 缺少值}"; shift 2 ;;
    --version) VERSION="${2:?--version 缺少值}"; shift 2 ;;
    --from-dir) FROM_DIR="${2:?--from-dir 缺少值}"; shift 2 ;;
    --no-systemd) NO_SYSTEMD=1; shift ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --listen) LISTEN="${2:?--listen 缺少值}"; shift 2 ;;
    --server) SERVER_URL="${2:?--server 缺少值}"; shift 2 ;;
    --node) NODE="${2:?--node 缺少值}"; shift 2 ;;
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

# server 模式不再收 --node（旧版参数）。它只能加一个节点，装第二台机器还得
# 改配置重装，而 add-node.sh 可以反复用。给了就明确说清楚，别默默忽略。
if [[ "$MODE" == "server" && -n "$NODE" ]]; then
  die "server 模式不接受 --node。装完用 bin/add-node.sh 加节点（可反复用）。"
fi

# ---- 卸载：只清 systemd，不碰别的 ----
if [[ "$MODE" == "uninstall" ]]; then
  if ((DRY_RUN == 0)) && ! can_write "$UNIT_DIR"; then
    [[ $EUID -eq 0 ]] || die "卸载要动 $UNIT_DIR，需要 root。只是看看加 --dry-run。"
  fi

  unit="simple-probe-$UNINSTALL_TARGET"
  unit_file="$UNIT_DIR/$unit.service"
  old_file="$UNIT_DIR/probe-$UNINSTALL_TARGET.service"

  step "卸载 $UNINSTALL_TARGET"
  log "  只清 systemd 配置，不删任何文件。"

  # 先从单元里读出安装目录，这样能明确告诉运维文件在哪。
  inst=""
  [[ -f "$unit_file" ]] &&
    inst="$(awk -F= '/^WorkingDirectory=/{print $2; exit}' "$unit_file")"

  if [[ ! -f "$unit_file" && ! -f "$old_file" ]]; then
    log "  没找到 $unit.service（旧名的 probe-$UNINSTALL_TARGET.service 也没有），本来就没装。"
    exit 0
  fi

  if ((NO_SYSTEMD)); then
    log "  --no-systemd：只删单元文件，不碰 systemctl"
  elif [[ -f "$unit_file" ]]; then
    run systemctl disable --now "$unit" 2>/dev/null || true
  fi
  if [[ -f "$unit_file" ]]; then
    run rm -f "$unit_file"
  fi
  if [[ -f "$old_file" ]]; then
    log "  顺带清掉旧服务名 probe-$UNINSTALL_TARGET.service"
    run rm -f "$old_file"
  fi
  ((NO_SYSTEMD)) || run systemctl daemon-reload

  log "  已移除 systemd 配置。"
  if [[ -n "$inst" ]]; then
    printf '\n  文件都还在（这是有意的）：%s\n' "$inst"
    printf '  确认不要了就自己删：rm -rf %s\n' "$inst"
  else
    printf '\n  安装目录没能从单元文件里读出来，文件应该还在你当初指定的 --dir 里。\n'
  fi
  exit 0
fi

# ---- 解析安装目录 ----
# 默认当前目录：想装哪就 cd 到哪，或者用 --dir 明说。
DIR="${DIR:-$PWD}"
DIR="$(realpath -m "$DIR" 2>/dev/null || printf '%s' "$DIR")"

# 这些目录下面直接摊开 bin/ etc/ data/ 会把系统搞乱，明确拒绝。
case "$DIR" in
  / | /bin | /boot | /dev | /etc | /home | /lib | /lib64 | /opt | /proc | /root | /run | /sbin | /srv | /sys | /usr | /var)
    die "拒绝把 $DIR 当安装目录：那是系统目录本身。
     换一个专用子目录，例如 --dir $DIR/simpleprobe"
    ;;
esac
# 单元文件里这些路径是裸写的，带空格/冒号会写出一个坏单元。
case "$DIR" in
  *[[:space:]]* | *:*)
    die "安装目录不能含空格或冒号：$DIR（systemd 单元里没法安全表达）"
    ;;
esac

# ---- 运行身份：安装目录的属主 ----
# 以 sudo 跑就用调用者；直接用 root 跑时，如果目录已经属于某个普通用户就用那个人。
RUN_USER="${SUDO_USER:-$(id -un)}"
if [[ "$RUN_USER" == "root" && -d "$DIR" ]]; then
  owner="$(stat -c '%U' "$DIR" 2>/dev/null || true)"
  [[ -n "$owner" && "$owner" != "root" ]] && RUN_USER="$owner"
fi

# ---- 权限：按可写性判断，而不是按 EUID ----
# 这条是被 CI 逼出来的：本地开发沙箱是 root，GitHub runner 不是，同一段脚本
# 在两边行为不同（本地全绿、CI 直接 die）。真正只有 root 才能做的，是装 systemd。
if ((DRY_RUN == 0)); then
  if ((NO_SYSTEMD == 1)) && can_write "$DIR" && can_write "$UNIT_DIR" 2>/dev/null; then
    : # 只装文件且目标可写，谁跑都行
  elif ((NO_SYSTEMD == 1)) && can_write "$DIR"; then
    :
  elif [[ $EUID -ne 0 ]]; then
    die "需要 root：要么要装 systemd 单元，要么 $DIR 对当前用户不可写。
     · 只看计划：     加 --dry-run
     · 无 root 安装： 加 --no-systemd，并把 --dir 指到可写目录"
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
log "  安装目录  $DIR"
log "  运行身份  $RUN_USER"
if ((NO_SYSTEMD)); then
  log "  systemd   跳过（--no-systemd）"
else
  log "  systemd   $UNIT_DIR/simple-probe-$MODE.service"
fi
if [[ "$RUN_USER" == "root" ]]; then
  log ""
  log "  注意：解析出来的运行用户是 root，服务将以 root 运行。"
  log "       想避免的话，用普通用户身份装到 ta 自己的目录（默认就是当前目录）。"
fi
((DRY_RUN)) && log "  （--dry-run：不会真的改动系统）"

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

step "安装到 $DIR"
run install -d -m 0755 "$DIR/bin" "$DIR/etc"

if [[ "$MODE" == "server" ]]; then
  run install -d -m 0755 "$DIR/web"
  for f in "$WORK"/web/*; do
    run install -m 0644 "$f" "$DIR/web/"
  done
  # 数据目录交给服务写，所以属主要对。
  run install -d -m 0755 "$DIR/data"
  # 加节点的脚本对 server 是必需品：装完 nodes 是空的，没有它就没法加机器。
  # 它默认从自己所在目录的 ../ 找 server 与配置，放在 bin/ 下刚好对上。
  [[ -f "$WORK/add-node.sh" ]] && run install -m 0755 "$WORK/add-node.sh" "$DIR/bin/add-node.sh"
fi

run install -m 0755 "$WORK/$MODE" "$DIR/bin/$MODE"

# ---- 配置：默认不覆盖已有文件，这样重跑就是升级 ----
CONF_FILE="$DIR/etc/$([[ "$MODE" == "server" ]] && echo nodes.yaml || echo agent.yaml)"

write_conf() {
  if [[ -e "$CONF_FILE" ]] && ((FORCE == 0)); then
    log "  已存在 $CONF_FILE，保留不动（要覆盖加 --force）"
    return
  fi
  if ((DRY_RUN)); then
    printf '  [dry-run] 写入 %s\n' "$CONF_FILE"
    return
  fi
  cat >"$CONF_FILE"
  # 服务以安装目录属主的身份运行，所以这个文件可以保持只有本人可读。
  chmod 0600 "$CONF_FILE"
  log "  写入 $CONF_FILE（0600）"
}

if [[ "$MODE" == "server" ]]; then
  write_conf <<EOF
# 由 install.sh 生成。
#
# nodes 一开始是空的，这是正常的：装完 server 再用 bin/add-node.sh 逐个加节点。
# 写作 "nodes:"（而不是 "nodes: []"）是有意的——add-node.sh 的策略是往文件
# 末尾追加列表项，"[]" 后面跟块序列是非法 YAML。
#
# 加节点：$DIR/bin/add-node.sh --id <节点 id> --name "<显示名>"
# 加完不用重启：systemctl reload simple-probe-server
#
# 单节点每分钟上报次数上限，默认 60。它拦的是失控的死循环，不是访问控制；
# 调小到低于 (60/interval)*3 会让正常节点也被限流。
# report_per_minute: 60
listen: "$LISTEN"
db: "$DIR/data/probe.db"

nodes:
EOF
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

# ---- 属主：先交给运行用户，再启动服务 ----
# 顺序很重要：服务以 RUN_USER 身份运行。如果配置还是 root 属主的 0600，
# 服务第一次启动就会读不到它——所以必须在 systemctl start 之前还回去。
# 只动我们管理的这几个子目录，不对 $DIR 整体递归——$DIR 可能是你的已有目录。
if [[ "$RUN_USER" != "$(id -un)" ]] && ((DRY_RUN == 0)); then
  for sub in bin etc data web; do
    [[ -e "$DIR/$sub" ]] && chown -R "$RUN_USER" "$DIR/$sub"
  done
  log "  安装目录已交给 $RUN_USER（之后改配置不用 sudo）"
fi

# ---- 单元文件 ----
# 模板里的路径与用户名在这里替换成实际值；服务名带 simple- 前缀，
# 避免与系统上别的 probe 服务撞名。
UNIT_SRC="$WORK/deploy/simple-probe-$MODE.service"
UNIT_DST="$UNIT_DIR/simple-probe-$MODE.service"
UNIT_TMP="$WORK/simple-probe-$MODE.service.rewritten"
UNIT_OLD="$UNIT_DIR/probe-$MODE.service"
sed -e "s|/opt/simpleprobe|$DIR|g" -e "s|^User=probe$|User=$RUN_USER|" \
  "$UNIT_SRC" >"$UNIT_TMP"

if ((NO_SYSTEMD)); then
  log "  跳过 systemd 安装（--no-systemd）"
  log "  手动启动：$DIR/bin/$MODE -config $CONF_FILE"
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
  if "$DIR/bin/$MODE" -check -config "$CONF_FILE" >/dev/null; then
    log "  自检通过：$MODE -check 能解析 $CONF_FILE"
  else
    die "自检失败：$DIR/bin/$MODE -check -config $CONF_FILE 报错，配置没写对"
  fi
fi

step "完成"
if [[ "$MODE" == "server" ]]; then
  log "  面板  http://$LISTEN"
  log "  数据  $DIR/data/probe.db（首次启动自动创建）"
  cat <<EOF

  现在配置里还没有节点。加一台机器（可以反复执行，一次加一个）：

    $DIR/bin/add-node.sh --id <节点 id> --name "<显示名>"

  它会生成 token、写进 $CONF_FILE，并打印 agent 侧那条一键安装命令。

EOF
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status simple-probe-server"
    log "  日志  journalctl -u simple-probe-server -f"
    log "  加完节点不用重启：systemctl reload simple-probe-server"
    log "  卸载  bash install.sh uninstall server"
  fi
else
  log "  节点  $NODE → $SERVER_URL"
  if ((NO_SYSTEMD == 0)); then
    log "  状态  systemctl status simple-probe-agent"
    log "  日志  journalctl -u simple-probe-agent -f"
    log "  看是否上线  curl -s $SERVER_URL/api/v1/nodes"
    log "  卸载  bash install.sh uninstall agent"
  else
    log "  手动启动  $DIR/bin/agent -config $CONF_FILE"
  fi
fi
