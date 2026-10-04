# SimpleProbe

> 简单的不能再简单的探针系统。

只看**负载 / CPU / 内存 / 磁盘 / 在线率**。完全自研，前后端分离，零前端依赖。

设计规格见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 它不做什么

这是这套东西存在的理由——它**结构性地**没有这些能力，不是靠配置关掉的：

- ❌ 没有 SSH / Web 终端
- ❌ 没有文件管理
- ❌ 没有远程任务执行
- ❌ 没有任意 URL 探测（SSRF 的源头）
- ❌ 没有 WebSocket、没有登录、没有多用户
- ❌ 没有节点自注册

server 里**不存在任何向 agent 下发指令的代码路径**。面板即使被打穿，攻击者最多
污染监控数据。agent 非 root 即可运行、只读 `/proc` 与 `statvfs`、纯出站、
不开任何入站端口。

## 架构

```
[agent × N]  ──HTTPS POST /api/v1/report──►  [server 单二进制 + SQLite]
 非root/只读/proc                               ▲            │
 纯出站，不开入站端口                            │            │ GET /api/v1/*（公开）
                                                │            ▼
                                    反向代理（可选，任意实现）  [web 静态产物]
                                                              ▲
                                              nodes.yaml ─────┘ 节点身份唯一真源
```

## 目录

| 路径 | 作用 |
|---|---|
| `cmd/agent` | 被监控机上的采集端 |
| `cmd/server` | 服务端（收上报 + 只读查询） |
| `internal/protocol` | **契约唯一真源**：载荷 struct、`Validate()`、`DecodeStrict()`，agent 与 server 共享 |
| `internal/collect` | `/proc` 与 `statvfs` 解析（纯函数，可测） |
| `internal/push` | 上报客户端：4xx 不重试，5xx/网络错误退避重试 |
| `internal/config` | `nodes.yaml` / `agent.yaml` 解析与严格校验 |
| `internal/store` | SQLite：明细、计数桶、当前快照、曲线降采样 |
| `internal/uptime` | 在线率公式（计数法，非分钟占比） |
| `internal/api` | 四条路由 |
| `web/app.css` | **全部视觉参数集中在 `:root`**，换皮只改这一个文件 |
| `web/app.js` | 两页共用的格式化与请求工具 |
| `web/index.html` | 列表页（概览条 + 每节点一张卡片） |
| `web/detail.html` | 详情页（自写 SVG 折线，零依赖） |
| `deploy/` | systemd 单元、两份配置模板、两份**可选**的反代示例 |
| `scripts/install.sh` | **一键安装 / 升级**：下载 release 包 → 校验 SHA256 → 装二进制与单元 → 启动 |
| `scripts/add-node.sh` | 加节点：生成 token → 改配置 → `-check` 校验（失败自动回滚）→ 产出 agent 配置 |
| `scripts/build-release.sh` | 交叉编译 + 打包 + 校验和（本地与 release 工作流共用） |
| `scripts/e2e.sh` | 端到端验证（含"用 release 包装一遍再跑起来"） |
| `scripts/check-web.sh` | 把 HTML 里的内联脚本抽出来交给 `node --check` |
| `docs/API.md` | **接口契约**：字段语义、边界情况、前端要点 |
| `docs/DESIGN.md` | 设计规格（含每个决策的理由与踩过的坑） |

## 接口

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/v1/report` | 每节点 Bearer token | agent 上报，成功 204 |
| GET | `/api/v1/nodes` | 公开 | 当前快照 + 在线率三窗口 |
| GET | `/api/v1/series` | 公开 | 曲线，参数 `node` / `metric` / `from` / `to` / `step` |
| GET | `/api/v1/health` | 公开 | 自检 |

可用指标：`cpu_pct`、`mem_pct`、`load1`、`load5`、`load15`（`mem_pct` 由查询时算出）。

字段语义、`null` 与 `[]` 的区别、折线为什么要断开、错误码表——都在
**[docs/API.md](docs/API.md)**。

## 快速开始

```bash
make test          # 全部单测（含覆盖率）
make build         # 产出 bin/agent 与 bin/server
make e2e           # 端到端：起 server + agent，跑完整套验收
make ci            # 本地一条命令复现 CI（格式/静态检查/单测/前端语法/构建/端到端）
```

手动跑一遍：

```bash
# 1) server 端配置（权限 0600，不要进 Git——里面是明文 token）
cat > nodes.yaml <<'EOF'
listen: "127.0.0.1:8080"
db: "./data/probe.db"
nodes:
  - id: web01
    display_name: "Web 01"
    token: "用 openssl rand -hex 32 生成"
EOF

# 2) 每台被监控机
cat > agent.yaml <<'EOF'
server: "https://probe.example.com"
node: "web01"
token: "同上"
interval: 30s
mounts: ["/"]
EOF

./bin/server -config nodes.yaml -web ./web    # -web 让它顺便托管页面（开发时方便）
./bin/agent  -config agent.yaml
```

加一台机器（推荐用脚本，它会校验配置并生成 agent 侧的命令）：

```bash
./scripts/add-node.sh --id nas --name "NAS"
systemctl reload simple-probe-server        # 等价于 kill -HUP <pid>，不用重启
```

脚本会打印 agent 侧要执行的一键安装命令。手改也行：编辑 `nodes.yaml` 加一个
节点块，然后 `kill -HUP`。

## 部署

发布物是静态二进制（agent ≈ 3 MB，server ≈ 5 MB，压缩后），
支持 `linux/amd64`、`linux/arm64`、`linux/armv7`

### 文件都放在哪

**除了 systemd 单元，所有东西都在同一个目录里**——二进制、配置、数据、前端。
安装目录由 `--dir` 指定，不指定就是**当前目录**：

```
/opt/simpleprobe/
├── bin/
│   ├── server 或 agent
│   └── add-node.sh          仅 server
├── etc/
│   └── nodes.yaml 或 agent.yaml
├── data/
│   └── probe.db             仅 server
└── web/                     仅 server

