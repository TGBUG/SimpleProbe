package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

const goodConfig = `
listen: "127.0.0.1:9999"
db: "/tmp/probe.db"

nodes:
  - id: web01
    display_name: "Web 01"
    token: "aaa"
    since: 2026-10-01
  - id: nas
    token: "bbb"
`

func TestLoadServer_OK(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatalf("LoadServer() 失败: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.DB != "/tmp/probe.db" {
		t.Errorf("DB = %q", cfg.DB)
	}
	if len(cfg.Nodes) != 2 {
		t.Fatalf("节点数 = %d，期望 2", len(cfg.Nodes))
	}

	// 配置顺序必须被保留：运维用书写顺序决定面板顺序。
	if cfg.Nodes[0].ID != "web01" || cfg.Nodes[1].ID != "nas" {
		t.Errorf("节点顺序被改动: %v", cfg.Nodes)
	}
	// display_name 缺省时回落到 id。
	if cfg.Nodes[1].DisplayName != "nas" {
		t.Errorf("display_name = %q，期望回落到 id", cfg.Nodes[1].DisplayName)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !cfg.Nodes[0].Since.Equal(want) {
		t.Errorf("since = %v，期望 %v", cfg.Nodes[0].Since, want)
	}
	if !cfg.Nodes[1].Since.IsZero() {
		t.Errorf("未声明的 since 应为零值，得到 %v", cfg.Nodes[1].Since)
	}
}

func TestLoadServer_AppliesDefaultListen(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, "db: /tmp/x.db\nnodes:\n  - id: a\n    token: t\n"))
	if err != nil {
		t.Fatalf("LoadServer() 失败: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q，期望默认值 %q", cfg.Listen, DefaultListen)
	}
}

func TestLoadServer_SinceRFC3339(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, "db: /tmp/x.db\nnodes:\n  - id: a\n    token: t\n    since: 2026-10-01T08:30:00Z\n"))
	if err != nil {
		t.Fatalf("LoadServer() 失败: %v", err)
	}
	if want := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC); !cfg.Nodes[0].Since.Equal(want) {
		t.Errorf("since = %v，期望 %v", cfg.Nodes[0].Since, want)
	}
}

func TestLoadServer_ReportPerMinute(t *testing.T) {
	// 留空用默认值。
	cfg, err := LoadServer(writeConfig(t, "db: /tmp/x.db\nnodes:\n  - id: a\n    token: t\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReportPerMinute != DefaultReportPerMinute {
		t.Errorf("默认 = %d，期望 %d", cfg.ReportPerMinute, DefaultReportPerMinute)
	}

	// 这个断言是这条配置项存在的理由：默认额度必须容得下**最坏合法情况**，
	// 也就是协议允许的最快间隔，乘上 agent 单周期最多的尝试次数。
	// 一旦低于它，就会出现"server 接受一份它自己拒绝服务的配置"——
	// 曾经写死 10/分钟，于是任何 interval < 6s 的节点都会每分钟必然吃 429。
	worst := (60 / protocol.MinIntervalS) * 3
	if DefaultReportPerMinute < worst {
		t.Errorf("默认额度 %d 低于最坏合法频率 %d，会有正常节点被无谓限流",
			DefaultReportPerMinute, worst)
	}

	// 显式配置生效。
	cfg, err = LoadServer(writeConfig(t,
		"db: /tmp/x.db\nreport_per_minute: 120\nnodes:\n  - id: a\n    token: t\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReportPerMinute != 120 {
		t.Errorf("显式配置 = %d，期望 120", cfg.ReportPerMinute)
	}
}

func TestLoadServer_AllowsEmptyNodes(t *testing.T) {
	// 空 nodes 不是配置错误，而是装完 server 之后、加第一个节点之前的正常状态。
	// 曾经这里报错，导致 install.sh server 一装就在自检那步失败。
	cfg, err := LoadServer(writeConfig(t, "db: /tmp/x.db\nnodes: []\n"))
	if err != nil {
		t.Fatalf("空 nodes 应被接受，却报错: %v", err)
	}
	if len(cfg.Nodes) != 0 {
		t.Errorf("节点数 = %d，期望 0", len(cfg.Nodes))
	}
}

func TestLoadServer_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{name: "缺 db", content: "nodes:\n  - id: a\n    token: t\n", wantMsg: "db"},
		{name: "report_per_minute 为负", content: "db: /tmp/x.db\nreport_per_minute: -1\n", wantMsg: "report_per_minute"},
		{name: "report_per_minute 过大", content: "db: /tmp/x.db\nreport_per_minute: 100001\n", wantMsg: "report_per_minute"},
		{name: "节点缺 id", content: "db: /tmp/x.db\nnodes:\n  - token: t\n", wantMsg: "id"},
		{name: "节点 id 非法字符", content: "db: /tmp/x.db\nnodes:\n  - id: \"a b\"\n    token: t\n", wantMsg: "id"},
		{name: "节点 id 重复", content: "db: /tmp/x.db\nnodes:\n  - id: a\n    token: t\n  - id: a\n    token: u\n", wantMsg: "重复"},
		{name: "节点缺 token", content: "db: /tmp/x.db\nnodes:\n  - id: a\n", wantMsg: "token"},
		{name: "未知字段（键名拼错）", content: "db: /tmp/x.db\nnodes:\n  - id: a\n    token: t\n    displayname: typo\n", wantMsg: "displayname"},
		{name: "未知顶层字段", content: "db: /tmp/x.db\nnods:\n  - id: a\n", wantMsg: "nods"},
		{name: "YAML 语法错误", content: "db: [unclosed\n", wantMsg: "解析"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadServer(writeConfig(t, tt.content))
			if err == nil {
				t.Fatal("期望报错，却成功了")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("错误信息 %q 未提及 %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestLoadServer_MissingFile(t *testing.T) {
	_, err := LoadServer(filepath.Join(t.TempDir(), "不存在.yaml"))
	if err == nil {
		t.Fatal("期望报错，却成功了")
	}
	if !strings.Contains(err.Error(), "读取配置") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// TestConfigIDsAreAcceptedByProtocol 钉住“配置里能写出来的 id 一定能上报”。
// 两边用了不同的规则的话，这个测试会红。
func TestConfigIDsAreAcceptedByProtocol(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatalf("LoadServer() 失败: %v", err)
	}
	for _, n := range cfg.Nodes {
		if err := protocol.ValidateNodeID(n.ID); err != nil {
			t.Errorf("配置接受了 id %q，但上报校验拒绝它: %v", n.ID, err)
		}
	}
}
