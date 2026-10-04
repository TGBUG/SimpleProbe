// Command server 是探针的服务端：收上报、存 SQLite、对外提供只读查询。
//
// 它刻意只有三条路由，且没有任何向 agent 下发指令的能力——这是这套系统
// 与哪吒类面板最根本的区别（规格 §1）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/api"
	"github.com/TGBUG/SimpleProbe/internal/config"
	"github.com/TGBUG/SimpleProbe/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "server 退出: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "nodes.yaml", "配置文件路径")
	webDir := flag.String("web", "", "静态前端目录（留空则不提供页面）")
	sampleTTL := flag.Duration("sample-ttl", 30*24*time.Hour, "明细保留时长")
	bucketTTL := flag.Duration("bucket-ttl", 365*24*time.Hour, "在线率计数桶保留时长")
	check := flag.Bool("check", false, "只校验配置文件后退出（供部署脚本使用）")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}

	// -check 只解析配置就退出：让部署脚本能用「server 自己的解析器」
	// 校验它刚改过的文件，而不是自己再实现一遍规则。
	if *check {
		fmt.Printf("配置 OK：%d 个节点，listen=%s，db=%s\n", len(cfg.Nodes), cfg.Listen, cfg.DB)
		return nil
	}

	// 空节点列表是允许的（装完 server、还没加节点时的正常状态），但值得说一声，
	// 否则容易对着一个空面板怀疑是不是装坏了。
	if len(cfg.Nodes) == 0 {
		logger.Warn("配置里还没有任何节点，面板会是空的",
			"怎么加", "用 add-node.sh 加一个节点，再 systemctl reload（或 kill -HUP）")
	}

	st, err := store.Open(cfg.DB, cfg.Nodes)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	handler := api.New(st, api.Options{Logger: logger}).Handler()
	if *webDir != "" {
		// 前端与 API 同源，省掉 CORS；生产上也可以让反向代理
		// （nginx / Caddy / 任意）直接托管静态文件，这里就不用开 -web。
		// 服务端本身不假设前面有没有代理，它只是个 HTTP 端点。
		root := http.NewServeMux()
		root.Handle("/api/", handler)
		root.Handle("/", http.FileServer(http.Dir(*webDir)))
		handler = root
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	watchConfig(ctx, *configPath, st, logger)
	go purgeLoop(ctx, st, logger, *sampleTTL, *bucketTTL)

	go func() {
		logger.Info("server 启动", "listen", cfg.Listen, "db", cfg.DB, "nodes", len(cfg.Nodes))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("正在关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// watchConfig 监听 SIGHUP 热加载节点列表：加一台机器不需要重启服务。
//
// 任何一步失败都保留旧配置继续跑——运维改错一个字段不该让面板下线。
func watchConfig(ctx context.Context, path string, st *store.Store, logger *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	go func() {
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				newCfg, err := config.LoadServer(path)
				if err != nil {
					logger.Error("热加载失败，保留旧配置", "err", err)
					continue
				}
				if err := st.SetNodes(newCfg.Nodes); err != nil {
					logger.Error("热加载失败，保留旧配置", "err", err)
					continue
				}
				logger.Info("节点列表已热加载", "count", len(newCfg.Nodes))
			}
		}
	}()
}

// purgeLoop 清理过期数据。启动时先清一次（可能是停机数日后的第一次启动），
// 之后每 24 小时一次。
func purgeLoop(ctx context.Context, st *store.Store, logger *slog.Logger, sampleTTL, bucketTTL time.Duration) {
	purge := func() {
		start := time.Now()
		if err := st.Purge(time.Now(), sampleTTL, bucketTTL); err != nil {
			logger.Error("清理过期数据失败", "err", err)
			return
		}
		logger.Info("已清理过期数据", "耗时", time.Since(start).Round(time.Millisecond))
	}
	purge()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}
