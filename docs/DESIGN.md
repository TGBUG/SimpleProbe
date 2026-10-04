# 自研极简探针 · 实现规格草案 v0（讨论稿）

> 锁定决策：
> **① 主机在线率**（只统计上报成功率，不做主动探活）
> **② Go(agent+server) + Vue3 + ECharts**
> **③ 30s 默认上报，明细留 30 天，在线率桶留 1 年**
> **④ 公开状态页**（读接口无需登录，字段白名单）
> **⑤ 磁盘只显示当前值，不做曲线**
> **⑥ 节点身份写在 server 配置文件中**（token 明文 / 名称等），不落库、无自注册
> **⑦ 告警 v0 不做**
> **⑧ 上报数据严格校验，任何一处不合规即拒绝**（fail closed）
> **⑨ 上报频率由 agent 自行配置**（频率随载荷上报，server 不重复配置）
>
> 状态：v0.0（协议与校验层）实现中。日期 2026-10-03

---

## 1. 整体拓扑与安全模型

```
[agent × N]  ──HTTPS POST /api/v1/report──►  [server 单二进制 + SQLite]
 非root/只读/proc                               ▲            │
 纯出站，不开任何入站端口                        │            │ GET /api/v1/*（公开）
 频率可自行配置                                  │            ▼
                                    反向代理（可选，任意实现）  [web 静态产物]
                                                              ▲
                                              nodes.yaml ─────┘ 节点身份唯一真源
```

**服务端只是个 HTTP 端点**：它监听配置里的地址，不假设前面有没有代理，也不读
`X-Forwarded-For`（没有任何地方读客户端 IP）。所以 nginx / Caddy / Traefik /
云厂商 LB 都能用，**也可以完全不用**——直接暴露端口、或者只在内网跑都行。
TLS 由谁终止是部署方的事，服务端代码里没有证书相关逻辑。

**安全模型是结构性的，不是配置性的**——这是这套东西和你离开哪吒的根本区别：

| 层 | 能力边界 |
|---|---|
| agent | 只读 `/proc/loadavg`、`/proc/stat`、`/proc/meminfo`、`/proc/uptime` + `statvfs`；非 root 即可；**代码里没有 exec/shell/文件写** |
| server | 只有「收上报」和「发查询」两条路径；**不存在任何向 agent 下发指令的代码**。面板被打穿，攻击者最多污染监控数据 |
| 公开读接口 | 字段白名单（不含内网 IP / token / 主机名，只暴露配置里给的 `display_name`） |
| 上报接口 | 每节点独立高熵 token（明文比对配置文件里的值）、常量时间比较、body 限长、**严格校验**、per-node 限流 |

**关于 token 明文存放**（已定）：`nodes.yaml` 直接写明文 token，不做哈希、不做环境变量间接。判断依据是——**能读到这个文件的人，已经拿到了远超这个 token 的能力**（同机的 shell、DB、进程内存），为它再套一层哈希属于自我安慰。缓解：文件权限 `0600`、不进 Git。token 泄露的后果被架构限制在"可以伪造上报数据"，**拿不到 shell**。

---

## 2. 数据模型（SQLite DDL 草案）

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA auto_vacuum  = INCREMENTAL;   -- 必须在建库时设置，建完再改无效

-- 节点状态：身份来自配置文件，不落库。这里只存"跑起来才知道"的东西。
-- 首屏只读这张表，不需要 join，也不需要扫 sample
CREATE TABLE node_state (
  node_id       TEXT PRIMARY KEY,          -- slug：web01 / nas / hk-1，与配置里的 id 一致
  first_seen    INTEGER NOT NULL,          -- 首次成功上报；配置里声明了 since 就用配置值
  last_seen     INTEGER NOT NULL,          -- server 接收时间
  last_interval INTEGER NOT NULL,          -- 最近一次上报自带的 interval_s，用于在线率与在线阈值
  last_state    TEXT    NOT NULL,          -- 最近一次上报的 JSON 原文
  agent_version TEXT
) WITHOUT ROWID;

-- 时序明细：30 天。聚簇索引让"取某节点最新一条"= 一次索引 seek
CREATE TABLE sample (
  node_id   TEXT    NOT NULL,
  ts        INTEGER NOT NULL,              -- server 接收时间(unix秒)，非 agent 时间
  load1 REAL, load5 REAL, load15 REAL,
  cpu_pct   REAL,
  mem_used  INTEGER, mem_total INTEGER,
  uptime_s  INTEGER,
  PRIMARY KEY (node_id, ts)
) WITHOUT ROWID;

