package uptime

import (
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func TestWindow_Rate(t *testing.T) {
	tests := []struct {
		name  string
		win   Window
		want  float64
		about string
	}{
		{
			name: "24 小时满勤（30 秒间隔 → 期望 2880 条）",
			win: Window{
				From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-48 * time.Hour),
				IntervalS: 30, Reports: 2880,
			},
			want: 1,
		},
		{
			name: "24 小时少一半",
			win: Window{
				From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-48 * time.Hour),
				IntervalS: 30, Reports: 1440,
			},
			want: 0.5,
		},
		{
			// 规格 §4.2 的反例：90 秒间隔、100% 在线。
			// 按“有数据的分钟数/总分钟数”会算成约 0.67，按计数法必须是 1。
			name: "90 秒间隔满勤仍是 100%",
			win: Window{
				From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-48 * time.Hour),
				IntervalS: 90, Reports: 960, // 24*3600/90
			},
			want:  1,
			about: "间隔不整除 60 时不能算错",
		},
		{
			name: "分母被 since 裁剪：刚接入 1 小时，满勤",
			win: Window{
				From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-1 * time.Hour),
				IntervalS: 30, Reports: 120, // 3600/30
			},
			want:  1,
			about: "不裁剪的话会显示 4% 而不是 100%",
		},
		{
			name: "分母被 since 裁剪：刚接入 1 小时，只收到一半",
			win: Window{
				From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-1 * time.Hour),
				IntervalS: 30, Reports: 60,
			},
			want: 0.5,
		},
		{
			name: "重试造成重复上报也被夹到 100%",
			win: Window{
				From: base.Add(-time.Hour), To: base, Since: base.Add(-2 * time.Hour),
				IntervalS: 30, Reports: 500, // 期望 120
			},
			want:  1,
			about: "上夹紧",
		},
		{
			name: "一条都没收到",
			win: Window{
				From: base.Add(-time.Hour), To: base, Since: base.Add(-2 * time.Hour),
				IntervalS: 30, Reports: 0,
			},
			want: 0,
		},
		{
			name: "since 在未来",
			win: Window{
				From: base.Add(-time.Hour), To: base, Since: base.Add(time.Hour),
				IntervalS: 30, Reports: 10,
			},
			want: 0,
		},
		{
			name: "窗口为空",
			win: Window{
				From: base, To: base, Since: base.Add(-time.Hour),
				IntervalS: 30, Reports: 10,
			},
			want: 0,
		},
		{
			name: "上报间隔为 0（防御）",
			win: Window{
				From: base.Add(-time.Hour), To: base, Since: base.Add(-2 * time.Hour),
				IntervalS: 0, Reports: 10,
			},
			want: 0,
		},
		{
			name: "上报间隔为负（防御）",
			win: Window{
				From: base.Add(-time.Hour), To: base, Since: base.Add(-2 * time.Hour),
				IntervalS: -30, Reports: 10,
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.win.Rate()
			if math.Abs(got-tt.want) > 1e-9 {
				msg := ""
				if tt.about != "" {
					msg = "（" + tt.about + "）"
				}
				t.Errorf("Rate() = %v，期望 %v%s", got, tt.want, msg)
			}
		})
	}
}

// TestWindow_Rate_NotMinuteBuckets 用一个具体数字把“计数法 vs 分钟占比法”
// 的差别钉死：如果谁把实现改回数分钟桶，这条会立刻红。
func TestWindow_Rate_NotMinuteBuckets(t *testing.T) {
	// 90 秒间隔，整整 24 小时在线：960 条上报，分布在 1440 个分钟里，
	// 其中有数据的分钟只有 960 个（每 3 分钟 2 个）。
	win := Window{
		From: base.Add(-24 * time.Hour), To: base, Since: base.Add(-48 * time.Hour),
		IntervalS: 90, Reports: 960,
	}
	if got := win.Rate(); math.Abs(got-1) > 1e-9 {
		t.Errorf("Rate() = %v，期望 1（按分钟桶算会得到约 0.67）", got)
	}
}

func TestOnline(t *testing.T) {
	const threshold = 75 * time.Second

	tests := []struct {
		name     string
		lastSeen time.Time
		now      time.Time
		thr      time.Duration
		want     bool
	}{
		{name: "刚上报过", lastSeen: base, now: base.Add(10 * time.Second), thr: threshold, want: true},
		{name: "容差边界内", lastSeen: base, now: base.Add(74 * time.Second), thr: threshold, want: true},
		{name: "恰好等于阈值算离线", lastSeen: base, now: base.Add(75 * time.Second), thr: threshold, want: false},
		{name: "超过阈值", lastSeen: base, now: base.Add(10 * time.Minute), thr: threshold, want: false},
		{name: "从未上报", lastSeen: time.Time{}, now: base, thr: threshold, want: false},
		{name: "阈值为 0（防御）", lastSeen: base, now: base, thr: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Online(tt.lastSeen, tt.now, tt.thr); got != tt.want {
				t.Errorf("Online() = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestWindows(t *testing.T) {
	want := map[string]time.Duration{
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	if len(Windows) != len(want) {
		t.Fatalf("窗口数 = %d，期望 %d", len(Windows), len(want))
	}
	for _, w := range Windows {
		if want[w.Name] != w.Dur {
			t.Errorf("窗口 %s 时长 = %v，期望 %v", w.Name, w.Dur, want[w.Name])
		}
	}
}
