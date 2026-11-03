package collect

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// Metrics 是采集到的本机指标。不含节点身份与上报间隔——那些来自 agent 配置。
type Metrics struct {
	Load   []float64
	CPUPct float64
	Mem    protocol.Mem
	Disk   []protocol.Disk
	// DiskIssues 记录本轮被跳过的挂载点（不存在、没权限、不是存储）。
	// 它们不进上报载荷——协议要求 disk[].total 不为 0——但调用方应当写日志，
	// 否则一块盘悄悄消失会毫无痕迹。
	DiskIssues []MountIssue
	// Net 同时带速率（差分算得）与累计（网卡计数器原值）。
	Net     protocol.NetValues
	UptimeS int64
}

// MountIssue 是一个被跳过的挂载点及原因。
type MountIssue struct {
	Mount  string
	Reason string
}

// Collector 按固定间隔采集本机指标。
//
// 它不是并发安全的：应由单个 goroutine 顺序驱动。
type Collector struct {
	interval time.Duration
	// mounts 为空表示自动发现（见 DiscoverMounts）。
	mounts []string

	// 以下四个字段在测试中被替换，生产路径用真实实现。
	readFile func(string) ([]byte, error)
	statfs   func(string) (usage, error)
	isDir    func(string) bool
	now      func() time.Time

	prevStat *CPUStat
	prevNet  *NetCounters
	prevAt   time.Time
}

// New 创建采集器。
//
// mounts 为空表示**自动发现**所有真实存储设备。显式给了就只采集给的那些，
// 一个不多一个不少——想要“只看根分区”就写 ["/"]。
func New(interval time.Duration, mounts []string) *Collector {
	return &Collector{
		interval: interval,
		mounts:   append([]string(nil), mounts...),
		readFile: os.ReadFile,
		statfs:   statfsUsage,
		isDir:    func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() },
		now:      time.Now,
	}
}

// Mounts 返回配置的挂载点列表。**空切片表示自动发现**，不是“什么都没配”。
func (c *Collector) Mounts() []string {
	return append([]string(nil), c.mounts...)
}

// Sample 采集一次。
//
// 返回 ErrWarmup 时调用方应当跳过这一轮而不是上报——首次采样、进程被挂起、
// 机器休眠唤醒之后都属于这种情况（规格 §3.3）。
//
// 任何一项指标读取失败都会让整轮失败：与其上报一条半真半假的数据，
// 不如不发。这是协议层严格口径的延伸。
func (c *Collector) Sample() (Metrics, error) {
	now := c.now()

	cur, err := c.readCPU()
	if err != nil {
		return Metrics{}, err
	}
	curNet, err := c.readNet()
	if err != nil {
		return Metrics{}, err
	}

	// 无论本轮是否可用，cur 都是当前最新的基线，先换上再说。
	prev, prevAt := c.prevStat, c.prevAt
	prevNet := c.prevNet
	c.prevStat, c.prevAt = &cur, now
	c.prevNet = &curNet

	if prev == nil || prevNet == nil {
		return Metrics{}, fmt.Errorf("%w: 尚无 CPU/网络基线", ErrWarmup)
	}
	elapsed := now.Sub(prevAt)
	if elapsed > maxIntervalFactor*c.interval {
		return Metrics{}, fmt.Errorf("%w: 距上次采样 %v，超过 %v",
			ErrWarmup, elapsed.Round(time.Millisecond), maxIntervalFactor*c.interval)
	}

	cpuPct, err := CPUPct(*prev, cur)
	if err != nil {
		return Metrics{}, err
	}
	// 与 CPU 同一个 elapsed：两者都是差分，用两个时间源只会引入不一致。
	rxBps, txBps := NetRate(*prevNet, curNet, elapsed)

	load, err := c.readLoad()
	if err != nil {
		return Metrics{}, err
	}
	mem, err := c.readMem()
	if err != nil {
		return Metrics{}, err
	}
	disk, diskIssues, err := c.readDisk()
	if err != nil {
		return Metrics{}, err
	}
	uptime, err := c.readUptime()
	if err != nil {
		return Metrics{}, err
	}

	return Metrics{
		Load:       load,
		CPUPct:     cpuPct,
		Mem:        mem,
		Disk:       disk,
		DiskIssues: diskIssues,
		Net: protocol.NetValues{
			RxBps: rxBps, TxBps: txBps,
			// 累计值直接取本次读到的计数器——它是“开机以来”，不是差值。
			RxTotal: curNet.RxBytes, TxTotal: curNet.TxBytes,
		},
		UptimeS: uptime,
	}, nil
}

