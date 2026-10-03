// Package uptime 按规格 §4.2 计算在线率。
//
// 核心是一个纯函数：给定“窗口、纳入监控的时刻、节点申报的上报间隔、窗口内
// 实际收到的上报条数”，算出在线率。把它独立出来的原因是这个公式有两个
// 必须写对的细节——分母裁剪与上下夹紧——单独测试最省事。
package uptime

import "time"

// NamedWindow 是一个对外展示的在线率窗口。
type NamedWindow struct {
	Name string
	Dur  time.Duration
}

// Windows 是面板展示的三个窗口。
var Windows = []NamedWindow{
	{Name: "24h", Dur: 24 * time.Hour},
	{Name: "7d", Dur: 7 * 24 * time.Hour},
	{Name: "30d", Dur: 30 * 24 * time.Hour},
}

// Window 是一次在线率计算的输入。
type Window struct {
	// From 是窗口起点，通常是 now - Dur。
	From time.Time
	// To 是窗口终点，通常是 now。
	To time.Time
	// Since 是节点纳入监控的时刻：配置里声明的 since，或数据库中的首次上报时间。
	//
	// 它的作用是裁剪分母——新节点若直接按 30 天算分母，接入第一天
	// 会显示在线率 1%。
	Since time.Time
	// IntervalS 是节点最近一次申报的上报间隔（秒）。
	IntervalS int
	// Reports 是窗口内实际收到的上报条数。
	Reports int64
}

// Rate 返回 [0,1] 的在线率。
//
// 用“实际收到的条数 / 期望收到的条数”，而不是“有数据的分钟数 / 总分钟数”：
// 后者在节点上报间隔不整除 60 时会算错——90 秒间隔、100% 在线的节点会被
// 算成约 67%（规格 §4.2 的反例）。
func (w Window) Rate() float64 {
	start := w.From
	if w.Since.After(start) {
		start = w.Since
	}
	if !w.To.After(start) {
		// 可统计的时段为零：要么 since 在未来，要么窗口本身是空的。
		return 0
	}
	if w.IntervalS <= 0 {
		return 0
	}

	// 上面两个守卫保证 To > start（秒数为正）且 IntervalS > 0，所以期望值
	// 必然为正，不需要再判一次。
	expected := w.To.Sub(start).Seconds() / float64(w.IntervalS)
	rate := float64(w.Reports) / expected

	// 只夹上界：Reports 是数据库里 reports 列的求和，不可能为负，所以下界
	// 天然成立。上界则必须有——agent 若被配得比它申报的更频繁（改了配置
	// 没重启，或时钟跳变导致密发），实际条数会超过期望；重试造成的重复
	// 上报也会。不夹就会出现 >100% 的在线率。
	if rate > 1 {
		return 1
	}
	return rate
}

// Online 判断节点当前是否在线。
//
// 刻意不看在线率，只看“最后上报时间距今是否超过阈值”。阈值由调用方从
// protocol.Report.OnlineThreshold() 取（= 2.5 × 上报间隔），这样容差
// 的定义只有一处。
//
// lastSeen 为零值时返回 false——那代表这个节点从未上报过。
func Online(lastSeen, now time.Time, threshold time.Duration) bool {
	if lastSeen.IsZero() || threshold <= 0 {
		return false
	}
	return now.Sub(lastSeen) < threshold
}