/etc/systemd/system/simple-probe-server.service
```

### 一键安装

```bash
# 1) server。先 cd 到你想装的地方，或者用 --dir 明说。
curl -fsSL https://github.com/TGBUG/SimpleProbe/releases/latest/download/install.sh \
  | sudo bash -s -- server --dir /opt/simpleprobe

# 2) 加第一台机器。add-node.sh 随 server 包一起装到了 bin/ 下，
#    默认路径都从它自身位置推出，不需要额外参数。
sudo /opt/simpleprobe/bin/add-node.sh --id web01 --name "Web 01"
sudo systemctl reload simple-probe-server

# 3) agent（上一步会把这条命令连同 token 一起打印出来）
curl -fsSL https://github.com/TGBUG/SimpleProbe/releases/latest/download/install.sh \
  | sudo bash -s -- agent --server https://probe.example.com --node web01 --token <token>
```

其他参数：`--dry-run` 只打印计划；`--no-systemd` 只装文件（容器里用）；
`--from-dir` 从本地目录取包（离线安装）；`--version <tag>` 装指定版本。

### 卸载

```bash
curl -fsSL .../install.sh | sudo bash -s -- uninstall server    # 或 uninstall agent
```

**只清 systemd 配置**（停服务、删单元、`daemon-reload`），
**不删任何数据或配置文件**。它会先从单元里读出安装目录，跑完告诉你文件还在哪，
想删自己 `rm -rf`。这样卸载不会误伤你辛苦攒的历史数据。

### 从源码装

```bash
make build                      # 产出 bin/server 与 bin/agent
sudo install -d /opt/simpleprobe/{bin,etc,data,web}
sudo install -m 0755 bin/server /opt/simpleprobe/bin/server
sudo install -m 0755 scripts/add-node.sh /opt/simpleprobe/bin/add-node.sh
sudo cp web/* /opt/simpleprobe/web/
sudo install -m 0600 nodes.yaml /opt/simpleprobe/etc/nodes.yaml
# 单元模板里的 /opt/simpleprobe 与 User=probe 按你的实际情况改掉
sudo install -m 0644 deploy/simple-probe-server.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now simple-probe-server
```

## 几个刻意的取舍

- **节点身份写在 server 配置里，不落库**。所以面板能显示“配置里声明了、
  但从未上报”的节点——部署期这一条能立刻告诉你是 agent 没装好还是网络不通。
- **token 明文存**。能读到这个文件的人，能力已经远超一个只用于写数据的 token。
- **不做降采样表**。30 秒 × 30 天 = 86,400 行/节点，SQLite 毫无压力，
  查询时按 step 分组即可。
- **在线率按“实际条数 / 期望条数”算**，不是“有数据的分钟数 / 总分钟数”。
  后者在 90 秒间隔下会把 100% 在线的节点算成 67%。
- **上报严格校验**：未知字段、尾随内容、越界取值一律拒绝，不补默认值。
- **前端零依赖零构建**：没有 npm、没有 node_modules，图表是自写的 SVG。
- **限流按节点，不按 IP**（agent 很可能都在同一个出口或反代后面），
  且**认证在限流之前**——token 不对的请求拿 401，不消耗该节点的额度。

## 限流

`nodes.yaml` 里的 `report_per_minute`（默认 **60**）是**单个节点**每分钟的上报次数
上限，固定一分钟窗口。

**它必须容得下协议允许的最快间隔**：`interval_s` 最小 5s，也就是 12 次/分钟，乘上
agent 单周期最多 3 次尝试是 36 次/分钟。阈值一旦低于这个数，配成 5s 的节点就会每
分钟必然吃 429——server 接受一份它自己拒绝服务的配置。它拦的是失控的死循环，
不是访问控制。

**429 不重试**：窗口要整整一分钟才滚动，而 agent 的重试在几秒内打完，不可能成功，
只会继续消耗同一个计数器。

## 状态

- ✅ v0.0 协议与校验层
- ✅ v0.1 agent 采集 → 上报 → 落库 → `/api/v1/nodes` → 列表页
- ✅ v0.2 历史曲线 `/api/v1/series` + 详情页
- ✅ v0.3 部署件 + 前端收口（概览条、区间极值、主题契约）
- ✅ v0.4 Release 工作流（三平台静态二进制 + SHA256SUMS）+ 一键安装脚本 + 去掉建账户步骤
- ✅ v0.5 单目录布局（`--dir`，默认当前目录）+ 运行身份改为目录属主（配置回到 0600）+ `uninstall`
- ⏳ 候选：离线 webhook 告警

`make e2e` 覆盖的验收：真实 `/proc` 数据落库、停掉 agent 后在线率确实下降、
非法上报被拒、曲线分桶正确、改配置 + SIGHUP 后新节点上线，
以及**用 release 包装进临时前缀再跑起来**（安装脚本坏了就什么都装不上）。

## License

MIT，见 [LICENSE](LICENSE)。
