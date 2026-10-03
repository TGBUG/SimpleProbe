package protocol_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// canonical 与规格 docs/DESIGN.md §3.7 的示例载荷字段完全一致
// （紧凑写法，便于做体积边界测试）。它必须永远能通过校验——这条测试的用途
// 是让规格与实现无法各自漂移。
const canonical = `{"v":1,"node":"web01","interval_s":30,` +
	`"load":[0.42,0.55,0.61],"cpu_pct":12.3,` +
	`"mem":{"used":3221225472,"total":8589934592},` +
	`"disk":[{"mount":"/","used":21474836480,"total":53687091200}],` +
	`"uptime_s":1234567,"agent_version":"0.1.0"}`

func mustDecode(t *testing.T, body string) *protocol.Report {
	t.Helper()
	rep, err := protocol.DecodeStrict(strings.NewReader(body))
	if err != nil {
		t.Fatalf("期望解析成功，却失败：%v", err)
	}
	return rep
}

func validReport(t *testing.T) protocol.Report {
	t.Helper()
	return *mustDecode(t, canonical)
}

// TestValidate 逐行覆盖规格 §4.7 的“取值”一栏。
// wantCode 为空表示期望通过。
func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*protocol.Report)
		wantCode  protocol.ErrorCode
		wantField string
	}{
		{name: "canonical 通过", mutate: func(*protocol.Report) {}},

		{name: "版本为 2", mutate: func(r *protocol.Report) { r.V = 2 },
			wantCode: protocol.CodeUnsupportedVer, wantField: "v"},
		{name: "版本为 0", mutate: func(r *protocol.Report) { r.V = 0 },
			wantCode: protocol.CodeUnsupportedVer, wantField: "v"},

		{name: "node 为空", mutate: func(r *protocol.Report) { r.Node = "" },
			wantCode: protocol.CodeInvalidField, wantField: "node"},
		{name: "node 含空格", mutate: func(r *protocol.Report) { r.Node = "web 01" },
			wantCode: protocol.CodeInvalidField, wantField: "node"},
		{name: "node 含路径穿越字符", mutate: func(r *protocol.Report) { r.Node = "../etc" },
			wantCode: protocol.CodeInvalidField, wantField: "node"},
		{name: "node 含斜杠", mutate: func(r *protocol.Report) { r.Node = "a/b" },
			wantCode: protocol.CodeInvalidField, wantField: "node"},
		{name: "node 超长", mutate: func(r *protocol.Report) { r.Node = strings.Repeat("a", protocol.MaxNodeIDLen+1) },
			wantCode: protocol.CodeInvalidField, wantField: "node"},
		{name: "node 长度恰好在上限", mutate: func(r *protocol.Report) { r.Node = strings.Repeat("a", protocol.MaxNodeIDLen) }},
		{name: "node 用点下划线连字符", mutate: func(r *protocol.Report) { r.Node = "hk-1.web_2" }},

		{name: "interval 为 0", mutate: func(r *protocol.Report) { r.IntervalS = 0 },
			wantCode: protocol.CodeInvalidField, wantField: "interval_s"},
		{name: "interval 低于下限", mutate: func(r *protocol.Report) { r.IntervalS = protocol.MinIntervalS - 1 },
			wantCode: protocol.CodeInvalidField, wantField: "interval_s"},
		{name: "interval 高于上限", mutate: func(r *protocol.Report) { r.IntervalS = protocol.MaxIntervalS + 1 },
			wantCode: protocol.CodeInvalidField, wantField: "interval_s"},
		{name: "interval 为负", mutate: func(r *protocol.Report) { r.IntervalS = -30 },
			wantCode: protocol.CodeInvalidField, wantField: "interval_s"},
		{name: "interval 取上限合法", mutate: func(r *protocol.Report) { r.IntervalS = protocol.MaxIntervalS }},
		{name: "interval 取下限合法", mutate: func(r *protocol.Report) { r.IntervalS = protocol.MinIntervalS }},

		{name: "load 为 nil", mutate: func(r *protocol.Report) { r.Load = nil },
			wantCode: protocol.CodeInvalidField, wantField: "load"},
		{name: "load 只有 2 个元素", mutate: func(r *protocol.Report) { r.Load = []float64{1, 2} },
			wantCode: protocol.CodeInvalidField, wantField: "load"},
		{name: "load 有 4 个元素", mutate: func(r *protocol.Report) { r.Load = []float64{1, 2, 3, 4} },
			wantCode: protocol.CodeInvalidField, wantField: "load"},
		{name: "load 为负", mutate: func(r *protocol.Report) { r.Load = []float64{-1, 0, 0} },
			wantCode: protocol.CodeInvalidField, wantField: "load[0]"},
		{name: "load 含 NaN", mutate: func(r *protocol.Report) { r.Load = []float64{math.NaN(), 0, 0} },
			wantCode: protocol.CodeInvalidField, wantField: "load[0]"},
		{name: "load 含 +Inf", mutate: func(r *protocol.Report) { r.Load = []float64{0, math.Inf(1), 0} },
			wantCode: protocol.CodeInvalidField, wantField: "load[1]"},
		{name: "load 含 -Inf", mutate: func(r *protocol.Report) { r.Load = []float64{0, 0, math.Inf(-1)} },
			wantCode: protocol.CodeInvalidField, wantField: "load[2]"},
		{name: "load 为 0 合法", mutate: func(r *protocol.Report) { r.Load = []float64{0, 0, 0} }},

		{name: "cpu 为负", mutate: func(r *protocol.Report) { r.CPUPct = -0.1 },
			wantCode: protocol.CodeInvalidField, wantField: "cpu_pct"},
		{name: "cpu 超过 100", mutate: func(r *protocol.Report) { r.CPUPct = 100.1 },
			wantCode: protocol.CodeInvalidField, wantField: "cpu_pct"},
		{name: "cpu 含 NaN", mutate: func(r *protocol.Report) { r.CPUPct = math.NaN() },
			wantCode: protocol.CodeInvalidField, wantField: "cpu_pct"},
		{name: "cpu 为 0 合法", mutate: func(r *protocol.Report) { r.CPUPct = 0 }},
		{name: "cpu 为 100 合法", mutate: func(r *protocol.Report) { r.CPUPct = 100 }},

		{name: "mem.total 为 0", mutate: func(r *protocol.Report) { r.Mem.Total = 0 },
			wantCode: protocol.CodeInvalidField, wantField: "mem.total"},
		{name: "mem.used 大于 total", mutate: func(r *protocol.Report) { r.Mem.Used = r.Mem.Total + 1 },
			wantCode: protocol.CodeInvalidField, wantField: "mem.used"},
		{name: "mem.used 等于 total 合法", mutate: func(r *protocol.Report) { r.Mem.Used = r.Mem.Total }},

		{name: "disk 为 nil", mutate: func(r *protocol.Report) { r.Disk = nil },
			wantCode: protocol.CodeInvalidField, wantField: "disk"},
		{name: "disk 为空数组", mutate: func(r *protocol.Report) { r.Disk = []protocol.Disk{} },
			wantCode: protocol.CodeInvalidField, wantField: "disk"},
		{name: "disk 条目过多", mutate: func(r *protocol.Report) {
			r.Disk = make([]protocol.Disk, protocol.MaxDiskEntries+1)
			for i := range r.Disk {
				r.Disk[i] = protocol.Disk{Mount: "/m" + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1), Used: 1, Total: 2}
			}
		}, wantCode: protocol.CodeInvalidField, wantField: "disk"},
		{name: "mount 为空", mutate: func(r *protocol.Report) { r.Disk[0].Mount = "" },
			wantCode: protocol.CodeInvalidField, wantField: "disk[0].mount"},
		{name: "mount 超长", mutate: func(r *protocol.Report) { r.Disk[0].Mount = strings.Repeat("/", protocol.MaxMountLen+1) },
			wantCode: protocol.CodeInvalidField, wantField: "disk[0].mount"},
		{name: "mount 重复", mutate: func(r *protocol.Report) {
			r.Disk = append(r.Disk, protocol.Disk{Mount: "/", Used: 1, Total: 2})
		}, wantCode: protocol.CodeInvalidField, wantField: "disk[1].mount"},
		{name: "disk.total 为 0", mutate: func(r *protocol.Report) { r.Disk[0].Total = 0 },
			wantCode: protocol.CodeInvalidField, wantField: "disk[0].total"},
		{name: "disk.used 大于 total", mutate: func(r *protocol.Report) { r.Disk[0].Used = r.Disk[0].Total + 1 },
			wantCode: protocol.CodeInvalidField, wantField: "disk[0].used"},
		{name: "disk.used 等于 total 合法", mutate: func(r *protocol.Report) { r.Disk[0].Used = r.Disk[0].Total }},
		{name: "多个挂载点合法", mutate: func(r *protocol.Report) {
			r.Disk = append(r.Disk, protocol.Disk{Mount: "/data", Used: 1, Total: 2})
		}},

		{name: "uptime 为负", mutate: func(r *protocol.Report) { r.UptimeS = -1 },
			wantCode: protocol.CodeInvalidField, wantField: "uptime_s"},
		{name: "uptime 为 0 合法", mutate: func(r *protocol.Report) { r.UptimeS = 0 }},

		{name: "agent_version 为空", mutate: func(r *protocol.Report) { r.AgentVersion = "" },
			wantCode: protocol.CodeInvalidField, wantField: "agent_version"},
		{name: "agent_version 超长", mutate: func(r *protocol.Report) {
			r.AgentVersion = strings.Repeat("1", protocol.MaxAgentVersionLen+1)
		}, wantCode: protocol.CodeInvalidField, wantField: "agent_version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := validReport(t)
			tt.mutate(&rep)

			err := rep.Validate()
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("期望通过，却得到错误：%v", err)
				}
				return
			}

			var ve *protocol.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("期望 *ValidationError，得到 %v", err)
			}
			if ve.Code != tt.wantCode {
				t.Errorf("错误码 = %q，期望 %q（%s）", ve.Code, tt.wantCode, ve.Msg)
			}
			if ve.Field != tt.wantField {
				t.Errorf("字段 = %q，期望 %q", ve.Field, tt.wantField)
			}
		})
	}
}

