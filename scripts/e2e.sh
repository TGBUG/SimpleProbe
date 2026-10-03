#!/usr/bin/env bash
# 端到端验证：起 server + agent，检查数据落库、在线判定与在线率的反向验证。
#
# 用法：scripts/e2e.sh（推荐用 `make e2e`，它会把 local.mk 里的 Go 环境带进来）
# 之所以把“停掉 agent 后在线率必须下降”也放进 v0.1，是因为它一次性验证了
# 整条链路：agent 停止上报 → server 判定离线 → 分母裁剪正确 → 前端拿到数字。
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
WORK="$ROOT/.e2e"
PORT=${PORT:-18080}
INTERVAL=${INTERVAL:-5s}
TOKEN="e2e-token"

# 这里刻意不设置 GOCACHE / GOPATH / GOTMPDIR：那属于调用方的环境。
# 曾经硬编码成 "$ROOT/../.gotmp"，本地能用，CI 上因为目录不存在直接报
# "go: creating work dir: ... no such file or directory"（Go 只创建
# GOCACHE，不创建 GOTMPDIR）。外部若指定了它们，这里只负责把目录建出来。
for d in "${GOCACHE:-}" "${GOPATH:-}" "${GOTMPDIR:-}"; do
  [[ -n "$d" ]] && mkdir -p "$d"
done

