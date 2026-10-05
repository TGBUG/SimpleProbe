package collect

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseNetDev(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantRx  uint64
		wantTx  uint64
		wantErr string
	}{
		{
			name: "多网卡求和、排除 lo",
			content: netDevHeader +
				netDevLine("lo", 111, 222) + "\n" +
				netDevLine("eth0", 1000, 2000) + "\n" +
				netDevLine("eth1", 30, 40) + "\n",
			wantRx: 1030, wantTx: 2040,
		},
		{
			// 只有 lo 的容器是真实存在的（--network none / host 网络），
			// 这不是错误，合计就是 0。
			name:    "只有 lo 时合计为 0 而不是报错",
			content: netDevHeader + netDevLine("lo", 111, 222) + "\n",
			wantRx:  0, wantTx: 0,
		},
		{
			name: "网卡别名与网桥名字里的点和连字符不影响解析",
			content: netDevHeader +
				netDevLine("eth0.100", 7, 8) + "\n" +
				netDevLine("br-abc123", 1, 2) + "\n",
			wantRx: 8, wantTx: 10,
		},
		{
			name:    "列数不足要报错，而不是把缺的当 0",
			content: netDevHeader + "eth0: 1 2 3\n",
			wantErr: "列",
		},
		{
			name:    "只有表头、一块网卡都没有要报错",
			content: netDevHeader,
			wantErr: "没有任何网卡",
		},
		{
			name:    "字节数不是数字要报错",
			content: netDevHeader + "eth0: abc 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n",
			wantErr: "无法解析",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNetDev(tt.content)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错（含 %q），却成功了：%+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("错误信息 %q 未提及 %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got.RxBytes != tt.wantRx || got.TxBytes != tt.wantTx {
				t.Errorf("合计 = rx %d / tx %d，期望 rx %d / tx %d",
					got.RxBytes, got.TxBytes, tt.wantRx, tt.wantTx)
			}
		})
	}
}

func TestNetRate(t *testing.T) {
	tests := []struct {
		name           string
		prev, cur      NetCounters
		elapsed        time.Duration
		wantRx, wantTx float64
	}{
		{
			name: "正常差分",
			prev: NetCounters{1000, 2000}, cur: NetCounters{4000, 8000},
			elapsed: 30 * time.Second, wantRx: 100, wantTx: 200,
		},
		{
			// 网卡 down/up 会让计数器归零。差值没有意义，夹成 0，
			// 而不是给一个负数或者一根几十 GB/s 的针。
			name: "收侧倒退夹成 0，发侧照常",
			prev: NetCounters{5000, 5000}, cur: NetCounters{100, 6000},
			elapsed: 30 * time.Second, wantRx: 0, wantTx: 1000.0 / 30,
		},
		{
			name: "两侧都倒退就是 0",
			prev: NetCounters{5000, 5000}, cur: NetCounters{1, 2},
			elapsed: 30 * time.Second, wantRx: 0, wantTx: 0,
		},
		{
			name: "零间隔不能算出 NaN/Inf",
			prev: NetCounters{1, 1}, cur: NetCounters{2, 2},
			elapsed: 0, wantRx: 0, wantTx: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rx, tx := NetRate(tt.prev, tt.cur, tt.elapsed)
			if math.Abs(rx-tt.wantRx) > 1e-9 || math.Abs(tx-tt.wantTx) > 1e-9 {
				t.Errorf("NetRate() = (%v, %v)，期望 (%v, %v)", rx, tx, tt.wantRx, tt.wantTx)
			}
			if math.IsNaN(rx) || math.IsInf(rx, 0) || math.IsNaN(tx) || math.IsInf(tx, 0) {
				t.Errorf("速率不能是 NaN/Inf，得到 (%v, %v)", rx, tx)
			}
		})
	}
}

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
