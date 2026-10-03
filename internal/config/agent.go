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

// Duration 让 YAML 里可以写 "30s" 这种人类可读的时长。
//
// yaml.v3 默认把 time.Duration 当整数解析，直接写 30s 会失败。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return errors.New("时长必须写成字符串，例如 30s")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Agent 是 agent 端配置。
//
// interval 只写在这里一处：server 端不再重复配置，频率随载荷自描述
// （见规格 §4.2 与决策 ⑨）。
type Agent struct {
	Server   string   `yaml:"server"`
	Node     string   `yaml:"node"`
	Token    string   `yaml:"token"`
	Interval Duration `yaml:"interval"`
	// Mounts 是要监控的挂载点，留空则只报根分区。
	Mounts []string `yaml:"mounts"`
}

// DefaultInterval 是默认上报间隔。
const DefaultInterval = 30 * time.Second

// LoadAgent 读取并严格校验 agent 配置。
func LoadAgent(path string) (*Agent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}

	var cfg Agent
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
func (c *Agent) Normalize() error {
	switch {
	case c.Server == "":
		return errors.New("server 不能为空")
	case c.Node == "":
		return errors.New("node 不能为空")
	case c.Token == "":
		return errors.New("token 不能为空")
	}
	if err := protocol.ValidateNodeID(c.Node); err != nil {
		return fmt.Errorf("node: %w", err)
	}

	if c.Interval == 0 {
		c.Interval = Duration(DefaultInterval)
	}
	// 用上报校验的同一套区间，避免配出一个 server 一定会拒绝的间隔。
	secs := int(time.Duration(c.Interval).Seconds())
	if secs < protocol.MinIntervalS || secs > protocol.MaxIntervalS {
		return fmt.Errorf("interval 必须在 [%d, %d] 秒之间，得到 %s",
			protocol.MinIntervalS, protocol.MaxIntervalS, time.Duration(c.Interval))
	}
	return nil
}
