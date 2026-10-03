package collect

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// fakeFS 让采集器脱离真实机器：按路径返回预置内容。
type fakeFS map[string]string

func (f fakeFS) read(name string) ([]byte, error) {
	content, ok := f[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(content), nil
}

// statLine 拼一行 /proc/stat 的 cpu 汇总行，参数依次是
// user/nice/system/idle/iowait/irq/softirq/steal。
func statLine(v ...uint64) string {
	parts := []string{"cpu"}
	for _, x := range v {
		parts = append(parts, strconv.FormatUint(x, 10))
	}
	return strings.Join(parts, " ")
}

func newTestCollector(t *testing.T, now *time.Time) (*Collector, fakeFS) {
	t.Helper()
	files := fakeFS{
		"/proc/stat":    statLine(100, 0, 100, 800, 0, 0, 0, 0),
		"/proc/loadavg": "0.42 0.55 0.61 1/234 5678\n",
		"/proc/meminfo": "MemTotal:       16384000 kB\nMemAvailable:    8192000 kB\n",
		"/proc/uptime":  "1234567.89 9876543.21\n",
	}
	c := New(30*time.Second, []string{"/", "/data"})
	c.readFile = files.read
	c.statfs = func(mount string) (usage, error) {
		switch mount {
		case "/":
			return usage{Used: 50, Total: 100}, nil
		case "/data":
			return usage{Used: 25, Total: 200}, nil
		}
		return usage{}, errors.New("未知挂载点 " + mount)
	}
	c.now = func() time.Time { return *now }
	return c, files
}

func TestCollector_WarmupThenSample(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	// 第一轮：没有基线，必须跳过而不是上一条 0 或 100。
	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}

	// 30 秒后：总时间 1000 → 2000，空闲 800 → 1600，即 20% 使用率。
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)

	m, err := c.Sample()
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}

	if math.Abs(m.CPUPct-20) > 1e-9 {
		t.Errorf("CPUPct = %v，期望 20", m.CPUPct)
	}
	if len(m.Load) != 3 || m.Load[0] != 0.42 || m.Load[1] != 0.55 || m.Load[2] != 0.61 {
		t.Errorf("Load = %v，期望 [0.42 0.55 0.61]", m.Load)
	}
	if want := uint64(8192000 * 1024); m.Mem.Used != want {
		t.Errorf("Mem.Used = %d，期望 %d", m.Mem.Used, want)
	}
	if want := uint64(16384000 * 1024); m.Mem.Total != want {
		t.Errorf("Mem.Total = %d，期望 %d", m.Mem.Total, want)
	}
	if m.UptimeS != 1234567 {
		t.Errorf("UptimeS = %d，期望 1234567", m.UptimeS)
	}
	if len(m.Disk) != 2 {
		t.Fatalf("Disk 条目数 = %d，期望 2", len(m.Disk))
	}
	if want := (protocol.Disk{Mount: "/", Used: 50, Total: 100}); m.Disk[0] != want {
		t.Errorf("disk[0] = %+v，期望 %+v", m.Disk[0], want)
	}
	if want := (protocol.Disk{Mount: "/data", Used: 25, Total: 200}); m.Disk[1] != want {
		t.Errorf("disk[1] = %+v，期望 %+v", m.Disk[1], want)
	}
}

func TestCollector_LongGapTriggersWarmup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}

	// 间隔 91 秒 > 3 × 30 秒：机器休眠或进程被挂起过，本轮必须作废。
	now = now.Add(91 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("超长间隔应返回 ErrWarmup，得到 %v", err)
	}

	// 基线已在上一轮重建，再等 30 秒应恢复正常。
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(300, 0, 300, 2400, 0, 0, 0, 0)
	if _, err := c.Sample(); err != nil {
		t.Fatalf("重建基线后应能正常采样，得到 %v", err)
	}
}

func TestCollector_ExactlyAtIntervalLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}

	// 恰好 3 × interval 是允许的边界（判定用的是 “>”）。
	now = now.Add(90 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)
	if _, err := c.Sample(); err != nil {
		t.Fatalf("恰好等于上限时不应跳过，得到 %v", err)
	}
}

func TestCollector_ReadFailures(t *testing.T) {
	tests := []struct {
		name    string
		drop    string
		wantErr string
	}{
		{name: "缺 /proc/stat", drop: "/proc/stat", wantErr: "/proc/stat"},
		{name: "缺 /proc/loadavg", drop: "/proc/loadavg", wantErr: "/proc/loadavg"},
		{name: "缺 /proc/meminfo", drop: "/proc/meminfo", wantErr: "/proc/meminfo"},
		{name: "缺 /proc/uptime", drop: "/proc/uptime", wantErr: "/proc/uptime"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			c, files := newTestCollector(t, &now)

			if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
				t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
			}
			now = now.Add(30 * time.Second)
			files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)
			delete(files, tt.drop)

			_, err := c.Sample()
			if err == nil {
				t.Fatal("期望报错，却成功了")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("错误信息 %q 未提及 %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestCollector_StatfsFailureFailsWholeRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)
	c.statfs = func(string) (usage, error) { return usage{}, errors.New("statfs 炸了") }

	if _, err := c.Sample(); err == nil {
		t.Fatal("挂载点读不到时应整轮失败，而不是上报半真半假的数据")
	}
}

func TestNew_DefaultsToRootMount(t *testing.T) {
	c := New(30*time.Second, nil)
	if len(c.mounts) != 1 || c.mounts[0] != "/" {
		t.Errorf("默认挂载点 = %v，期望 [\"/\"]", c.mounts)
	}
}