-- 上报计数桶：1 年。一天 1440 行/节点 → 一年 52.6 万行/节点，约 5MB
CREATE TABLE uptime_bucket (
  node_id TEXT    NOT NULL,
  minute  INTEGER NOT NULL,                -- ts / 60
  reports INTEGER NOT NULL,
  PRIMARY KEY (node_id, minute)
) WITHOUT ROWID;
```

> `uptime_bucket` 现在只作**计数**用途，不再有"这个分钟有没有数据 = 在不在线"的语义——原因见 §4.2。

**四个刻意的取舍**

1. **节点身份放配置文件，不落库**。带来的几个好处：
   - 没有"注册"这个动作，也就没有可被滥用的注册入口；token 直接明文比对配置值
   - **面板能显示"配置里声明了、但从未上报"的节点**（`node_state` 无行 + 配置有此 id → 显示"从未上线"）。部署期加机器时，这条能立刻告诉你"是 agent 没装好还是网络不通"，比"节点根本不出现"友好得多
   - 节点列表 = 配置 ∪ 数据库中已有状态行；配置里删掉一个节点，历史数据仍留着（默认不删），只是不再出现在面板上
2. **磁盘不进 sample 表**。"存储占用"是当前值不是曲线，放在 `node_state.last_state` 的 JSON 里就够——省掉一张按挂载点膨胀的表，让 DDL 简单一半。
3. **不存降采样表**。30s × 30 天 = 86,400 行/节点（≈8MB/节点）。10 台机不到 100MB，SQLite 毫无压力。既然选了"明细留 30 天"，**就别再做聚合层**，查询时按 step 用 SQL 分组即得。这是"明细留 30 天"换来的最大简化。
4. **在线率独立成表**，因为它和明细的保留期不同（1 年 vs 30 天）。同表会导致要么删了明细丢掉在线率，要么为了在线率留一堆没用的明细。

**容量核对**：10 节点 × (86.4k 明细 + 525k 桶) ≈ 180MB 稳态；节点数翻倍只是线性增长。若把某台 agent 的间隔调小，只影响那台的数据量——`interval_s` 的合法下限（5s）就是为防这一点设的。

---

## 3. 采集端（agent）设计 + 5 个必踩的坑

常驻 daemon（systemd），内部 ticker，**默认 30s，可由 agent 自己配置**。**不做落盘队列**：离线期间数据直接丢弃（在线率由 server 侧判定，不需要补历史，这是把复杂度砍掉的关键决定）。

### 3.1 agent 配置
```yaml
server:  "https://probe.example.com"
node:    "web01"
token:   "3f9a...c1"
interval: 30s            # 上报频率，agent 自己说了算；会随载荷上报给 server
mounts:  ["/"]           # 要监控的挂载点，默认 /；可加 /data
```
`interval` 只写在 agent 一处，server 端不重复配置——**两处配置同一个值必然漂移**，所以频率随载荷自描述（`interval_s`）。

### 3.2 CPU 差分——guest 时间会被重复计算
`/proc/stat` 的 `guest` / `guest_nice` **已经包含在 `user` / `nice` 里**，若把它们再加进 total 就会重复计入，CPU 显示偏低：
```
total    = user + nice + system + idle + iowait + irq + softirq + steal   // 不含 guest*
idle_all = idle + iowait                                                   // iowait 要算进空闲
cpu_pct  = (1 - Δidle_all / Δtotal) * 100
```
`iowait` 不计入 idle 是另一个常见错误——IO 密集的机器 CPU 会显示虚高。

### 3.3 首次与异常采样必须丢弃
第一次没有 prev，不能上报 0 或 100。做法：启动先采一次**基线**，一个 interval 后才开始上报；若 `Δtotal` 小于 ~0.1s（时钟跳变）或大于 3×interval（挂起/休眠唤醒），**丢弃本次**而不是硬算。

### 3.4 内存要用 MemAvailable，不是 MemFree
`MemFree` 不含 cache/buffer，几乎每台机都会显示"内存快满了"。用 `MemAvailable`（内核估算的可回收量）：
```
mem_used = MemTotal - MemAvailable
```

### 3.5 statvfs 要用 f_frsize，且用 bavail
```
total = f_blocks * f_frsize
free  = f_bfree  * f_frsize
avail = f_bavail * f_frsize        // 非特权用户可用，更贴近"还能写多少"
used  = total - free
```
用 `f_bsize` 在部分文件系统上会算错（`f_bsize` 是"最优 IO 块"，`f_frsize` 才是"片段大小"）。

### 3.6 挂载点会爆炸
从 `/proc/mounts` 全量读会把 `tmpfs/devtmpfs/overlay/squashfs/proc/sysfs/cgroup*` 全报上来。**默认只报 `/`，配置里显式追加**。顺带注意：容器里 `/proc/meminfo` 与负载是**宿主机的**（除非 cgroup/lxcfs 介入），如果 agent 打算跑在容器里要提前想清楚。

### 3.7 载荷
```json
{ "v": 1, "node": "web01", "interval_s": 30,
  "load": [0.42, 0.55, 0.61],
  "cpu_pct": 12.3,
  "mem": { "used": 3221225472, "total": 8589934592 },
  "disk": [ { "mount": "/", "used": 21474836480, "total": 53687091200 } ],
  "uptime_s": 1234567,
  "agent_version": "0.1.0" }
