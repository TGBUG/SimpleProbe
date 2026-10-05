// Package collect 负责从 /proc 与 statvfs 读出本机指标。
//
// 解析部分全部是纯函数（输入是文件内容的字符串），因此可以脱离真实机器测试；
// 读取与系统调用只在 Collector 里出现。对应规格 §3.2–§3.6。
package collect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// ErrWarmup 表示本次采样不可用，调用方应当跳过这一轮而不是上报。
//
// 两种情况会触发：
//   - 第一次采样没有上一次的 CPU 计数器，算不出差值（规格 §3.3）；
//   - 距上次采样的间隔异常（过短说明时钟跳变，过长说明进程被挂起或机器休眠），
//     用这样的间隔做差分得到的 CPU 使用率没有意义。
var ErrWarmup = errors.New("collect: 采样基线未就绪或间隔异常，跳过本轮")

var (
	errCounterWentBackwards = errors.New("collect: CPU 计数器倒退，疑似回绕")
	errIdleExceedsTotal     = errors.New("collect: idle 增量大于总增量")
)

const (
	// userHZ 是 /proc/stat 计数器的频率。Linux 上 USER_HZ 恒为 100，
	// 与内核的 CONFIG_HZ 无关（后者只影响调度粒度）。用它把 tick 换算成秒。
	userHZ = 100

	// minDeltaCPU 是两次采样之间至少要有多少“CPU 时间总和”增量（秒）。
	// 低于它说明计数器几乎没动，差值会被噪声放大。
	minDeltaCPU = 0.1
	// maxIntervalFactor 是间隔上限相对上报间隔的倍数。
	maxIntervalFactor = 3
)

// CPUStat 是 /proc/stat 首行的一组累计计数器（单位：USER_HZ）。
type CPUStat struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total 返回用于计算使用率的分母。
//
// 注意不含 guest / guest_nice：内核已经把 guest 时间计入 user、guest_nice 计入 nice，
// 再加一遍就是重复计算，CPU 使用率会偏低（规格 §3.2）。
func (s CPUStat) Total() uint64 {
	return s.User + s.Nice + s.System + s.Idle + s.IOWait + s.IRQ + s.SoftIRQ + s.Steal
}

// IdleAll 返回“非忙”时间，必须把 iowait 算进来。
//
// 不算 iowait 的话，等待磁盘的时间会被当成 CPU 忙，IO 密集的机器 CPU 会显示虚高。
func (s CPUStat) IdleAll() uint64 {
	return s.Idle + s.IOWait
}

// ParseLoadavg 解析 /proc/loadavg，取前三个数（1/5/15 分钟负载）。
func ParseLoadavg(content string) ([]float64, error) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return nil, fmt.Errorf("collect: /proc/loadavg 字段不足，得到 %d 个", len(fields))
	}
	load := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, fmt.Errorf("collect: /proc/loadavg 第 %d 个字段无法解析: %w", i+1, err)
		}
		load[i] = v
	}
	return load, nil
}

// ParseStat 解析 /proc/stat 的 "cpu " 汇总行。
func ParseStat(content string) (CPUStat, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		// 字段数随内核版本变化（老内核没有 steal/guest），所以按需取值，
		// 缺失的一律当 0——这里刻意不用固定长度校验。
		vals := make([]uint64, 8)
		for i := 1; i < len(fields) && i <= len(vals); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return CPUStat{}, fmt.Errorf("collect: /proc/stat cpu 行第 %d 列无法解析: %w", i, err)
			}
			vals[i-1] = v
		}
		return CPUStat{
			User: vals[0], Nice: vals[1], System: vals[2], Idle: vals[3],
			IOWait: vals[4], IRQ: vals[5], SoftIRQ: vals[6], Steal: vals[7],
		}, nil
	}
	return CPUStat{}, errors.New("collect: /proc/stat 中没有 cpu 汇总行")
}

// CPUPct 用两次采样算 CPU 使用率（百分比，[0,100]）。
func CPUPct(prev, cur CPUStat) (float64, error) {
	totalDelta := cur.Total() - prev.Total()
	idleDelta := cur.IdleAll() - prev.IdleAll()

	// 计数器回绕或顺序颠倒，必须挡住：uint64 相减会得到一个巨大的数。
	if cur.Total() < prev.Total() || cur.IdleAll() < prev.IdleAll() {
		return 0, errCounterWentBackwards
	}
	if float64(totalDelta)/userHZ < minDeltaCPU {
		return 0, fmt.Errorf("%w: CPU 时间增量仅 %.3f 秒", ErrWarmup, float64(totalDelta)/userHZ)
	}
	if idleDelta > totalDelta {
		return 0, errIdleExceedsTotal
	}

	// 上面的检查保证了 0 <= idleDelta <= totalDelta，所以比值必然落在 [0,1]，
	// pct 不夹紧也已在 [0,100] 内，且两端是精确值（idle 增量为 0 时是 100，
	// idle 增量等于总增量时是 0）。刻意不写无用的夹紧分支。
	pct := (1 - float64(idleDelta)/float64(totalDelta)) * 100
	return pct, nil
}