SERVER_PID=""
AGENT_PID=""
AGENT2=""
INST_SRV=""
INST_AG=""
cleanup() {
  [[ -n "$INST_AG" ]] && kill "$INST_AG" 2>/dev/null || true
  [[ -n "$INST_SRV" ]] && kill "$INST_SRV" 2>/dev/null || true
  [[ -n "$AGENT2" ]] && kill "$AGENT2" 2>/dev/null || true
  [[ -n "$AGENT_PID" ]] && kill "$AGENT_PID" 2>/dev/null || true
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "== 构建 =="
rm -rf "$WORK"
mkdir -p "$WORK/bin"
go build -o "$WORK/bin/server" ./cmd/server
go build -o "$WORK/bin/agent" ./cmd/agent

echo "== 写配置 =="
cat >"$WORK/nodes.yaml" <<EOF
listen: "127.0.0.1:$PORT"
db: "$WORK/probe.db"
nodes:
  - id: local
    display_name: "本机"
    token: "$TOKEN"
EOF

cat >"$WORK/agent.yaml" <<EOF
server: "http://127.0.0.1:$PORT"
node: "local"
token: "$TOKEN"
interval: $INTERVAL
mounts: ["/"]
EOF

echo "== 启动 server =="
"$WORK/bin/server" -config "$WORK/nodes.yaml" -web "$ROOT/web" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

echo "== 启动 agent =="
"$WORK/bin/agent" -config "$WORK/agent.yaml" >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!

echo "== 等待 3 个上报周期 =="
sleep 16

curl -fsS "http://127.0.0.1:$PORT/api/v1/nodes" -o "$WORK/nodes-online.json"
python3 - "$WORK/nodes-online.json" <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
n = d["nodes"][0]
m = n["metrics"]
print("  node:", n["id"], "/", n["name"], " online:", n["online"])
print("  load:", m["load"])
print("  cpu_pct:", m["cpu_pct"])
print("  mem:", m["mem"])
print("  disk:", m["disk"])
print("  uptime_s:", m["uptime_s"], " agent:", m["agent_version"])
print("  rates:", n["uptime"])

assert n["online"] is True, "agent 正在跑，应显示在线"
assert m is not None, "应有指标"
assert len(m["load"]) == 3, "负载应有三项"
assert 0 <= m["cpu_pct"] <= 100, "CPU 使用率应在 [0,100]"
assert m["mem"]["total"] > 0 and m["mem"]["used"] <= m["mem"]["total"]
assert m["disk"] and m["disk"][0]["total"] > 0, "磁盘用量应为正"
assert m["disk"][0]["mount"] == "/"
assert n["uptime"]["24h"] == 1, "刚接入且满勤，24h 在线率应为 1"
PY

echo "== 反向验证：停掉 agent =="
kill "$AGENT_PID"
wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""
sleep 16

curl -fsS "http://127.0.0.1:$PORT/api/v1/nodes" -o "$WORK/nodes-offline.json"
python3 - "$WORK/nodes-offline.json" <<'PY'
import json, sys

n = json.load(open(sys.argv[1]))["nodes"][0]
print("  online:", n["online"], " rates:", n["uptime"])
assert n["online"] is False, "agent 已停，应判定为离线"
assert n["uptime"]["24h"] < 1, "停掉之后在线率必须下降"
PY

echo "== 拒绝非法上报 =="
VALID='{"v":1,"node":"local","interval_s":5,"load":[1,2,3],"cpu_pct":1,"mem":{"used":1,"total":2},"disk":[{"mount":"/","used":1,"total":2}],"uptime_s":1,"agent_version":"e2e"}'

curl -sS -o "$WORK/reject.json" -w "  合法载荷但无 token => HTTP %{http_code}\n" \
  -X POST "http://127.0.0.1:$PORT/api/v1/report" \
  -H 'Content-Type: application/json' -d "$VALID"
python3 - "$WORK/reject.json" <<'PY'
import json, sys
e = json.load(open(sys.argv[1]))
assert e["code"] == "unauthorized", e
PY

curl -sS -o "$WORK/reject2.json" -w "  未知字段          => HTTP %{http_code} " \
  -X POST "http://127.0.0.1:$PORT/api/v1/report" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  -d "${VALID%\}},\"evil\":1}"
cat "$WORK/reject2.json"; echo
python3 - "$WORK/reject2.json" <<'PY'
import json, sys
e = json.load(open(sys.argv[1]))
assert e["code"] == "unknown_field", e
# 控制字符不该出现在给对端的描述里。
assert all(ord(c) >= 0x20 and ord(c) != 0x7f for c in e["message"] + e["field"])
PY

echo "== 历史曲线 =="
FROM=$(( $(date +%s) - 120 ))
TO=$(date +%s)
curl -fsS "http://127.0.0.1:$PORT/api/v1/series?node=local&metric=cpu_pct&from=$FROM&to=$TO&step=5" \
  -o "$WORK/series.json"
python3 - "$WORK/series.json" <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
print("  metric:", d["metric"], " step:", d["step"], " points:", len(d["points"]))
print("  首尾:", d["points"][0] if d["points"] else None, d["points"][-1] if d["points"] else None)

assert d["metric"] == "cpu_pct"
assert len(d["points"]) >= 2, "应至少有两个点"
for ts, v in d["points"]:
    assert ts % d["step"] == 0, f"时间戳 {ts} 不是 step {d['step']} 的整数倍"
    assert 0 <= v <= 100, f"cpu_pct 越界: {v}"
ts_list = [p[0] for p in d["points"]]
assert ts_list == sorted(ts_list), "时间戳应递增"
PY

# 内存百分比是查询时算出来的，顺手验一下口径。
curl -fsS "http://127.0.0.1:$PORT/api/v1/series?node=local&metric=mem_pct&from=$FROM&to=$TO&step=5" \
  -o "$WORK/series-mem.json"
python3 - "$WORK/series-mem.json" <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
print("  mem_pct 首点:", d["points"][0] if d["points"] else None)
assert d["points"], "内存曲线不应为空"
for _, v in d["points"]:
    assert 0 <= v <= 100, f"mem_pct 越界: {v}"
PY

curl -sS -o /dev/null -w "  未知指标 => HTTP %{http_code}\n" \
  "http://127.0.0.1:$PORT/api/v1/series?node=local&metric=nope"
curl -sS -o /dev/null -w "  未知节点 => HTTP %{http_code}\n" \
  "http://127.0.0.1:$PORT/api/v1/series?node=ghost"

echo "== 加一台新机：改配置 + SIGHUP + 装 agent =="
"$ROOT/scripts/add-node.sh" \
  --nodes "$WORK/nodes.yaml" --id nas --name "NAS · 家里" --interval 5s \
  --server-url "http://127.0.0.1:$PORT" --out "$WORK" --bin "$WORK/bin/server" >"$WORK/add-node.log"
tail -6 "$WORK/add-node.log"

python3 - "$WORK/nodes.yaml" <<'PY'
import sys

text = open(sys.argv[1], encoding="utf-8").read()
assert "id: nas" in text, "新节点没写进配置"
assert text.count("token:") == 2, "token 条数不对"
assert "id: local" in text, "原有节点被弄丢了"
PY

# 热加载：不重启进程。
kill -HUP "$SERVER_PID"
sleep 1

curl -fsS "http://127.0.0.1:$PORT/api/v1/nodes" -o "$WORK/nodes-after-hup.json"
python3 - "$WORK/nodes-after-hup.json" <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
ids = [n["id"] for n in d["nodes"]]
print("  SIGHUP 之后节点列表:", ids)
assert ids == ["local", "nas"], f"热加载后节点列表不对: {ids}"

nas = d["nodes"][1]
assert nas["name"] == "NAS · 家里"
assert nas["metrics"] is None, "还没装 agent，应显示从未上线"
PY

# 把生成的 agent 配置装上，验证「装 agent」这一步真能接通。
"$WORK/bin/agent" -config "$WORK/agent-nas.yaml" >"$WORK/agent-nas.log" 2>&1 &
AGENT2=$!
sleep 12

curl -fsS "http://127.0.0.1:$PORT/api/v1/nodes" -o "$WORK/nodes-two.json"
python3 - "$WORK/nodes-two.json" <<'PY'
import json, sys

d = json.load(open(sys.argv[1]))
nas = [n for n in d["nodes"] if n["id"] == "nas"][0]
print("  nas online:", nas["online"], " metrics:", "有" if nas["metrics"] else "无")
assert nas["online"] is True, "新加的节点应已上线"
assert nas["metrics"] is not None, "新节点应有指标"
PY
kill "$AGENT2" 2>/dev/null || true
wait "$AGENT2" 2>/dev/null || true
AGENT2=""

echo "== 落库检查 =="
python3 - "$WORK/probe.db" <<'PY'
import sqlite3, sys

c = sqlite3.connect(sys.argv[1])
for label, q in [
    ("sample 明细", "SELECT COUNT(*) FROM sample"),
    ("uptime_bucket", "SELECT COUNT(*) FROM uptime_bucket"),
    ("node_state", "SELECT COUNT(*) FROM node_state"),
]:
    print(f"  {label}: {c.execute(q).fetchone()[0]}")
PY

echo "== 页面可访问 =="
for path in / /detail.html?node=local /app.css /app.js; do
  curl -fsS -o /dev/null -w "  GET $path => HTTP %{http_code}\n" "http://127.0.0.1:$PORT$path"
done
curl -fsS -o /dev/null -w "  GET /api/v1/health => HTTP %{http_code}\n" "http://127.0.0.1:$PORT/api/v1/health"

echo "== 一键安装：用 release 包装进临时前缀并跑起来 =="
# 安装脚本现在是主要的部署路径，它坏了就什么都装不上，所以纳入验收。
# 用 --no-systemd + 自定义前缀把它限制在临时目录里，不碰宿主系统。
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64) HOST_ARCH=amd64 ;;
  aarch64 | arm64) HOST_ARCH=arm64 ;;
  armv7l) HOST_ARCH=armv7 ;;