```
**注意载荷里没有时间戳**——时间由 server 收到的那一刻决定，见 §4.1。

---

## 4. 服务端设计

### 4.0 节点配置（`nodes.yaml`，权限 0600，不进 Git）
```yaml
listen: "127.0.0.1:8080"
db: "./data/probe.db"
report_per_minute: 60            # 可选：单节点每分钟上报上限，默认 60（见 §4.5）

nodes:
  - id: web01
    display_name: "Web 01"
    token: "3f9a...c1"             # 明文；建议 openssl rand -base64 32 生成
    since: 2026-10-01              # 可选：纳入监控的起始日，用于裁剪在线率分母
  - id: nas
    display_name: "NAS"
    token: "8b2e...7d"
```
节点配置里**没有** `interval`——频率由 agent 自报。也**没有** `mounts`——挂载点是 agent 侧的事。

- **热加载**：`SIGHUP` 重读节点列表（不影响历史数据），避免为加一台机重启服务
- **token 比对**：`subtle.ConstantTimeCompare`，未知 `node_id` 直接 401

### 4.1 时间权威：一律用 server 接收时间
agent 的时钟可能不准，离线后补发还可能"伪造在线"。所以 `sample.ts` 与 `uptime_bucket.minute` **全部用 server 的 `time.Now()`**。agent 若上报了自带时间戳，只存进 `last_state` 供诊断，**不参与任何计算**。

### 4.2 在线率算法（最容易写错的一处）
**先记住一个反直觉的事实：在线率不能按"每个分钟桶里有没有数据"来算。** 一旦允许每个 agent 自己配上报间隔，这个算法立刻会坏：某台机配成 **90 秒**上报一次，它明明 100% 在线，但每个自然分钟里有 1/3 的分钟是空的，在线率会显示成约 67%。间隔越不整除 60，误差越大。

正确做法是**按"实际收到的上报数 / 期望收到的上报数"**：
```
窗口秒数 = to - max(from, since_or_first_seen)      // ← 分母裁剪，必须有
期望     = 窗口秒数 / interval_s                    // ← interval_s 来自最近一次上报
实际     = 窗口内 uptime_bucket.reports 之和
在线率   = min(1, 实际 / 期望)                       // ← min 是防御，不是装饰
```
- **`max(from, since_or_first_seen)` 的裁剪必须有**：新节点如果直接按 30 天算分母，接入第一天会显示在线率 1%。
- **`min(1, …)` 必须有**：如果有人把 agent 配得比它申报的还频繁（改配置忘了重启、或时钟跳变导致密发），实际会超过期望，不夹紧就会出现 >100% 的在线率。
- **「当前是否在线」不看在线率**，看最后上报时间：
  ```
  在线 ⟺ now - last_seen < 2.5 × interval_s      // 30s 间隔 → 75s 容差
  ```
  容差取 2.5 倍：能容忍一次丢包或一次抖动，又不会把真正离线的机器误判成在线。

三个窗口：`24h / 7d / 30d`。

### 4.3 查询与"降采样"
不做预聚合，查询时按 step 分组：
```sql
SELECT ts/300*300 AS bucket, AVG(cpu_pct) AS v
FROM sample WHERE node_id=? AND ts BETWEEN ? AND ?
GROUP BY bucket ORDER BY bucket;
```
`step` 下限取该节点的 `interval_s`（不细于原始粒度）。**取"当前值"不要走 sample 表**，直接读 `node_state.last_state`；sample 表只服务曲线。

### 4.4 清理与磁盘卫生
每天一次：
```sql
DELETE FROM sample        WHERE ts     < now - 30d;
DELETE FROM uptime_bucket WHERE minute < (now - 365d)/60;
PRAGMA incremental_vacuum;
```
（`auto_vacuum=INCREMENTAL` 建库时就设，否则这条没用。）

### 4.5 公开接口的防护
- `GET /api/v1/nodes`、`/series` 加 `Cache-Control: public, max-age=15`
- per-IP 令牌桶限流（`golang.org/x/time/rate`）
- `POST /api/v1/report`：body ≤ 8KB、per-node 限流

#### 限流阈值（v0.5.1 修：这里曾经是实现漂移）

规格原本就写了"阈值随 `interval_s` 自适应，别把频率调快的节点误伤"，
**但实现写死成了 10 次/分钟，没有跟 `interval_s` 挂钩**，于是：

```
interval=  5s → 12.0 次/分钟   ← 协议允许的最快间隔，却每分钟必然吃 429
interval= 30s →  2.0 次/分钟   ok
```

这就是"server 接受一份它自己拒绝服务的配置"：`MinIntervalS = 5` 是合法的，
但配成 5s 的节点永远上不了线。修法是**把阈值做成配置项 `report_per_minute`**，
并把默认值定在能容下最坏合法情况：

```
最坏合法频率 = (60 / MinIntervalS) × push 单周期最大尝试次数
             = (60 / 5) × 3 = 36 次/分钟   →  默认取 60