// ParseMeminfo 解析 /proc/meminfo，返回已用与总内存。
//
// 已用 = MemTotal - MemAvailable。刻意不用 MemFree：它不含 cache/buffer，
// 几乎每台机都会显示“内存快满了”（规格 §3.4）。
func ParseMeminfo(content string) (protocol.Mem, error) {
	var total, available uint64
	var haveTotal, haveAvailable bool

	for _, line := range strings.Split(content, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal", "MemAvailable":
		default:
			continue
		}
		v, err := parseKB(rest)
		if err != nil {
			return protocol.Mem{}, fmt.Errorf("collect: %s 解析失败: %w", key, err)
		}
		if key == "MemTotal" {
			total, haveTotal = v, true
		} else {
			available, haveAvailable = v, true
		}
	}

	if !haveTotal {
		return protocol.Mem{}, errors.New("collect: /proc/meminfo 缺少 MemTotal")
	}
	if !haveAvailable {
		return protocol.Mem{}, errors.New("collect: /proc/meminfo 缺少 MemAvailable")
	}
	if total == 0 {
		return protocol.Mem{}, errors.New("collect: MemTotal 为 0")
	}
	// MemAvailable 偶尔会因内核统计口径略大于 MemTotal，夹紧避免协议层拒绝。
	if available > total {
		available = total
	}
	return protocol.Mem{Used: total - available, Total: total}, nil
}

// parseKB 解析 meminfo 里的 "  12345 kB" 形式的数值。
func parseKB(s string) (uint64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, errors.New("空值")
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	// meminfo 一律以 kB 为单位（内核用的是 KiB）。
	if len(fields) > 1 && !strings.EqualFold(fields[1], "kB") {
		return 0, fmt.Errorf("未知单位 %q", fields[1])
	}
	return v * 1024, nil
}

// ParseUptime 解析 /proc/uptime，返回整数秒。
func ParseUptime(content string) (int64, error) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return 0, errors.New("collect: /proc/uptime 为空")
	}
	// 形如 "1234567.89 9876543.21"，第一列是 uptime 秒（含小数）。
	whole, _, _ := strings.Cut(fields[0], ".")
	secs, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("collect: /proc/uptime 无法解析: %w", err)
	}
	if secs < 0 {
		return 0, errors.New("collect: /proc/uptime 为负数")
	}
	return secs, nil
}

// NetCounters 是所有非回环网卡累计字节数的合计。
type NetCounters struct {
	RxBytes uint64
	TxBytes uint64
}

// ParseNetDev 解析 /proc/net/dev，求和非回环网卡的收发字节。
//
// 排除 lo 是刻意的：本机进程间通信全走它，算进去会让“这台机器用了多少流量”
// 失去意义——一台只在本机内部聊天的机器看起来会比实际忙得多。
//
// 这是**网卡视角的合计**，不等于“实际外网流量”：跑容器时网桥（docker0、br-*）
// 与 veth 会把同一份流量重复计入。这是“对非回环网卡求和”这个定义的固有结果，
// 换来的是不必去猜哪块网卡才是“真的”。
func ParseNetDev(content string) (NetCounters, error) {
	var sum NetCounters
	sawInterface := false

	for _, line := range strings.Split(content, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			// 两行表头没有冒号。
			continue
		}
		name = strings.TrimSpace(name)
		// 表头第二行会被 Cut 出 "  face |bytes    packets ..." 这种左半边，
		// 里面有空格与竖线，用字符集挡掉。
		if name == "" || strings.ContainsAny(name, " |") {
			continue
		}

		// 字段顺序：rx bytes, rx packets, ...（共 8 列）, tx bytes, tx packets, ...
		// 所以收字节在第 0 列、发字节在第 8 列。
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return NetCounters{}, fmt.Errorf("collect: /proc/net/dev 的 %s 只有 %d 列", name, len(fields))
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return NetCounters{}, fmt.Errorf("collect: %s 的收字节数无法解析: %w", name, err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return NetCounters{}, fmt.Errorf("collect: %s 的发字节数无法解析: %w", name, err)
		}
		sawInterface = true

		if name == "lo" {
			continue
		}
		sum.RxBytes += rx
		sum.TxBytes += tx
	}

	if !sawInterface {
		// 一块网卡都没解析出来说明文件本身不对，而不是“没有流量”。
		// 只有 lo 的容器是正常情况，那时 sawInterface 已经是 true 了。
		return NetCounters{}, errors.New("collect: /proc/net/dev 里没有任何网卡")
	}
	return sum, nil
}

// NetRate 由两次计数算出速率（字节/秒）。
//
// 计数器**会倒退**：网卡被 down/up、驱动重载、或者 32 位计数器回绕。这时差值
// 没有意义，夹成 0 而不是给出一个负数或者天文数字——一次网卡重启不该在流量图上
// 戳出一根几十 GB/s 的针。
func NetRate(prev, cur NetCounters, elapsed time.Duration) (rxBps, txBps float64) {
	if elapsed <= 0 {
		return 0, 0
	}
	secs := elapsed.Seconds()
	if cur.RxBytes >= prev.RxBytes {
		rxBps = float64(cur.RxBytes-prev.RxBytes) / secs
	}
	if cur.TxBytes >= prev.TxBytes {
		txBps = float64(cur.TxBytes-prev.TxBytes) / secs
	}
	return rxBps, txBps
}