func (c *Collector) readCPU() (CPUStat, error) {
	b, err := c.readFile("/proc/stat")
	if err != nil {
		return CPUStat{}, fmt.Errorf("读取 /proc/stat: %w", err)
	}
	return ParseStat(string(b))
}

func (c *Collector) readLoad() ([]float64, error) {
	b, err := c.readFile("/proc/loadavg")
	if err != nil {
		return nil, fmt.Errorf("读取 /proc/loadavg: %w", err)
	}
	return ParseLoadavg(string(b))
}

func (c *Collector) readMem() (protocol.Mem, error) {
	b, err := c.readFile("/proc/meminfo")
	if err != nil {
		return protocol.Mem{}, fmt.Errorf("读取 /proc/meminfo: %w", err)
	}
	return ParseMeminfo(string(b))
}

func (c *Collector) readNet() (NetCounters, error) {
	b, err := c.readFile("/proc/net/dev")
	if err != nil {
		return NetCounters{}, fmt.Errorf("读取 /proc/net/dev: %w", err)
	}
	return ParseNetDev(string(b))
}

func (c *Collector) readUptime() (int64, error) {
	b, err := c.readFile("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("读取 /proc/uptime: %w", err)
	}
	return ParseUptime(string(b))
}

// readDisk 采集挂载点用量。
//
// 与 CPU / 内存那种“一个整体指标”不同，磁盘是一组**互相独立**的设备：
// 一块盘读不到，不该把其它盘和整台机器的监控一起拖下水。所以这里逐个跳过
// 失败的挂载点，把原因交回调用方记日志。
//
// 三种情况会跳过：
//   - statfs 失败（路径不存在：U 盘拔了、手滑写错、NFS 掉线）；
//   - total 为 0（/proc、/sys 这类伪文件系统，没有容量概念）；
//   - 全部失败时——那说明配置整个不对，整轮作废（协议也要求至少一个挂载点）。
func (c *Collector) readDisk() ([]protocol.Disk, []MountIssue, error) {
	targets := c.mounts
	auto := len(targets) == 0
	if auto {
		targets = c.discoverMounts()
	}

	out := make([]protocol.Disk, 0, len(targets))
	var issues []MountIssue

	for _, mount := range targets {
		u, err := c.statfs(mount)
		if err != nil {
			issues = append(issues, MountIssue{Mount: mount, Reason: err.Error()})
			continue
		}
		if u.Total == 0 {
			issues = append(issues, MountIssue{Mount: mount, Reason: "总容量为 0，不是存储设备"})
			continue
		}
		out = append(out, protocol.Disk{Mount: mount, Used: u.Used, Total: u.Total})
	}

	if len(out) == 0 {
		how := "配置"
		if auto {
			how = "自动发现"
		}
		return nil, issues, fmt.Errorf("磁盘：%s的 %d 个挂载点全部不可用", how, len(targets))
	}
	return out, issues, nil
}

// discoverMounts 从 /proc/mounts 找出真实存储。
//
// 自动发现失败时退回根分区：磁盘发现不该成为整轮采集的绊脚石——读不到
// /proc/mounts 的机器极少，但真遇到时，「只报根分区」远好过「什么都报不了」。
func (c *Collector) discoverMounts() []string {
	b, err := c.readFile("/proc/mounts")
	if err != nil {
		return []string{"/"}
	}
	found := DiscoverMounts(string(b), c.isDir)
	if len(found) == 0 {
		return []string{"/"}
	}

	out := make([]string, 0, len(found))
	for _, m := range found {
		out = append(out, m.MountPoint)
		// 协议对挂载点数量有上限，超了就按排序截断（DiscoverMounts 已排好序）。
		if len(out) >= protocol.MaxDiskEntries {
			break
		}
	}
	return out
}

type usage struct{ Used, Total uint64 }

// statfsUsage 取挂载点的容量。
//
// 用 Frsize 而不是 Bsize：Bsize 是“最优 IO 块大小”，Frsize 才是片段大小，
// 在部分文件系统上两者不相等，用错会算出错误的容量（规格 §3.5）。
//
// 已用空间按 df 的口径算成 total - free（free 是 Bfree，含保留块），
// 而不是减去 Bavail——Bavail 表达的是“非特权用户还能写多少”，那是另一个问题。
func statfsUsage(mount string) (usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return usage{}, err
	}
	frsize := uint64(st.Frsize)
	if frsize == 0 {
		// 极少数文件系统不填 Frsize，退回 Bsize。
		frsize = uint64(st.Bsize)
	}
	total := st.Blocks * frsize
	free := st.Bfree * frsize
	if free > total {
		free = total
	}
	return usage{Used: total - free, Total: total}, nil
}
