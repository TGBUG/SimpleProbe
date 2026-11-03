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

// netDevHeader 是 /proc/net/dev 固定不变的两行表头。
const netDevHeader = "Inter-|   Receive                                                |  Transmit\n" +
	" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"

// netDevLine 拼一行 /proc/net/dev。只填收发字节，其余 14 列补 0。
func netDevLine(name string, rx, tx uint64) string {
	zeros := func() []string {
		out := make([]string, 0, 8)
		for i := 0; i < 7; i++ {
			out = append(out, "0")
		}
		return out
	}
	recv := append([]string{strconv.FormatUint(rx, 10)}, zeros()...)
	send := append([]string{strconv.FormatUint(tx, 10)}, zeros()...)
	return name + ": " + strings.Join(recv, " ") + " " + strings.Join(send, " ")
}

func newTestCollector(t *testing.T, now *time.Time) (*Collector, fakeFS) {
	t.Helper()
	files := fakeFS{
		"/proc/stat":    statLine(100, 0, 100, 800, 0, 0, 0, 0),
		"/proc/loadavg": "0.42 0.55 0.61 1/234 5678\n",
		"/proc/meminfo": "MemTotal:       16384000 kB\nMemAvailable:    8192000 kB\n",
		"/proc/uptime":  "1234567.89 9876543.21\n",
		"/proc/net/dev": netDevHeader +
			netDevLine("lo", 1000, 1000) + "\n" +
			netDevLine("eth0", 5000, 3000) + "\n",
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
	// lo 故意暴涨，用来证明它被排除在合计之外。
	files["/proc/net/dev"] = netDevHeader +
		netDevLine("lo", 999_999_999, 999_999_999) + "\n" +
		netDevLine("eth0", 8000, 3600) + "\n"

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

	// 网络：eth0 收 5000→8000、发 3000→3600，30 秒，即 100 与 20 字节/秒。
	// 同时把 lo 的计数拉到天文数字——它必须被完全忽略，否则这两个断言会飞掉。
	if math.Abs(m.Net.RxBps-100) > 1e-9 {
		t.Errorf("Net.RxBps = %v，期望 100（lo 的增量必须被排除）", m.Net.RxBps)
	}
	if math.Abs(m.Net.TxBps-20) > 1e-9 {
		t.Errorf("Net.TxBps = %v，期望 20", m.Net.TxBps)
	}
	// 累计值取本次计数器原值，同样只算非回环网卡。
	if m.Net.RxTotal != 8000 || m.Net.TxTotal != 3600 {
		t.Errorf("Net 累计 = rx %d / tx %d，期望 8000 / 3600",
			m.Net.RxTotal, m.Net.TxTotal)
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
		{name: "缺 /proc/net/dev", drop: "/proc/net/dev", wantErr: "/proc/net/dev"},
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

// TestCollector_AllMountsFailFailsRound 说的是“**全部**读不到才作废”。
//
// 只坏一块盘的情况见 TestCollector_OneBadMountDoesNotKillTheRound——
// 两件事必须分开，否则一块 U 盘掉了会让整台机器失联。
func TestCollector_AllMountsFailFailsRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)
	c.statfs = func(string) (usage, error) { return usage{}, errors.New("statfs 炸了") }

	if _, err := c.Sample(); err == nil {
		t.Fatal("所有挂载点都读不到时应整轮失败——协议要求至少一个挂载点")
	}
}

// TestCollector_OneBadMountDoesNotKillTheRound 盯的是多盘场景里最要命的那种：
// 一块盘没了（U 盘拔了、网络盘掉线、路径写错），**其它盘和整台机器的监控**
// 不该跟着一起失联。
func TestCollector_OneBadMountDoesNotKillTheRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now) // 配置是 ["/", "/data"]

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(200, 0, 200, 1600, 0, 0, 0, 0)
	c.statfs = func(mount string) (usage, error) {
		if mount == "/data" {
			return usage{}, errors.New("no such file or directory")
		}
		return usage{Used: 50, Total: 100}, nil
	}

	m, err := c.Sample()
	if err != nil {
		t.Fatalf("一块盘失败不该让整轮失败，得到 %v", err)
	}
	if len(m.Disk) != 1 || m.Disk[0].Mount != "/" {
		t.Errorf("应只报出可用的那块盘，得到 %+v", m.Disk)
	}
	if len(m.DiskIssues) != 1 || m.DiskIssues[0].Mount != "/data" {
		t.Errorf("跳过的盘应记进 DiskIssues 供日志使用，得到 %+v", m.DiskIssues)
	}
}

// TestCollector_SkipsZeroSizeMount 盯的是 /proc、/sys 这类伪文件系统。
//
// 它们 statfs 成功但 total=0。协议层要求 disk[].total 不为 0（那是对的，
// 0 容量说明数据本身有问题），所以采集层必须先滤掉，否则一条坏数据会让
// 整个载荷被拒——节点直接离线。
func TestCollector_SkipsZeroSizeMount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, _ := newTestCollector(t, &now)
	c.mounts = []string{"/", "/proc"}
	c.statfs = func(mount string) (usage, error) {
		if mount == "/proc" {
			return usage{Used: 0, Total: 0}, nil
		}
		return usage{Used: 50, Total: 100}, nil
	}

	disk, issues, err := c.readDisk()
	if err != nil {
		t.Fatalf("伪文件系统应被跳过而不是报错，得到 %v", err)
	}
	if len(disk) != 1 || disk[0].Mount != "/" {
		t.Errorf("应只保留真正的存储，得到 %+v", disk)
	}
	if len(issues) != 1 || issues[0].Mount != "/proc" {
		t.Errorf("跳过的 /proc 应记进 issues，得到 %+v", issues)
	}
}

// TestNew_EmptyMountsMeansAutoDiscover 钉住“空 = 自动发现”这个语义。
//
// 曾经空表示“只报根分区”，那让多盘机器必须手工枚举每一块盘，漏一块就看不见。
func TestNew_EmptyMountsMeansAutoDiscover(t *testing.T) {
	c := New(30*time.Second, nil)
	if len(c.Mounts()) != 0 {
		t.Errorf("mounts 留空应表示自动发现，Mounts() = %v", c.Mounts())
	}
}

// TestCollector_AutoDiscoverUsesProcMounts 验证自动发现真的走 /proc/mounts，
// 并且会排除伪文件系统、按设备去重。
func TestCollector_AutoDiscoverUsesProcMounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)
	c.mounts = nil // 自动发现
	// 测试里的路径是编的，不真的去 stat：一律当作目录。
	c.isDir = func(string) bool { return true }
	files["/proc/mounts"] = strings.Join([]string{
		"/dev/sda1 / ext4 rw 0 0",
		"proc /proc proc rw 0 0",
		"/dev/sdb1 /data xfs rw 0 0",
		"/dev/sdb1 /data/backup xfs rw 0 0", // bind mount，应被去重
		"tmpfs /run tmpfs rw 0 0",
	}, "\n")

	disk, _, err := c.readDisk()
	if err != nil {
		t.Fatalf("自动发现失败: %v", err)
	}
	got := make([]string, 0, len(disk))
	for _, d := range disk {
		got = append(got, d.Mount)
	}
	want := []string{"/", "/data"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("自动发现的挂载点 = %v，期望 %v（排除 proc/tmpfs，并按设备去重）", got, want)
	}
}