```

没有走"按 `interval_s` 动态计算"那条路（那正是规格原来的措辞），因为
`interval_s` 是 agent **自报**的，用它算额度等于让请求方自己定额度；而且
可变阈值会让限流逻辑从十行变成需要解释的东西。一个开得够宽的固定值配上
配置项，既满足同一条不变量，又不用信任请求方。

**它的职责是拦住失控的死循环**（几千次/分钟），不是访问控制——能拿出 token
的人本来就有权上报这个节点的数据。

**配套修的一条：429 不重试。** 原先 429 被归成"临时错误"而重试 3 次，但限流
窗口是整整一分钟，而单周期的重试在几秒内打完——那些重试不可能成功，反而继续
消耗同一个计数器，**把节点往坑里推得更深**（自我维持的限流）。现在收到 429
当轮直接放弃，下个周期窗口已滚动，自然恢复。

### 4.6 不做 WebSocket
轮询 30s 完全够用。哪吒的 9.9 分漏洞之一就是终端 WebSocket 的 CSWSH，**只读面板没有任何理由引入长连接**。

### 4.7 严格校验：任何一处不合规就拒绝（fail closed，已定）
服务端对上报**不做任何宽容处理**——不补默认值、不忽略未知字段、不"尽力解析"。校验点：

| 层 | 规则 | 失败 |
|---|---|---|
| 传输 | `Content-Type: application/json`；body ≤ 8KB | 415 / 413 |
| 解析 | JSON 必须完整、**不允许尾随数据**；**未知字段即拒绝**（`DisallowUnknownFields`） | 400 |
| 结构 | 必需字段缺失、类型不符（如 `load` 不是数组） | 400 |
| 版本 | `v` 必须是 server 显式支持的版本 | 400 |
| 身份 | `node` 必须存在于配置；token 常量时间比对 | 401 |
| 取值 | `interval_s ∈ [5,3600]`；`load` **恰好 3 个元素**且均为有限非负数；`cpu_pct ∈ [0,100]` 且有限；`mem.total > 0` 且 `used ≤ total`；`disk` 至少 1 条、每条 `mount` 非空不重复且 `total > 0`、`used ≤ total`；`uptime_s ≥ 0`；`node` 与 `agent_version` 非空且长度受限 | 400 |
| 频率 | 同节点超出限流阈值 | 429 |

**必须显式校验 `load` 的长度**：Go 的 `encoding/json` 在解码到数组时会**静默丢弃多余元素**，`[1,2,3,4]` 会被当成合法载荷接受——这与"任何一处不合规都拒绝"直接冲突。所以协议里 `load` 用**切片**而不是 `[3]float64`，长度在 `Validate()` 里显式检查。

**NaN / Inf 检查不是多余的**：JSON 本身写不出 NaN，但 agent 是**先在内存里构造 struct 再校验**的，而 `0/0` 这类除零完全可能算出 NaN。共享同一份校验代码，agent 就能在本地挡住自己的 bug，不用等对端 400。

- **响应体不回显 payload**（避免日志注入与反射型问题），只给机器可读的 `code` 和一句短描述。
- **agent 侧必须区分错误类别**：
  - 网络错误 / 5xx → 指数退避重试（"对端暂时有问题"）
  - **429 限流 → 不重试**，当轮放弃（窗口没滚，重试不可能成功且会加深限流；见 §4.5）
  - **其余 4xx → 不重试**，本地高亮报错（"我发的东西不对"，重试一万次也一样）
- **已知代价（要接受）**：严格拒绝意味着**将来加字段必须成对升级**——老 server 会拒掉新 agent 的载荷。所以 `v` 字段就是为此准备的：改 schema 时递增 `v`，server 同时支持 v1/v2，**先升 server 再升 agent**。

---

## 5. API 契约

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/v1/report` | Bearer token（比对配置） | agent 上报，成功 204，校验规则见 §4.7 |
| GET | `/api/v1/nodes` | 公开 | 配置节点 ∪ 状态行；含 `online`、`online_rate{24h,7d,30d}`、最新负载/CPU/内存/磁盘/uptime |
| GET | `/api/v1/series?node=&metric=&from=&to=&step=` | 公开 | 曲线，返回 `points:[[ts,v],...]` |
| GET | `/api/v1/health` | 公开 | 自检（DB 可写、在线节点数） |

