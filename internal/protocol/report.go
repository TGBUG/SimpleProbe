// Package protocol 定义 agent 与 server 之间唯一的数据契约。
//
// 这个包被两个二进制共享：agent 在发送前调用 Validate 自检，server 在接收后
// 用同一份代码再校验一遍。契约由编译器保证，不存在字段漂移的可能。
// 对应规格 docs/DESIGN.md 的 §3.7 与 §4.7。
package protocol

import "time"

// Version 是当前协议版本。任何字段增删都必须递增它。
//
// 服务端对不认识的版本直接拒绝（fail closed），代价是升级必须成对做：
// 先升 server（同时支持 vN 与 vN+1），再升 agent。
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

	UptimeS      int64  `json:"uptime_s"`
	AgentVersion string `json:"agent_version"`
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
