// Package protocol 定义 agent 与 server 之间唯一的数据契约。
//
// 这个包被两个二进制共享：agent 在发送前调用 Validate 自检，server 在接收后
// 用同一份代码再校验一遍。契约由编译器保证，不存在字段漂移的可能。
// 对应规格 docs/DESIGN.md 的 §3.7 与 §4.7。
package protocol

import "time"

// Version 是当前协议版本。
//
// 递增它的目的是让**不兼容的改动**被明确拒绝，而不是让 agent 收到一个看不懂的
// 错误。判断标准是针对"老 server + 新 agent"这一侧的：
//
//   - **不兼容改动**（新增必填字段、改字段语义、改字段类型）→ 必须递增。
//     老 server 拒掉是必然的，递增版本能让错误变成 unsupported_version。
//   - **新增可缺省字段** → 不递增。老 agent 不发它、老 server 也不受影响；
//     而新 agent 打老 server 会因为 DisallowUnknownFields 收到
//     unknown_field（还带着字段名），已经足够指向"先升 server"。
//
// 无论哪种，**升级顺序都是先 server 后 agent**：新 server 同时容忍新旧 agent，
// 而老 server 一定拒新 agent。这样整个集群不需要停机窗口。
const Version = 1

const (
	// MinIntervalS / MaxIntervalS 是上报间隔的合法区间（秒）。
	// 下限防止有人把间隔配成 1 秒把库写爆；上限是因为超过一小时一次的话，
	// “在线率”本身已经失去意义。
	MinIntervalS = 5
	MaxIntervalS = 3600

	MaxNodeIDLen       = 64
	MaxAgentVersionLen = 64
	MaxDiskEntries     = 64
	MaxMountLen        = 256

	// MaxBodyBytes 是单次上报请求体的硬上限。
	MaxBodyBytes = 8 << 10 // 8 KiB

	// OnlineTolerance 是“当前是否在线”判定的容差倍数，见 OnlineThreshold。
	OnlineTolerance = 2.5
)

// Report 是一次上报的完整载荷。
//
// 这里刻意没有时间戳字段：时间一律以 server 收到的那一刻为准。agent 的时钟
// 可能不准，离线后补发还可能伪造在线，因此 agent 侧的时间不参与任何计算。
type Report struct {
	V int `json:"v"`
	// Node 是节点标识，必须与 server 配置文件中的 id 一致。
	Node string `json:"node"`
	// IntervalS 是 agent 自己的上报间隔（秒），随载荷自描述。
	//
	// 不在 server 配置里重复配置这个值：两处配置同一个值必然漂移，
	// 而在线率的分母直接依赖它（见规格 §4.2）。
	IntervalS int `json:"interval_s"`

	// Load 是 1/5/15 分钟负载，必须恰好 3 个元素。
	//
	// 这里用切片而不是 [3]float64 是刻意的：encoding/json 解码到 Go 数组时
	// 会静默丢弃多余元素，[1,2,3,4] 会被当成合法载荷接受，与“任何一处不合规
	// 都拒绝”直接冲突。用切片 + Validate 里显式校验长度才拦得住。
	Load []float64 `json:"load"`

	// CPUPct 是 CPU 使用率，取值 [0, 100]。
	CPUPct float64 `json:"cpu_pct"`

	Mem  Mem    `json:"mem"`
	Disk []Disk `json:"disk"`

	// Net 是网络流量。**可缺省**：老 agent 不发它，此时该节点的网络指标为空。
	//
	// 做成可缺省是为了让升级不用停机——新 server 同时接受新旧 agent，
	// 各节点升到新版后自然开始有流量数据。
	Net *Net `json:"net"`

	UptimeS      int64  `json:"uptime_s"`
	AgentVersion string `json:"agent_version"`
}

// Net 是网络流量（所有非回环网卡的合计）。
//
// 四个字段都用了指针，为的是能区分"上报了 0"和"压根没上报"：只要 net 对象
// 出现，四个字段就必须齐全，缺一个就整条拒绝——与"任何一处不合规都拒绝"
// 保持同一口径，不给半份数据留口子。
type Net struct {
	// RxBps / TxBps 是最近一个采集周期的收发速率，单位**字节/秒**。
	RxBps *float64 `json:"rx"`
	TxBps *float64 `json:"tx"`
	// RxTotal / TxTotal 是各网卡自计数以来的累计字节数（≈开机以来）。
	RxTotal *uint64 `json:"rx_total"`
	TxTotal *uint64 `json:"tx_total"`
}

// NetValues 是 net 解引用之后的确定值。
type NetValues struct {
	RxBps   float64
	TxBps   float64
	RxTotal uint64
	TxTotal uint64
}

// NewNet 由确定值构造上报用的 Net。
//
// 四个字段都是指针（用来区分"没上报"与"上报了 0"），每次手工取地址太啰嗦，
// 这里收口一个构造器，agent 侧只有这一个入口。
func NewNet(v NetValues) *Net {
	return &Net{
		RxBps: &v.RxBps, TxBps: &v.TxBps,
		RxTotal: &v.RxTotal, TxTotal: &v.TxTotal,
	}
}

// NetValues 返回确定值；第二个返回值为 false 表示这次上报没带 net
// （老 agent），或字段不齐（但那种情况在 Validate 就被拒了，走不到这里）。
//
// 存在的意义是让 store 与 api 不必各自做一遍四重 nil 判断。
func (r *Report) NetValues() (NetValues, bool) {
	n := r.Net
	if n == nil || n.RxBps == nil || n.TxBps == nil || n.RxTotal == nil || n.TxTotal == nil {
		return NetValues{}, false
	}
	return NetValues{
		RxBps: *n.RxBps, TxBps: *n.TxBps,
		RxTotal: *n.RxTotal, TxTotal: *n.TxTotal,
	}, true
}

// Mem 是内存占用。Used 由 MemTotal - MemAvailable 得出，不是 MemFree。
type Mem struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// Disk 是单个挂载点的占用。
type Disk struct {
	Mount string `json:"mount"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// Interval 返回该节点申报的上报间隔。
func (r *Report) Interval() time.Duration {
	return time.Duration(r.IntervalS) * time.Second
}

// OnlineThreshold 返回“当前是否在线”的判定阈值：last_seen 距今超过它就判离线。
//
// 容差取 2.5 倍间隔——既能容忍一次丢包或一次抖动，又不会把真正离线的机器
// 误判成在线（30 秒间隔 → 75 秒）。
func (r *Report) OnlineThreshold() time.Duration {
	return time.Duration(float64(r.IntervalS) * OnlineTolerance * float64(time.Second))
}
