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
	// Net 同时带速率（差分算得）与累计（网卡计数器原值）。
	Net     protocol.NetValues
	UptimeS int64
}

// Collector 按固定间隔采集本机指标。
//
// 它不是并发安全的：应由单个 goroutine 顺序驱动。
type Collector struct {
	interval time.Duration
	mounts   []string

	// 以下三个字段在测试中被替换，生产路径用真实实现。
	readFile func(string) ([]byte, error)
	statfs   func(string) (usage, error)
	now      func() time.Time

	prevStat *CPUStat
	prevNet  *NetCounters
	prevAt   time.Time
}

// New 创建采集器。mounts 为空时默认只采集根分区。
func New(interval time.Duration, mounts []string) *Collector {
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	return &Collector{
		interval: interval,
		mounts:   mounts,
		readFile: os.ReadFile,
		statfs:   statfsUsage,
		now:      time.Now,
	}
}

// Mounts 返回正在采集的挂载点列表（只读）。
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
	disk, err := c.readDisk()
	if err != nil {
		return Metrics{}, err
	}
	uptime, err := c.readUptime()
	if err != nil {
		return Metrics{}, err
	}

	return Metrics{
		Load:   load,
		CPUPct: cpuPct,
		Mem:    mem,
		Disk:   disk,
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

func (c *Collector) readDisk() ([]protocol.Disk, error) {
	out := make([]protocol.Disk, 0, len(c.mounts))
	for _, mount := range c.mounts {
		u, err := c.statfs(mount)
		if err != nil {
			return nil, fmt.Errorf("读取挂载点 %s 的用量: %w", mount, err)
		}
		out = append(out, protocol.Disk{Mount: mount, Used: u.Used, Total: u.Total})
	}
	return out, nil
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
