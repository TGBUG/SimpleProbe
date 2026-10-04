#!/usr/bin/env bash
# 给 server 加一个节点：生成 token、追加到 nodes.yaml、写出 agent 配置、打印后续命令。
#
# 为什么是「追加」而不是解析后重写 YAML：这里没有 YAML 库，也不该为了一个
# 部署脚本去装一个。追加在「nodes 是文件最后一个键」这个约定下是对的；
# 万一放错了位置，紧接着的 -check 会失败并自动回滚——失败是安全的。
set -euo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

REPO="TGBUG/SimpleProbe"

BIN=""
NODES=""
ID=""
NAME=""
SERVER_URL=""
OUT_DIR=""
INTERVAL="30s"

usage() {
  cat <<'EOF'
用法：
  add-node.sh --id <节点 id> [选项]

选项：
  --nodes <nodes.yaml> 配置文件；默认 /etc/probe/nodes.yaml
                       （安装时用 --conf-dir 改了路径的话，用这个指过去）
  --name <显示名>      默认与 id 相同
  --server-url <URL>   写进打印出来的 agent 安装命令；默认从 nodes.yaml 的
                       listen 推断，推断出来是回环地址时会提示你手填
  --interval <时长>    上报间隔，如 30s / 5m；合法范围 5s ~ 1h（默认 30s）
  --out <目录>         生成的 agent 配置放哪；默认与 nodes.yaml 同目录
  --bin <路径>         server 二进制路径；默认 <脚本目录>/../bin/server
  -h, --help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --nodes) NODES="${2:?--nodes 缺少值}"; shift 2 ;;
    --id) ID="${2:?--id 缺少值}"; shift 2 ;;
    --name) NAME="${2:?--name 缺少值}"; shift 2 ;;
    --server-url) SERVER_URL="${2:?--server-url 缺少值}"; shift 2 ;;
    --interval) INTERVAL="${2:?--interval 缺少值}"; shift 2 ;;
    --out) OUT_DIR="${2:?--out 缺少值}"; shift 2 ;;
    --bin) BIN="${2:?--bin 缺少值}"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage >&2; exit 2 ;;
  esac
done

# 默认值对齐一键安装的布局：脚本装在 $DIR/bin/，配置就在 $DIR/etc/。
# 两个默认值都从脚本自身位置推出来，所以装到任何目录都不需要额外参数。
NODES="${NODES:-$SELF_DIR/../etc/nodes.yaml}"
# 归一化，免得后面每条提示都拖着一串 ../。
NODES="$(realpath -m "$NODES" 2>/dev/null || printf '%s' "$NODES")"

if [[ -z "$ID" ]]; then
  usage >&2
  exit 2
fi
[[ -f "$NODES" ]] || { echo "找不到配置文件：$NODES（用 --nodes 指定，或先跑一键安装）" >&2; exit 1; }

BIN="${BIN:-$SELF_DIR/../bin/server}"
[[ -x "$BIN" ]] || { echo "找不到可执行的 server：$BIN（先 make build，或用 --bin 指定）" >&2; exit 1; }

# 配置目录跟随 --nodes 的位置，后面提到 agent 配置文件该放哪时才不会说错。
CONF_DIR="$(cd "$(dirname "$NODES")" && pwd)"

NAME="${NAME:-$ID}"
OUT_DIR="${OUT_DIR:-$(dirname "$NODES")}"

# id 字符集与 server 端的 ValidateNodeID 保持一致。
if [[ ! "$ID" =~ ^[A-Za-z0-9._-]{1,64}$ ]]; then
  echo "节点 id 只允许字母、数字、点、下划线、连字符，且不超过 64 字符：$ID" >&2
  exit 2
fi

# 先挡住明显写错的时长，免得生成一份 agent 一定会被 server 拒绝的配置。
if [[ ! "$INTERVAL" =~ ^[0-9]+(s|m|h)$ ]]; then
  echo "interval 只接受 <数字><s|m|h> 形式，例如 30s / 5m / 1h：$INTERVAL" >&2
  exit 2
fi

if grep -qE "^[[:space:]]*-[[:space:]]*id:[[:space:]]*[\"']?${ID}[\"']?[[:space:]]*$" "$NODES"; then
  echo "节点 $ID 已经在 $NODES 里了" >&2
  exit 1
fi

command -v openssl >/dev/null 2>&1 || { echo "需要 openssl 来生成 token" >&2; exit 1; }
TOKEN="$(openssl rand -hex 32)"

BACKUP="$NODES.bak.$(date +%Y%m%d%H%M%S)"
cp -p "$NODES" "$BACKUP"

# 记下原始属主与权限。用 root 跑这个脚本时（很常见：sudo bin/add-node.sh），
# sed -i 会新建一个文件、属主变成 root，而服务是以「安装目录的属主」身份运行的，
# 那样它就再也读不到自己的配置了。改完统一还回去。
ORIG_OWNER="$(stat -c '%u:%g' "$NODES" 2>/dev/null || true)"
ORIG_MODE="$(stat -c '%a' "$NODES" 2>/dev/null || true)"

