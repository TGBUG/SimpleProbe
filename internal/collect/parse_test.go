package collect

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestParseLoadavg(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []float64
		wantErr bool
	}{
		{name: "典型", in: "0.42 0.55 0.61 1/234 5678\n", want: []float64{0.42, 0.55, 0.61}},
		{name: "高负载", in: "12.5 9.25 8.00 3/1000 4242", want: []float64{12.5, 9.25, 8}},
		{name: "全零", in: "0.00 0.00 0.00 0/1 1", want: []float64{0, 0, 0}},
		{name: "只有两个字段", in: "0.1 0.2", wantErr: true},
		{name: "空内容", in: "", wantErr: true},
		{name: "非数字", in: "abc def ghi", wantErr: true},
		{name: "科学计数法", in: "1e-3 2e-3 3e-3 0/1 1", want: []float64{1e-3, 2e-3, 3e-3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLoadavg(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseLoadavg() = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestParseStat(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    CPUStat
		wantErr bool
	}{
		{
			name: "典型",
			in:   "cpu  100 0 50 800 20 0 5 0 0 0\ncpu0 1 2 3 4\n",
			want: CPUStat{User: 100, Nice: 0, System: 50, Idle: 800, IOWait: 20, IRQ: 0, SoftIRQ: 5, Steal: 0},
		},
		{
			name: "cpu0 行在前且必须被跳过",
			in:   "cpu0 999 999 999 999\ncpu  10 0 5 80 2 0 1 0\n",
			want: CPUStat{User: 10, Nice: 0, System: 5, Idle: 80, IOWait: 2, IRQ: 0, SoftIRQ: 1, Steal: 0},
		},
		{
			name: "老内核列数较少",
			in:   "cpu  10 0 5 80\n",
			want: CPUStat{User: 10, System: 5, Idle: 80},
		},
		{name: "没有 cpu 汇总行", in: "cpu0 1 2 3 4\n", wantErr: true},
		{name: "空内容", in: "", wantErr: true},
		{name: "非数字", in: "cpu  x 0 5 80\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStat(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseStat() = %+v，期望 %+v", got, tt.want)
			}
		})
	}
}

// TestCPUStat_GuestNotDoubleCounted 锁住规格 §3.2 的第一个坑。
//
// /proc/stat 的 guest 列已经被内核计入 user、guest_nice 计入 nice。
// 如果实现里去读第 9/10 列并把它们加进 Total()，就是重复计算。
func TestCPUStat_GuestNotDoubleCounted(t *testing.T) {
	st, err := ParseStat("cpu  10 0 5 80 2 0 1 0 999 999\n")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	const want = uint64(10 + 0 + 5 + 80 + 2 + 0 + 1 + 0)
	if got := st.Total(); got != want {
		t.Errorf("Total() = %d，期望 %d（guest 列 999 不应被计入）", got, want)
	}
}

func TestCPUPct(t *testing.T) {
	tests := []struct {
		name    string
		prev    CPUStat
		cur     CPUStat
		want    float64
		wantErr error // nil 表示不期望错误
	}{
		{
			name: "基本 20%",
			prev: CPUStat{User: 100, System: 100, Idle: 800},
			cur:  CPUStat{User: 200, System: 200, Idle: 1600},
			want: 20,
		},
		{
			name: "满载 100%",
			prev: CPUStat{User: 100, Idle: 800},
			cur:  CPUStat{User: 1100, Idle: 800},
			want: 100,
		},
		{
			name: "全空闲 0%",
			prev: CPUStat{User: 100, Idle: 800},
			cur:  CPUStat{User: 100, Idle: 1800},
			want: 0,
		},
		{
			// iowait 必须算进空闲。若算成忙，这个用例会得到 40% 而不是 20%。
			name: "iowait 计入空闲",
			prev: CPUStat{User: 100, System: 100, Idle: 700, IOWait: 100},
			cur:  CPUStat{User: 200, System: 200, Idle: 1400, IOWait: 200},
			want: 20,
		},
		{
			name:    "计数器回绕",
			prev:    CPUStat{User: 1000, Idle: 1000},
			cur:     CPUStat{User: 10, Idle: 10},
			wantErr: errCounterWentBackwards,
		},
		{
			name:    "增量过小触发预热",
			prev:    CPUStat{User: 100, Idle: 800},
			cur:     CPUStat{User: 100, Idle: 800},
			wantErr: ErrWarmup,
		},
		{
			// 单个字段倒退（user 少了 100）但总量仍在增长：
			// 这会让 idle 增量大于总增量，必须挡住而不是算出负数或 >100。
			name:    "单字段倒退导致 idle 增量超过总增量",
			prev:    CPUStat{User: 1000, Idle: 100},
			cur:     CPUStat{User: 900, Idle: 500},
			wantErr: errIdleExceedsTotal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CPUPct(tt.prev, tt.cur)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("期望错误 %v，得到 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CPUPct() = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    uint64 // 期望的 Used
		wantErr bool
	}{
		{
			name: "典型",
			in: "MemTotal:       16384000 kB\n" +
				"MemFree:         1000000 kB\n" +
				"MemAvailable:    8192000 kB\n" +
				"Buffers:           10000 kB\n",
			want: 8192000 * 1024,
		},
		{
			name: "MemAvailable 大于 MemTotal 时夹紧为 0",
			in:   "MemTotal:        1000 kB\nMemAvailable:    2000 kB\n",
			want: 0,
		},
		{
			name: "MemAvailable 为 0",
			in:   "MemTotal:        1000 kB\nMemAvailable:       0 kB\n",
			want: 1000 * 1024,
		},
		{name: "缺 MemAvailable", in: "MemTotal: 1000 kB\n", wantErr: true},
		{name: "缺 MemTotal", in: "MemAvailable: 1000 kB\n", wantErr: true},
		{name: "MemTotal 为 0", in: "MemTotal: 0 kB\nMemAvailable: 0 kB\n", wantErr: true},
		{name: "未知单位", in: "MemTotal: 1000 MB\nMemAvailable: 10 MB\n", wantErr: true},
		{name: "MemTotal 没有值", in: "MemTotal:\nMemAvailable: 100 kB\n", wantErr: true},
		{name: "MemTotal 非数字", in: "MemTotal: abc kB\nMemAvailable: 100 kB\n", wantErr: true},
		{name: "空内容", in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMeminfo(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got.Used != tt.want {
				t.Errorf("Used = %d，期望 %d", got.Used, tt.want)
			}
		})
	}
}

func TestParseMeminfo_Total(t *testing.T) {
	got, err := ParseMeminfo("MemTotal: 16384000 kB\nMemAvailable: 8192000 kB\n")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if want := uint64(16384000 * 1024); got.Total != want {
		t.Errorf("Total = %d，期望 %d", got.Total, want)
	}
}

func TestParseUptime(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "典型", in: "1234567.89 9876543.21\n", want: 1234567},
		{name: "零", in: "0.00 0.00\n", want: 0},
		{name: "无小数部分", in: "42 100\n", want: 42},
		{name: "空内容", in: "", wantErr: true},
		{name: "非数字", in: "abc 123\n", wantErr: true},
		{name: "负数", in: "-5.0 1.0\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUptime(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseUptime() = %d，期望 %d", got, tt.want)
			}
		})
	}
}
