package protocol_test

import (
	"strings"
	"testing"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// TestValidateNodeID 覆盖导出给配置层复用的那份 id 规则。
func TestValidateNodeID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "普通 slug", id: "web01"},
		{name: "含点下划线连字符", id: "hk-1.web_2"},
		{name: "长度恰好在上限", id: strings.Repeat("a", protocol.MaxNodeIDLen)},
		{name: "空", id: "", wantErr: true},
		{name: "含空格", id: "a b", wantErr: true},
		{name: "含斜杠", id: "a/b", wantErr: true},
		{name: "路径穿越", id: "../etc", wantErr: true},
		{name: "含换行（日志注入）", id: "a\nb", wantErr: true},
		{name: "超长", id: strings.Repeat("a", protocol.MaxNodeIDLen+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := protocol.ValidateNodeID(tt.id)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateNodeID(%q) 期望报错", tt.id)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateNodeID(%q) 意外报错: %v", tt.id, err)
			}
		})
	}
}
