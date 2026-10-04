# API

全部接口在 `/api/v1/` 下。**读接口一律公开、不需要认证**——面板本来就是公开状态页；
唯一需要认证的是 agent 用的 `POST /api/v1/report`。

服务端只是个 HTTP 端点：不读 `X-Forwarded-For`、不基于客户端 IP 做任何判断，
前面有没有反向代理都一样。所有时间都是 **unix 秒（整数）**，时区由前端自己决定。

| 接口 | 认证 | 说明 |
|---|---|---|
| `GET /api/v1/nodes` | 否 | 所有节点的当前状态，**面板列表页的全部输入** |
| `GET /api/v1/series` | 否 | 某个节点的单条历史曲线 |
| `GET /api/v1/health` | 否 | 存活与汇总，给探活/监控用 |
| `POST /api/v1/report` | Bearer token | agent 上报，前端用不到 |

---

## GET /api/v1/nodes

```bash
curl http://127.0.0.1:8080/api/v1/nodes
```

```json
{
  "generated_at": 1791100106,
  "nodes": [
    {
      "id": "local",
      "name": "本机",
      "online": true,
      "last_seen": 1791100104,
      "first_seen": 1791100094,
      "interval_s": 5,
      "uptime": { "24h": 1, "7d": 1, "30d": 1 },
      "metrics": {
        "load": [0.43, 0.3, 0.26],
        "cpu_pct": 12.940584088620344,
        "mem": { "used": 1758633984, "total": 3997663232 },
        "disk": [{ "mount": "/", "used": 16084709376, "total": 54694076416 }],
        "uptime_s": 1195170,
        "agent_version": "0.1.0-dev"
      }
    },
    {
      "id": "ghost",
      "name": "从未上报的机器",
      "online": false,
      "last_seen": null,
      "first_seen": null,
      "interval_s": 0,
      "uptime": { "24h": 0, "7d": 0, "30d": 0 },
      "metrics": null
    }
  ]
}
```

`nodes` 的顺序 = `nodes.yaml` 里的书写顺序，**服务端不排序**——运维可以用书写顺序
控制面板顺序，前端不要重排。

| 字段 | 类型 | 说明 |
|---|---|---|
| `generated_at` | int | 服务端生成这份快照的时刻 |
| `id` | string | 节点 id，配置里的唯一标识 |
| `name` | string | 显示名，配置没写就回落到 `id` |
| `online` | bool | 当前是否在线，见下 |
| `last_seen` | int \| null | 最近一次收到上报的时刻；**`null` 表示从未上报** |
| `first_seen` | int \| null | 首次收到上报的时刻；同上 |
| `interval_s` | int | 节点最近一次**自报**的上报间隔（秒）；**从未上报时为 0** |
| `uptime` | object | 三个窗口的在线率，`0.0 ~ 1.0`，键固定为 `24h` / `7d` / `30d` |
| `metrics` | object \| null | 最近一次上报的当前值；**`null` 表示从未上报** |

### `online` 怎么算

只看“最后上报时间距今是否超过阈值”，**不看在线率**：

```
online = (now - last_seen) < 2.5 × interval_s
```

容差取 2.5 倍是为了容忍一次丢包或抖动，又不会把真离线的机器判成在线
（30 秒间隔 → 75 秒）。`last_seen` 为 `null` 或 `interval_s` 为 0 时恒为 `false`。

### `uptime` 怎么算

```
期望条数 = (窗口终点 - max(窗口起点, 该节点纳入监控的时刻)) / interval_s
实际条数 = 窗口内收到的上报条数之和
在线率   = min(1, 实际 / 期望)
```

用的是**计数法**而不是“有数据的分钟数 / 总分钟数”。后者在间隔不整除 60 时会算错：
90 秒间隔、全程在线的节点会被算成约 67%。

“纳入监控的时刻”取配置里的 `since`，没写就用数据库里的首次上报时间——这是为了
裁剪分母：新节点若直接按 30 天算，接入第一天会显示在线率 1%。

### `metrics` 字段

| 字段 | 类型 | 单位 |
|---|---|---|
| `load` | float[3] | 1 / 5 / 15 分钟负载，**恒为 3 个元素** |
| `cpu_pct` | float | `0 ~ 100`（已排除 guest 时间，含 iowait） |
| `mem.used` / `mem.total` | uint | **字节**；`used = MemTotal - MemAvailable`（刻意不用 MemFree——它不含 cache/buffer，几乎每台机都会显示内存快满） |
| `disk` | array | **至少 1 个**；每项 `mount` / `used` / `total`（字节） |
| `uptime_s` | int | 主机开机时长（秒） |
| `agent_version` | string | agent 版本 |

前端要算内存百分比请用 `100 * used / total`，不要假设 `total` 是常数或已扣缓存。

---

## GET /api/v1/series

```bash
curl "http://127.0.0.1:8080/api/v1/series?node=local&metric=cpu_pct&from=1791013707&to=1791100107"
```

```json
{
  "node": "local",
  "metric": "cpu_pct",
  "step": 300,
  "from": 1791013707,
  "to": 1791100107,
  "points": [[1791099900, 6.195156311020312]]
}
```

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `node` | ✅ | — | 节点 id，必须在配置里存在，否则 404 |
| `metric` | | `cpu_pct` | 见下方白名单，非法值 400 |
| `from` | | `to - 24h` | unix 秒 |
| `to` | | 当前时间 | unix 秒 |
| `step` | | 自动 | 降采样步长（秒），必须为正整数；比节点粒度更细会被抬到粒度 |