// TestDecodeStrict 覆盖规格 §4.7 的“传输 / 解析 / 结构”三栏。
func TestDecodeStrict(t *testing.T) {
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(canonical, old) {
			t.Fatalf("canonical 中不存在片段 %q", old)
		}
		return strings.Replace(canonical, old, new, 1)
	}

	tests := []struct {
		name     string
		body     string
		wantCode protocol.ErrorCode // 空表示期望成功
	}{
		{name: "规格示例载荷通过", body: canonical},
		{name: "顶层未知字段被拒绝",
			body:     replace(`"agent_version":"0.1.0"`, `"agent_version":"0.1.0","evil":true`),
			wantCode: protocol.CodeUnknownField},
		{name: "嵌套未知字段被拒绝",
			body:     replace(`"total":8589934592`, `"total":8589934592,"x":1`),
			wantCode: protocol.CodeUnknownField},
		{name: "未知字段即使值为 null 也被拒绝",
			body:     replace(`"agent_version":"0.1.0"`, `"agent_version":"0.1.0","evil":null`),
			wantCode: protocol.CodeUnknownField},
		{name: "尾随内容被拒绝", body: canonical + `{}`,
			wantCode: protocol.CodeTrailingData},
		{name: "尾随空白允许", body: canonical + "  \n\t "},
		{name: "空请求体被拒绝", body: "", wantCode: protocol.CodeMalformed},
		{name: "纯空白请求体被拒绝", body: "   ", wantCode: protocol.CodeMalformed},
		{name: "截断的 JSON 被拒绝", body: `{"v":1,"node":"web01"`,
			wantCode: protocol.CodeMalformed},
		{name: "语法错误的 JSON 被拒绝", body: `{"v":1,"node":}`,
			wantCode: protocol.CodeMalformed},
		{name: "load 为字符串被拒绝",
			body:     replace(`[0.42,0.55,0.61]`, `"x"`),
			wantCode: protocol.CodeInvalidType},
		{name: "mem.used 为负数被拒绝（无符号字段）",
			body:     replace(`"used":3221225472`, `"used":-1`),
			wantCode: protocol.CodeInvalidType},
		{name: "缺 interval_s 被拒绝",
			body:     replace(`"interval_s":30,`, ``),
			wantCode: protocol.CodeInvalidField},
		{name: "缺 load 被拒绝",
			body:     replace(`"load":[0.42,0.55,0.61],`, ``),
			wantCode: protocol.CodeInvalidField},
		// 关键用例：encoding/json 解码到 Go 数组时会静默丢弃多余元素，
		// 若协议里用 [3]float64，这条就会被漏过。
		{name: "load 多一个元素被拒绝",
			body:     replace(`[0.42,0.55,0.61]`, `[0.42,0.55,0.61,9.9]`),
			wantCode: protocol.CodeInvalidField},
		{name: "load 少一个元素被拒绝",
			body:     replace(`[0.42,0.55,0.61]`, `[0.42,0.55]`),
			wantCode: protocol.CodeInvalidField},
		{name: "版本不符被拒绝",
			body:     replace(`"v":1`, `"v":2`),
			wantCode: protocol.CodeUnsupportedVer},
		{name: "恰好等于体积上限可通过",
			body: canonical + strings.Repeat(" ", protocol.MaxBodyBytes-len(canonical))},
		{name: "超过体积上限被拒绝",
			body:     canonical + strings.Repeat(" ", protocol.MaxBodyBytes),
			wantCode: protocol.CodeBodyTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := protocol.DecodeStrict(strings.NewReader(tt.body))

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("期望成功，却得到错误：%v", err)
				}
				if rep == nil {
					t.Fatal("成功时不应返回 nil")
				}
				return
			}

			if rep != nil {
				t.Errorf("失败时不应返回载荷，得到 %+v", rep)
			}
			var ve *protocol.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("期望 *ValidationError，得到 %v", err)
			}
			if ve.Code != tt.wantCode {
				t.Errorf("错误码 = %q，期望 %q（%s）", ve.Code, tt.wantCode, ve.Msg)
			}
		})
	}
}