**契约一致性**：agent 与 server 放进**同一个 Go module**，共享 `internal/protocol` 里的 report struct 与 `Validate()` —— 契约由编译器保证，不可能漂移。前端用 `tygo` 从这几个 struct 生成 TS 类型，不手抄。

---

## 6. 前端

- Vue3 + Vite + TS（**v0.1~v0.3 实际用的是零依赖原生 JS，见 §9 的偏差说明**）；
  静态产物可由 server 的 `-web` 托管、交给任意反向代理、或单独部署
- **两个页面**：列表页（每节点一张卡片：名称 / 在线点 / 1·5·15 负载 / CPU·内存·磁盘 进度条 / uptime）＋ 详情页（曲线，范围切换 1h/24h/7d/30d）
- 未上报过的节点也要出卡片，状态显示"从未上线"（对应 §2 取舍 1）
- ECharts 按需引入；进度条用纯 CSS（不引 UI 框架，省体积也更好控外观）
- 数据获取：`setInterval` 轮询 `/nodes`（轮询间隔取各节点 `interval_s` 的众数），详情页按需拉 `/series`
- **为"换皮"留口子**：所有颜色/圆角/字体走 CSS 变量，主题 = 一个 CSS 文件覆盖变量。以后想做什么风格都不动组件

---

## 7. 项目结构

```
SimpleProbe/                  # module github.com/TGBUG/SimpleProbe
├── go.mod
├── cmd/agent/main.go
├── cmd/server/main.go
├── internal/
│   ├── protocol/             # ★ 上报载荷 struct + Validate() + DecodeStrict()（agent/server 共享）
│   ├── collect/              # /proc 解析，纯函数，便于单测
│   ├── push/                 # 上报客户端（退避重试 + 4xx 不重试）
│   ├── config/               # nodes.yaml / agent.yaml 解析 + SIGHUP 热加载
│   ├── api/                  # HTTP handler
│   ├── store/                # SQLite 读写 + 曲线降采样
│   └── uptime/               # 在线率计算（§4.2 公式）
├── web/                      # 零依赖前端（app.css / app.js / index.html / detail.html）
├── deploy/
│   ├── simple-probe-server.service
│   ├── simple-probe-agent.service
│   ├── nodes.example.yaml    # 节点身份模板（真文件 0600，不进 Git）
│   ├── agent.example.yaml
│   ├── nginx.conf.example    # 反代示例（可选，等价示例见 Caddyfile.example）
│   └── Caddyfile.example
├── scripts/
│   ├── e2e.sh                # 端到端验证
│   ├── add-node.sh           # 加节点：生成 token + 改配置 + 校验 + 产出 agent 配置
│   └── check-web.sh          # 内联脚本语法检查（前端零构建，CI 里必须挡）
├── .github/workflows/ci.yml
└── docs/DESIGN.md            # 本文档
```

---

## 8. 里程碑

| 版本 | 内容 | 验收 |
|---|---|---|
| **v0.0** ✅ | `internal/protocol`：载荷 struct + `Validate()` + `DecodeStrict()` + 表驱动单测 | ✅ **已完成**：72 个用例全过、语句覆盖率 100%、`go vet`/`gofmt` 干净；§3.7 那段示例 JSON 已作为一条测试锁住规格与实现不漂移 |
| **v0.1** ✅ | agent 采集 → 上报 → server 存库 → `/nodes` → 一个丑但能用的页面 | ✅ **已完成**：`make e2e` 端到端通过（真实 `/proc` 数据落库、反向验证在线率确实下降、非法上报被拒） |
| **v0.2** ✅ | 曲线 `/series` + 在线率三窗口（§4.2 计数法）+ 当前在线状态 | ✅ **已完成**：端到端反向验证通过（停 agent 后在线率 1.0→0.54）；新增 store 级验收测试 `TestOnlineRate_NinetySecondInterval` 证明 90 秒间隔、全程在线的节点在线率仍是 100%，同一测试里的反事实断言说明按分钟桶算会得到约 0.67 |
| **v0.3** ✅ | 前端美化 + 主题变量 + 部署脚本（systemd/nodes.yaml 模板/add-node.sh） | ✅ **已完成**：`make e2e` 里新增验收段——`add-node.sh` 改配置 → `SIGHUP` → 起第二个 agent，节点上线；systemd 单元经 `systemd-analyze verify` 结构校验 |
| **v0.4** ✅ | Release 工作流 + 一键安装脚本 + 免账户部署 | ✅ **已完成**：`make release` 交叉编译三平台静态二进制并产出 SHA256SUMS（本地与工作流共用同一份逻辑）；`install.sh` 下载→校验→安装→启动，且 `make e2e` 里新增「用 release 包装进临时前缀再跑起来」的验收段 |
| **v0.5** ✅ | 单目录布局 + `--dir` + 卸载 | ✅ **已完成**：除 systemd 单元外的文件全部收进一个目录（默认当前目录）；运行身份改为安装目录属主，配置权限随之回到 0600；`uninstall server\|agent` 只清 systemd，e2e 覆盖「装单元 → 卸载 → 文件原样保留」 |
| v0.6 | 可选：离线 webhook 告警 | 按需 |

