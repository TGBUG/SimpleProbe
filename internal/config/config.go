// Package config 读取 server 与 agent 的配置。
//
// 节点身份（id / 名称 / token）只写在 server 配置里：不落库、没有自注册入口，
// token 明文存放。理由见规格 §1 与 §2 的取舍 1。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// Node 是一个被监控节点的身份。
type Node struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	Token       string `yaml:"token"`
	// Since 是可选的“纳入监控的起始时刻”，用于裁剪在线率分母。
	// 留空则以数据库里的首次上报时间为准。
	Since time.Time `yaml:"since"`
}

// Server 是 server 端配置。
type Server struct {
	// Listen 是 HTTP 监听地址。默认只监听回环，公网入口交给反向代理。
	Listen string `yaml:"listen"`
	DB     string `yaml:"db"`
	// ReportPerMinute 是**单个节点**每分钟的上报次数上限。
	// 留空（0）用 DefaultReportPerMinute；它按节点计数，不按 IP——
	// agent 很可能都在同一个出口或反向代理后面。
	ReportPerMinute int `yaml:"report_per_minute"`
	// Nodes 的顺序即面板展示顺序——刻意不排序，让运维用书写顺序控制它。
	Nodes []Node `yaml:"nodes"`
}

const (
	DefaultListen = "127.0.0.1:8080"
	DefaultDB     = "./data/probe.db"

	// DefaultReportPerMinute 是单节点每分钟的上报次数上限。
	//
	// 这个值必须容得下**协议允许的最快间隔**，否则就会出现"server 接受一份
	// 它自己拒绝服务的配置"这种自相矛盾：MinIntervalS = 5，也就是 12 次/分钟，
	// 再乘上 push 客户端单周期最多 3 次尝试，最坏合法情况是 36 次/分钟。
	// 默认取 60，留出余量。
	//
	// 它的职责是拦住失控的死循环（那种会打到几千次/分钟），不是做访问控制：
	// 能拿出 token 的人本来就有权上报这个节点的数据。
	DefaultReportPerMinute = 60

	// MaxReportPerMinute 是配置上限，防止把限流写成事实上的关闭。
	MaxReportPerMinute = 100000
)

// LoadServer 读取并严格校验 server 配置。
//
// 与上报一样采用严格口径：配置里出现未知字段（多半是拼错的键名）直接报错，
// 而不是默默忽略——否则 `displayname:` 这种笔误要等到面板上看到 id 才发现。
func LoadServer(path string) (*Server, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}

	var cfg Server
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	return &cfg, nil
}

// Normalize 填补默认值并校验，直接修改接收者。
//
// 导出它是为了让 SIGHUP 热加载复用同一套规则。
func (c *Server) Normalize() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DB == "" {
		return errors.New("db 不能为空")
	}
	if c.ReportPerMinute < 0 || c.ReportPerMinute > MaxReportPerMinute {
		return fmt.Errorf("report_per_minute 必须在 [1, %d] 之间，得到 %d",
			MaxReportPerMinute, c.ReportPerMinute)
	}
	if c.ReportPerMinute == 0 {
		c.ReportPerMinute = DefaultReportPerMinute
	}
	// nodes 允许为空。这不是配置错误，而是装 server 之后的正常中间状态：
	// 先起服务、再用 add-node.sh 逐个加节点。面板本来就能显示"还没有节点"。
	// （曾经这里返回错误，结果 install.sh server 一装上就在自检那步失败——
	//   必须一次给一个节点才能装，那正是我们要去掉的限制。）
	// 空列表仍然值得提醒，但那属于运行期的事，由 server 启动时告警，不在这里拦。

	seen := make(map[string]struct{}, len(c.Nodes))
	for i := range c.Nodes {
		n := &c.Nodes[i]

		// 复用上报校验的同一份 id 规则：配置里能写出来的 id，
		// 一定要能通过上报校验，否则那个节点永远上不了线。
		if err := protocol.ValidateNodeID(n.ID); err != nil {
			return fmt.Errorf("nodes[%d].id: %w", i, err)
		}
		if _, dup := seen[n.ID]; dup {
			return fmt.Errorf("nodes[%d].id: 重复的节点 id %q", i, n.ID)
		}
		seen[n.ID] = struct{}{}

		if n.Token == "" {
			return fmt.Errorf("nodes[%d] (%s).token: 不能为空", i, n.ID)
		}
		if n.DisplayName == "" {
			n.DisplayName = n.ID
		}
	}
	return nil
}
