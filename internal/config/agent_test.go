package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAgentConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

func TestLoadAgent_OK(t *testing.T) {
	cfg, err := LoadAgent(writeAgentConfig(t, `
server: "https://probe.example.com"
node: "web01"
token: "s3cret"
interval: 45s
mounts: ["/", "/data"]
`))
	if err != nil {
		t.Fatalf("LoadAgent() 失败: %v", err)
	}
	if cfg.Server != "https://probe.example.com" || cfg.Node != "web01" || cfg.Token != "s3cret" {
		t.Errorf("基本字段不对: %+v", cfg)
	}
	if time.Duration(cfg.Interval) != 45*time.Second {
		t.Errorf("interval = %v，期望 45s", time.Duration(cfg.Interval))
	}
	if len(cfg.Mounts) != 2 || cfg.Mounts[1] != "/data" {
		t.Errorf("mounts = %v", cfg.Mounts)
	}
}

func TestLoadAgent_DefaultsInterval(t *testing.T) {
	cfg, err := LoadAgent(writeAgentConfig(t, "server: http://x\nnode: a\ntoken: t\n"))
	if err != nil {
		t.Fatalf("LoadAgent() 失败: %v", err)
	}
	if time.Duration(cfg.Interval) != DefaultInterval {
		t.Errorf("interval = %v，期望默认 %v", time.Duration(cfg.Interval), DefaultInterval)
	}
}

func TestLoadAgent_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{name: "缺 server", content: "node: a\ntoken: t\n", wantMsg: "server"},
		{name: "缺 node", content: "server: http://x\ntoken: t\n", wantMsg: "node"},
		{name: "缺 token", content: "server: http://x\nnode: a\n", wantMsg: "token"},
		{name: "node 含非法字符", content: "server: http://x\nnode: \"a b\"\ntoken: t\n", wantMsg: "node"},
		{name: "未知字段", content: "server: http://x\nnode: a\ntoken: t\nintervall: 30s\n", wantMsg: "intervall"},
		{name: "时长格式错误", content: "server: http://x\nnode: a\ntoken: t\ninterval: 三十秒\n", wantMsg: "时长"},
		{name: "时长不是字符串", content: "server: http://x\nnode: a\ntoken: t\ninterval: 30\n", wantMsg: "时长"},
		{name: "间隔低于下限", content: "server: http://x\nnode: a\ntoken: t\ninterval: 1s\n", wantMsg: "interval"},
		{name: "间隔高于上限", content: "server: http://x\nnode: a\ntoken: t\ninterval: 9999s\n", wantMsg: "interval"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadAgent(writeAgentConfig(t, tt.content))
			if err == nil {
				t.Fatal("期望报错，却成功了")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("错误信息 %q 未提及 %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestAgentIntervalMatchesProtocol 钉住“agent 配得出来的间隔，一定通得过上报校验”。
// 两边用不同的区间的话，agent 会一直发一直被拒。
func TestAgentIntervalMatchesProtocol(t *testing.T) {
	for _, interval := range []time.Duration{5 * time.Second, 30 * time.Second, time.Hour} {
		cfg := Agent{Server: "http://x", Node: "a", Token: "t", Interval: Duration(interval)}
		if err := cfg.Normalize(); err != nil {
			t.Errorf("interval=%v 应被接受，却报错: %v", interval, err)
		}
	}
}