**可用指标（白名单，只有这五个）**：

| metric | 含义 |
|---|---|
| `cpu_pct` | CPU 使用率 |
| `mem_pct` | 内存使用率（查询时按 `100 × AVG(used) / AVG(total)` 算） |
| `load1` / `load5` / `load15` | 1 / 5 / 15 分钟负载 |

`load` 在 `/nodes` 里是一个三元数组，在 `/series` 里拆成三个独立指标。

### `points` 的语义

`[[时间戳, 值], ...]`，二元数组而不是对象——省一半字节。
时间戳是桶的**起点**（`ts` 向下取整到 `step` 的整数倍）。

**空桶会被直接跳过，而不是补 0 或补 null。** 所以：

- 点与点之间的时间差可能大于 `step` —— **前端要把折线断开，不要连直线**
  （直线会凭空造出一段并不存在的趋势）。
- 完全没数据的区间返回空数组 `[]`（不是 `null`），前端少一个分支。

`step` 由服务端挑，响应里回显的是**实际生效值**：从固定阶梯里选
`30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400`，
保证点数 ≤ 500，且不细于该节点最近申报的上报间隔。

`from` / `to` 回显的也是**实际生效值**：跨度超过 30 天会被夹紧到 30 天
（明细则只留 30 天，再往前没有数据）。夹紧而不报错，是为了让前端的“30 天”
按钮在保留期被调短时仍然能用——**请以响应里的 `from`/`to` 画坐标轴**，
不要用你请求时传的值。

---

## GET /api/v1/health

```json
{ "status": "ok", "nodes": 2, "online": 1, "time": 1791100107 }
```

`status` 为 `ok` 或 `degraded`；读数据库失败时返回 `degraded` 且 HTTP 状态码为 **503**，
正常时 200。适合做探活。

---

## POST /api/v1/report

agent 专用，前端用不到。列在这里是因为它定义了数据模型，也方便自己写 agent。

```bash
curl -X POST http://127.0.0.1:8080/api/v1/report \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <token>' \
  -d '{"v":1,"node":"local","interval_s":30,"load":[0.4,0.3,0.2],"cpu_pct":12.9,
       "mem":{"used":1758633984,"total":3997663232},
       "disk":[{"mount":"/","used":16084709376,"total":54694076416}],
       "uptime_s":1195170,"agent_version":"0.1.0"}'
```

成功返回 **204 No Content**（无响应体）。

`interval_s` 是 agent **自报**的上报间隔、`v` 是协议版本（当前 `1`）。
请求体上限 **8 KiB**。

校验是**严格 fail-closed** 的：未知字段、尾随内容、越界取值一律拒绝，不补默认值。
自写 agent 时注意 `load` 必须恰好 3 个元素、`mem.total` 与 `disk[].total`
不能为 0、`disk[].used` 不能大于 `total`、挂载点不能重复。

---

## 错误

所有错误是同一个信封：

```json
{ "code": "invalid_field", "field": "metric", "message": "未知指标；可选：cpu_pct, mem_pct, load1, load5, load15" }
```

| HTTP | code | 出现场景 |
|---|---|---|
| 400 | `invalid_field` | 取值越界；或查询参数非法（`metric` / `step` / `from` / `to`） |
| 400 | `unknown_field` | 上报体里有未声明的字段（`field` 给出字段名，多半是拼错了） |
| 400 | `invalid_type` | 上报体里某个字段类型不对 |
| 400 | `malformed_json` | 上报体不是合法 JSON / 读不出来 |
| 400 | `trailing_data` | JSON 结束后还有尾随内容 |
| 400 | `unsupported_version` | 上报的 `v` 不是服务端支持的协议版本 |
| 401 | `unauthorized` | `/report` 的节点不存在或 token 不匹配（**两者返回同一个响应**，不泄露 id 是否存在） |
| 404 | `unknown_node` | `/series` 的 `node` 不在配置里 |
| 413 | `body_too_large` | 上报请求体超过 8 KiB |
| 415 | `unsupported_media_type` | 上报的 `Content-Type` 不是 `application/json` |
| 429 | `rate_limited` | 该节点超过 `report_per_minute`（默认 60 次/分钟） |
| 500 | `internal_error` | 服务端内部错误，细节只进日志 |

`400` 那一组只可能出现在 `/report`（前端读接口不产生它们）。`message` 可以直接
展示给用户；`field` 只在 `invalid_field` 与 `unknown_field` 时出现。

---

## 前端要点

- **轮询节奏**：两个读接口都带 `Cache-Control: public, max-age=15`，数据本身就是
  30 秒粒度的。列表页 15~30 秒轮询一次足够，别更快。
- **区分“从未上报”与“当前离线”**：`metrics === null` 是前者（配置里有、机器没接上），
  `metrics` 有值而 `online === false` 是后者。这两种状态在部署期含义完全不同，
  建议用不同的视觉表达。
- **在线率是 0~1 的小数**，展示时自己乘 100。
- **`interval_s` 可能为 0**（从未上报），不要用它做除法。
- **折线要断**：`points` 里相邻点的时间差大于 `step` 就说明中间没有数据。
- **坐标轴用响应里的 `from`/`to`/`step`**，不要用请求参数。
- **`disk` 是数组**，可能不止一个挂载点，`mount` 直接显示即可。
- 主题换肤的约定见 `web/app.css` 顶部的 `:root` 注释。