**明确不做**：SSH/终端、文件管理、任务下发、DDNS、插件市场、任意 URL 探测（SSRF 源头）、WebSocket、多用户与登录、节点自注册。

---

## 9. 决策状态

**全部已定**：主机在线率｜Go + Vue3 + ECharts｜30s 默认 / 30 天明细 / 1 年计数桶｜公开状态页｜磁盘只当前值｜节点身份进配置（token 明文，0600，不进 Git）｜v0 不做告警｜上报严格校验 fail closed｜**上报频率由 agent 配置并随载荷自描述**。

**v0.0 已完成**（2026-10-03）：`internal/protocol`，72 个用例、语句覆盖率 100%。

**v0.1 已完成**（2026-10-03）：agent 采集 → 上报 → SQLite 落库 → `/api/v1/nodes` → 单文件页面。
```
make test    # 全部单测；protocol 99% / uptime 100% / collect 98% / push 98% / config 97% / api 89% / store 84%
make build   # bin/server 与 bin/agent
make e2e     # 端到端（约 35 秒）：真实 /proc 数据落库 + 停掉 agent 后在线率确实下降 + 非法上报被拒
```
端到端实测到的真实数据：负载 `0.96 / 1.28 / 1.04`、CPU `2.2%`、内存 `1.9GB / 3.7GB`、
磁盘 `14.4GB / 50.9GB`、uptime `13 天`。停掉 agent 16 秒后 `online` 变 `false`、
24h 在线率从 `1.0` 落到 `0.53`——分母裁剪与判定阈值都按预期工作。

**环境注意**：本机 Go 构建缓存默认落在 `/root/.cache/go-build` 或 `/go`，在本沙箱下不可写。
`Makefile` 已把 `GOCACHE/GOPATH/GOTMPDIR` 指向工作区，直接用 `make` 即可；手工跑 `go` 命令时
需要自己带上这三个变量。另外 `proxy.golang.org` 不通，`goproxy.cn` 可用。

**与原始规格的两处偏差**（都是实现时的决定，已生效）：
1. 前端没有用 Vue3 + ECharts。v0.1 的验收标准是“丑但能用”，所以先用零依赖的单文件
   HTML + 原生 JS 实现，好处是**没有构建步骤、没有 node_modules**。颜色/圆角/字号已全部
   走 CSS 变量，v0.3 换皮时覆盖 `:root` 即可，届时再决定要不要引入框架。
2. `nodes.yaml` 解析用 `KnownFields(true)`，与上报采用同一套“未知字段即拒绝”口径——
   把 `display_name` 写成 `displayname` 会在启动时报错，而不是等到面板上看不见名字才发现。

**v0.2 已完成**（2026-10-03）：新增 `GET /api/v1/series` 与节点详情页。
```
make test    # protocol 99% / uptime 100% / collect 98% / push 98% / config 97% / api 91% / store 86%
make e2e     # 端到端：曲线按 step 分桶、时间戳对齐、mem_pct 由查询算出、未知指标 400、未知节点 404
```
要点：
- **降采样不建表**。`(ts/step)*step` 分组 + `AVG()`，步长从固定阶梯
  （30s/1m/2m/5m/10m/30m/1h/2h/6h/12h/1d）里选，保证一条曲线不超过 500 个点；
  步长永远不会细于该节点申报的上报间隔。
- **指标名是 SQL 白名单**。请求里的 `metric` 只用于查表，绝不拼进 SQL；
  `mem_pct` 在查询时由 `100.0 * AVG(mem_used) / AVG(mem_total)` 算出，不多存一列。
- **跨度超 30 天夹紧而不报错**，响应回显实际生效的 `from/to`——前端的 30 天按钮
  在保留期被调短时仍然能用。
