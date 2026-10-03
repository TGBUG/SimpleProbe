// Command agent 跑在每台被监控机上。
//
// 它只做两件事：读 /proc 与 statvfs，然后把结果 POST 出去。非 root 即可运行，
// 不需要开任何入站端口，代码里没有 exec/shell/文件写（规格 §1）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/collect"
	"github.com/TGBUG/SimpleProbe/internal/config"
	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/push"
)

// version 可在构建时注入：-ldflags "-X main.version=0.1.0"
var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent 退出: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "agent.yaml", "配置文件路径")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}
	interval := time.Duration(cfg.Interval)

	client, err := push.New(push.Options{
		ServerURL: cfg.Server,
		Node:      cfg.Node,
		Token:     cfg.Token,
	})
	if err != nil {
		return err
	}

	collector := collect.New(interval, cfg.Mounts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("agent 启动",
		"server", cfg.Server, "node", cfg.Node,
		"interval", interval, "mounts", collector.Mounts(), "version", version)

	// 启动瞬间先采一次只为建立 CPU 基线，不上报——第一次没有差值可用。
	if _, err := collector.Sample(); err != nil && !errors.Is(err, collect.ErrWarmup) {
		logger.Warn("建立基线时读取失败，将在下个周期重试", "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("agent 退出")
			return nil
		case <-ticker.C:
			cycle(ctx, collector, client, cfg.Node, int(interval.Seconds()), logger)
		}
	}
}

// cycle 走一轮“采集 → 自检 → 上报”。
//
// 任何一步失败都只是丢掉这一轮，不重排队列：在线率由 server 侧按接收情况
// 统计，不需要 agent 补历史（规格 §3 的关键简化）。
func cycle(ctx context.Context, c *collect.Collector, client *push.Client, node string, intervalS int, logger *slog.Logger) {
	metrics, err := c.Sample()
	switch {
	case errors.Is(err, collect.ErrWarmup):
		logger.Debug("跳过本轮", "reason", err)
		return
	case err != nil:
		logger.Error("采集失败", "err", err)
		return
	}

	rep := &protocol.Report{
		V:            protocol.Version,
		Node:         node,
		IntervalS:    intervalS,
		Load:         metrics.Load,
		CPUPct:       metrics.CPUPct,
		Mem:          metrics.Mem,
		Disk:         metrics.Disk,
		UptimeS:      metrics.UptimeS,
		AgentVersion: version,
	}

	// 发送前用 server 的同一份校验自检：能在本地挡住的 bug，就不要等到
	// 对面回 400 才发现。
	if err := rep.Validate(); err != nil {
		logger.Error("自检未通过，本轮不上报（agent 有 bug 或配置不对）", "err", err)
		return
	}

	if err := client.Send(ctx, rep); err != nil {
		var rejected *push.RejectedError
		if errors.As(err, &rejected) {
			// 4xx 表示发出去的东西不对，下个周期重发也还是不对。
			logger.Error("server 拒绝了上报，请检查节点配置与协议版本", "err", err)
			return
		}
		logger.Warn("上报失败，下个周期会重试", "err", err)
	}
}