func TestValidationErrorHTTPStatus(t *testing.T) {
	tests := []struct {
		code protocol.ErrorCode
		want int
	}{
		{protocol.CodeBodyTooLarge, 413},
		{protocol.CodeMalformed, 400},
		{protocol.CodeUnknownField, 400},
		{protocol.CodeInvalidType, 400},
		{protocol.CodeTrailingData, 400},
		{protocol.CodeUnsupportedVer, 400},
		{protocol.CodeInvalidField, 400},
	}
	for _, tt := range tests {
		ve := &protocol.ValidationError{Code: tt.code}
		if got := ve.HTTPStatus(); got != tt.want {
			t.Errorf("%s.HTTPStatus() = %d，期望 %d", tt.code, got, tt.want)
		}
	}
}

// TestDerivedValues 固定住 §4.2 依赖的两个派生量。
func TestDerivedValues(t *testing.T) {
	rep := validReport(t)

	if got, want := rep.Interval(), 30*time.Second; got != want {
		t.Errorf("Interval() = %v，期望 %v", got, want)
	}
	if got, want := rep.OnlineThreshold(), 75*time.Second; got != want {
		t.Errorf("OnlineThreshold() = %v，期望 %v", got, want)
	}

	// 90 秒间隔是规格 §4.2 举的反例，容差应随之变成 225 秒。
	rep.IntervalS = 90
	if got, want := rep.OnlineThreshold(), 225*time.Second; got != want {
		t.Errorf("90s 间隔的 OnlineThreshold() = %v，期望 %v", got, want)
	}
}
