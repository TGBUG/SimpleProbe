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
	// Nodes 的顺序即面板展示顺序——刻意不排序，让运维用书写顺序控制它。
	Nodes []Node `yaml:"nodes"`
}

const (
	DefaultListen = "127.0.0.1:8080"
	DefaultDB     = "/var/lib/probe/probe.db"
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
	if len(c.Nodes) == 0 {
		return errors.New("nodes 不能为空")
	}

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