- **空结果返回 `[]` 而不是 `null`**，前端少一个分支。
- 前端拆成共享的 `app.css` + `app.js` 与两个页面（`index.html` 列表、`detail.html` 详情），
  图表是自写的 SVG 折线，**依然零依赖、零构建**；数据中断处会**断开**而不是连成直线。
  v0.3 换皮只需要覆盖 `app.css` 里的 `:root`。

**v0.3 已完成**（2026-10-03）：部署件 + 前端收口。

```
make test    # 同 v0.2；另加 bash -n / node --check 检查脚本与前端
make e2e     # 新增验收段：add-node.sh 改配置 → SIGHUP → 装 agent → 新节点上线
```
要点：
- **`scripts/add-node.sh`** 一条命令完成「生成 token → 追加节点 → 用 server 自己的
  解析器 `-check` 校验 → 校验失败自动回滚 → 产出可直接拷走的 agent 配置」。
  刻意不引入 YAML 库：追加在「nodes 是最后一个键」的约定下是对的，万一放错位置，
  `-check` 会失败并还原——**失败是安全的**。
- **`server -check`** 这个新开关是给部署脚本用的：让脚本复用 server 的解析规则，
  而不是自己再实现一遍（两份规则必然漂移）。
- **systemd 单元**两个，都带加固：非 root、`ProtectSystem=strict`、`NoNewPrivileges`、
  地址族限制。最容易出问题的两项（`MemoryDenyWriteExecute`、`SystemCallFilter`）
  单独成段并注明「启动失败先注释掉这两行」。`systemd-analyze verify` 结构校验通过
  （❓ 未在真实 systemd 上跑过）。
- **反代是可选的**。`deploy/nginx.conf.example` 与 `deploy/Caddyfile.example` 只是
  示例；服务端不读 `X-Forwarded-For`、不做基于 IP 的限流，因此换任何反代都不用改
  配置或代码，也可以完全不用（❓ 两个示例配置本身未做语法校验——容器里没有
  nginx/caddy）。
- 前端：概览条（在线/离线/从未上线/总数）、离线卡片压暗、详情页图题右侧显示
  区间的最小/平均/最大；`app.css` 顶部写明**主题契约**（换皮 = 覆盖 `:root` 的三组
  语义变量），并补了 `:focus-visible` 与 `prefers-reduced-motion`。

**与原始规格的全部偏差**（累计）：
1. 前端没有用 Vue3 + ECharts。v0.1~v0.3 用的是零依赖原生 JS + 自写 SVG 折线：
   没有构建步骤、没有 node_modules。换皮靠 CSS 变量而非组件主题。要不要引入框架，
   留到真的需要复杂交互时再定。
2. `nodes.yaml` 解析用 `KnownFields(true)`，与上报同一套「未知字段即拒绝」口径。

**v0.4 已完成**（2026-10-03）：发布链路与免账户部署。

```
make release VERSION=v0.4.0    # 交叉编译 amd64/arm64/armv7 + 打包 + SHA256SUMS
make e2e                       # 新增验收段：用 release 包装进临时前缀再跑起来
```

三个决定值得记下来：

1. **发布物是静态二进制**。`CGO_ENABLED=0` 能编出来，是因为 SQLite 用的是纯 Go
   驱动（modernc.org/sqlite）；`ldd` 确认无动态依赖。agent ≈ 3 MB、server ≈ 5 MB
   （压缩后），装到任何发行版上都不需要 glibc 或运行时。打包脚本里有一条
   **静态性断言**与一条**本机架构烟测**（跑 `-check` 解析临时配置），发布物不会
   悄悄退化成动态链接。
2. **构建逻辑只有一处**。`make release` 与 release 工作流共用
   `scripts/build-release.sh`；工作流只负责把 `dist/` 挂上去。这样"发布物怎么打
   出来"不会出现本地与 CI 两套。
3. **不创建账户，也不退回 root**。判断依据是 agent↔server 之间只有单向上报、
   server 侧不存在下发指令的代码路径，为只读进程单独建账户收益有限。
   运行身份最终定为**安装目录的属主**（`User=` 由安装脚本写入），理由见
   「v0.5：单目录布局」——它同时解决了 DynamicUser 时代被迫把配置放宽到 `0644`
   的问题。

### v0.4 之后的三处修正

装过一次之后才发现的问题，都记在这里，因为它们暴露的是**脚本之间的隐式耦合**：

1. **服务名加 `simple-` 前缀**。`probe-server` 太通用，`systemctl stop probe-server`
   容易误伤系统上别的同名服务。现在是 `simple-probe-server` / `simple-probe-agent`。
   install.sh 会在安装时顺手停掉并删除旧的 `probe-*` 单元——否则两个服务抢同一个
   端口，新的根本起不来。