esac

PLATFORMS="$HOST_ARCH" DIST="$WORK/dist" VERSION=e2e \
  "$ROOT/scripts/build-release.sh" >"$WORK/release.log" 2>&1 ||
  { echo "  构建 release 失败，见 $WORK/release.log"; exit 1; }
echo "  构建出 $(find "$WORK/dist" -name '*.tar.gz' | wc -l) 个包"

INST_PORT=$((PORT + 1))
INST="$WORK/inst"
"$ROOT/scripts/install.sh" server --node inst \
  --listen "127.0.0.1:$INST_PORT" --db "$INST/probe.db" \
  --prefix "$INST/probe" --conf-dir "$INST/etc" \
  --version e2e --from-dir "$WORK/dist" --no-systemd >"$WORK/install-server.log" 2>&1 ||
  { echo "  安装 server 失败："; tail -20 "$WORK/install-server.log"; exit 1; }
echo "  server 装好"

INST_TOKEN="$(awk '/token:/{gsub(/[" ]/,"",$2);print $2;exit}' "$INST/etc/nodes.yaml")"
"$ROOT/scripts/install.sh" agent \
  --server "http://127.0.0.1:$INST_PORT" --node inst --token "$INST_TOKEN" --interval 5s \
  --prefix "$INST/probe-a" --conf-dir "$INST/etc-a" \
  --version e2e --from-dir "$WORK/dist" --no-systemd >"$WORK/install-agent.log" 2>&1 ||
  { echo "  安装 agent 失败："; tail -20 "$WORK/install-agent.log"; exit 1; }
echo "  agent 装好"

"$INST/probe/bin/server" -config "$INST/etc/nodes.yaml" -web "$INST/probe/web" \
  >"$WORK/inst-server.log" 2>&1 &
INST_SRV=$!
sleep 2
"$INST/probe-a/bin/agent" -config "$INST/etc-a/agent.yaml" >"$WORK/inst-agent.log" 2>&1 &
INST_AG=$!
sleep 12

curl -fsS "http://127.0.0.1:$INST_PORT/api/v1/nodes" -o "$WORK/inst-nodes.json"
python3 - "$WORK/inst-nodes.json" "$INST_PORT" <<'PY'
import json, sys, urllib.request

d = json.load(open(sys.argv[1]))
n = [x for x in d["nodes"] if x["id"] == "inst"][0]
print("  装出来的 agent online:", n["online"], " metrics:", "有" if n["metrics"] else "无")
assert n["online"] is True, "用 release 包装出来的 agent 应该能上报"
assert n["metrics"] is not None

# server 包里的前端也一并验一下（-web 指向的是安装目录里的副本）。
code = urllib.request.urlopen(f"http://127.0.0.1:{sys.argv[2]}/").status
print("  装出来的面板 GET / =>", code)
assert code == 200
PY

kill "$INST_AG" "$INST_SRV" 2>/dev/null || true
wait "$INST_AG" "$INST_SRV" 2>/dev/null || true
INST_AG=""
INST_SRV=""

echo
echo "全部通过 ✅"
