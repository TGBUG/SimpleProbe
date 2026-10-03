#!/usr/bin/env bash
# 给 server 加一个节点：生成 token、追加到 nodes.yaml、写出 agent 配置、打印后续命令。
#
# 为什么是「追加」而不是解析后重写 YAML：这里没有 YAML 库，也不该为了一个
# 部署脚本去装一个。追加在「nodes 是文件最后一个键」这个约定下是对的；
# 万一放错了位置，紧接着的 -check 会失败并自动回滚——失败是安全的。
set -euo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

# 打印给运维的那条一键安装命令要用到这两个。
REPO="TGBUG/SimpleProbe"
CONF_DIR="/etc/probe"

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
  add-node.sh --nodes <nodes.yaml> --id <节点 id> [选项]

选项：
  --name <显示名>      默认与 id 相同
  --server-url <URL>   写进生成的 agent 配置；默认从 nodes.yaml 的 listen 推断
  --interval <时长>    上报间隔，如 30s / 5m；合法范围 5s ~ 1h（默认 30s）
  --out <目录>         生成的 agent 配置放哪；默认与 nodes.yaml 同目录
  --bin <路径>         server 二进制路径；默认 <仓库>/bin/server
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

if [[ -z "$NODES" || -z "$ID" ]]; then
  usage >&2
  exit 2
fi
[[ -f "$NODES" ]] || { echo "找不到配置文件：$NODES" >&2; exit 1; }

BIN="${BIN:-$SELF_DIR/../bin/server}"
[[ -x "$BIN" ]] || { echo "找不到可执行的 server：$BIN（先 make build，或用 --bin 指定）" >&2; exit 1; }

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

printf '  - id: %s\n    display_name: "%s"\n    token: "%s"\n' "$ID" "$NAME" "$TOKEN" >>"$NODES"

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
if [[ -z "$SERVER_URL" ]]; then
  listen="$(awk -F'listen:' '/^[[:space:]]*listen:/{gsub(/["[:space:]]/, "", $2); print $2; exit}' "$NODES")"
  listen="${listen:-127.0.0.1:8080}"
  # agent 多数情况下与 server 不同机，0.0.0.0 对它是无意义的地址。
  listen="${listen/0.0.0.0/127.0.0.1}"
  SERVER_URL="http://$listen"
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

cat <<EOF

已添加节点：$ID（$NAME）

  server 端（本机）：
    systemctl reload probe-server        # 等价于 kill -HUP <pid>

  agent 端（在 $ID 这台机器上执行，一条命令搞定）：
    curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh \\
      | bash -s -- agent --server "$SERVER_URL" --node "$ID" --token "$TOKEN"

  上面那条命令会自己下载、校验、装二进制与 systemd 单元并启动。
  不想走网络的话，也可以把这份配置拷过去手装：

    $AGENT_FILE              （拷到目标机的 $CONF_DIR/agent.yaml）

  原配置备份：
    $BACKUP

加好之后打开面板就能看到它。没装好之前它会显示「从未上线」——
这是刻意的：配置里有、数据里没有，正好用来区分「agent 没装好」和「网络不通」。
EOF