2. **安装参数里不再有 `--node`**。它只能加一个节点，装第二台机器还得改配置重装；
   而 `add-node.sh` 可以反复用。相应地，`nodes` 为空成了**合法状态**（原先
   `config.Normalize()` 直接报错，导致不带 `--node` 就装不上——一个参数同时是
   "可选"和"必需"，本身就是设计味道）。现在空列表照常启动，只在日志里提醒一句。
   `add-node.sh` 也必须随 server 包一起发，否则一键装完的机器上根本没有加节点的
   工具——这是最初的遗漏。
3. **`nodes:` 不能写成 `nodes: []`**（安装模板与 add-node.sh 的隐式契约）。
   `add-node.sh` 的加节点方式是"往文件末尾追加列表项"，而 `nodes: []` 是流式序列，
   后面跟块序列是非法 YAML。安装模板因此写成裸的 `nodes:`；`add-node.sh` 也会把
   遇到的 `nodes: []` 自动改写成 `nodes:`，这样 v0.4.x 装出来的配置也能继续用。
   **这类耦合没有类型系统兜底**，只能靠 e2e 覆盖——所以 e2e 里现在会真的装一遍、
   加一个节点、再确认二进制能解析它。

顺带修掉的：`add-node.sh` 原先会在 `listen` 是回环地址时自作聪明地推出
`http://127.0.0.1:8080`，agent 在另一台机器上会往自己身上发数据——**静默失败**。
现在这种情况明确提示手填，并打印一个占位符。

### v0.5：单目录布局

原来的布局把文件摊在三个地方（`/opt/probe` 放二进制与前端、`/etc/probe` 放配置、
`/var/lib/probe` 放数据库），用户实测后提出这**不方便管理、太松散**。改成一个目录：

```
$DIR/                 默认 = 当前目录，可用 --dir 指定
├── bin/    server 或 agent，外加 add-node.sh
├── etc/    nodes.yaml / agent.yaml
├── data/   probe.db
└── web/    前端
```

只有 systemd 单元还在 `/etc/systemd/system`（挪不了）。于是备份 = `tar` 一个目录、
搬家 = `mv` 一个目录、删干净 = `rm -rf` 一个目录。

三件事值得记下来：

1. **运行身份改成"安装目录的属主"**。原先是 `DynamicUser=yes`，因为它是"不建账户
   又不当 root"的标准答案。但它的自动 chown 只认 `StateDirectory=` 那几个固定根
   （/var/lib、/var/cache…），**没法把属主给到一个任意目录**；数据一旦搬进 `$DIR`，
   临时 uid 就写不进去了。改成用目录属主之后：不建账户、不是 root（除非你以 root
   直接装，脚本会明确警告）、目录是服务自己的。
2. **配置权限回到 `0600`**。这是 (1) 的直接收益：DynamicUser 时代服务读不了 0600
   的 root 文件，只能放宽到 0644 让明文 token 同机可读；现在服务就是文件属主，
   不需要再妥协。（SIGHUP 热加载也一直要求服务能直接读配置路径——`LoadCredential`
   给的是启动快照，reload 会读到旧内容，所以那条路本来就走不通。）
3. **属主交接必须排在 `systemctl start` 之前**。以 sudo 安装时目录是 root 建的，
   如果先启动再 chown，服务第一次启动会因为读不到 root 属主的 0600 配置而失败。
   这个顺序错误在 `--no-systemd` 的路径上完全看不出来，是写这份文档时顺着
   "谁会读到这个文件"推出来的。

**卸载只清 systemd 配置**：停服务、删单元、`daemon-reload`，**不删任何文件**。
它会先从单元里读出 `WorkingDirectory`，跑完告诉运维文件在哪、想删自己删。
卸载是最容易误伤历史数据的地方，所以这里刻意不做"顺手清理"。

**与原始规格的全部偏差**（累计）：
1. 前端没有用 Vue3 + ECharts。v0.1~v0.4 用的是零依赖原生 JS + 自写 SVG 折线：
   没有构建步骤、没有 node_modules。换皮靠 CSS 变量而非组件主题。要不要引入框架，
   留到真的需要复杂交互时再定。
2. `nodes.yaml` 解析用 `KnownFields(true)`，与上报同一套「未知字段即拒绝」口径。
3. 配置权限最终回到 `0600`（见上）。中途一度放宽到 `0644`，那是 DynamicUser
   时代的产物，已随单目录布局撤销。

**下一步候选**（尚未排期）：v0.6 离线 webhook 告警；或把前端换成 Vue3 + ECharts
（只有当交互复杂度真的需要时才值得）。