# 这个脚本的加节点方式是「往文件末尾追加一个列表项」，所以文件末尾必须是
# 裸的 `nodes:`。如果写的是 `nodes: []`（v0.4.x 的 install.sh 就是这么写的），
# 追加会得到非法 YAML：
#     nodes: []
#       - id: web01        ← yaml: did not find expected key
# 所以先把它改写成裸的 `nodes:`。改写在备份之后，校验失败回滚时恢复的是
# 用户原始的文件。
if grep -qE '^[[:space:]]*nodes:[[:space:]]*\[[[:space:]]*\][[:space:]]*$' "$NODES"; then
  sed -i -E 's|^([[:space:]]*nodes:)[[:space:]]*\[[[:space:]]*\][[:space:]]*$|\1|' "$NODES"
  echo "  提示：把配置里的 'nodes: []' 改写成了 'nodes:'（追加列表项需要这种形式）" >&2
fi

printf '  - id: %s\n    display_name: "%s"\n    token: "%s"\n' "$ID" "$NAME" "$TOKEN" >>"$NODES"

restore_meta() {
  [[ -n "$ORIG_OWNER" ]] && chown "$ORIG_OWNER" "$NODES" 2>/dev/null || true
  [[ -n "$ORIG_MODE" ]] && chmod "$ORIG_MODE" "$NODES" 2>/dev/null || true
}
restore_meta

# 用 server 自己的解析器做校验——它才是规则的唯一权威。
if ! "$BIN" -check -config "$NODES" >/dev/null 2>"$OUT_DIR/.add-node.err"; then
  mv -f "$BACKUP" "$NODES"
  {
    echo "新配置没通过校验，已还原 $NODES："
    sed 's/^/    /' "$OUT_DIR/.add-node.err"
  } >&2
  rm -f "$OUT_DIR/.add-node.err"
  exit 1
fi
rm -f "$OUT_DIR/.add-node.err"

# 没给 server-url 就从 nodes.yaml 的 listen 推断。
NEED_MANUAL_URL=0
if [[ -z "$SERVER_URL" ]]; then
  listen="$(awk -F'listen:' '/^[[:space:]]*listen:/{gsub(/["[:space:]]/, "", $2); print $2; exit}' "$NODES")"
  listen="${listen:-127.0.0.1:8080}"
  host="${listen%:*}"
  port="${listen##*:}"
  case "$host" in
    127.0.0.1 | localhost | ::1 | "[::1]")
      # 默认配置就是只监听回环（公网入口交给反代或隧道）。但 agent 在别的机器上，
      # 照搬这个地址会让它往自己身上发数据——**静默失败**，最难查。
      # 所以这里必须提示手填，而不是自作聪明地给一个看起来能用的 URL。
      host="<server 的可达地址>"
      NEED_MANUAL_URL=1
      ;;
    0.0.0.0 | "::" | "[::]")
      host="<server 的可达地址>"
      NEED_MANUAL_URL=1
      ;;
  esac
  SERVER_URL="http://$host:$port"
fi

AGENT_FILE="$OUT_DIR/agent-$ID.yaml"
umask 077
cat >"$AGENT_FILE" <<EOF
# 由 add-node.sh 生成。等价于 install.sh agent 写出来的那份，含明文 token。
server: "$SERVER_URL"
node: "$ID"
token: "$TOKEN"
interval: $INTERVAL
mounts: ["/"]
EOF
chmod 600 "$AGENT_FILE"

if ((NEED_MANUAL_URL)); then
  cat <<EOF

  注意：$NODES 里的 listen 是回环地址，agent 在另一台机器上连不到它。
        下面命令里的 <server 的可达地址> 请替换成 IP 或域名——
        如果 server 前面有反向代理，这里填对外那个 https:// 地址。

EOF
fi

cat <<EOF

已添加节点：$ID（$NAME）

  server 端（本机）：
    systemctl reload simple-probe-server        # 等价于 kill -HUP <pid>

  agent 端（在 $ID 这台机器上执行，一条命令搞定）：
    curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh \\
      | sudo bash -s -- agent --server "$SERVER_URL" --node "$ID" --token "$TOKEN"

  上面那条命令会自己下载、校验、装二进制与 systemd 单元并启动。
  不想走网络的话，这份配置可以拷过去手装：

    $AGENT_FILE              →  目标机的 $CONF_DIR/agent.yaml

  手装的话：保持 0600，并确保属主是运行服务的那个用户（也就是安装目录的
  属主），否则服务读不到。install.sh 会自动处理好这些。

  原配置备份：
    $BACKUP

加好之后打开面板就能看到它。没装好之前它会显示「从未上线」——
这是刻意的：配置里有、数据里没有，正好用来区分「agent 没装好」和「网络不通」。
EOF
