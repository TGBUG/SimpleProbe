package protocol

import (
	"fmt"
	"math"
	"net/http"
)

// ErrorCode 是机器可读的错误码。HTTP 层据此决定状态码；
// 响应体只回 code 与一句短描述，不回显 payload。
type ErrorCode string

const (
	CodeMalformed      ErrorCode = "malformed_json"
	CodeBodyTooLarge   ErrorCode = "body_too_large"
	CodeUnknownField   ErrorCode = "unknown_field"
	CodeInvalidType    ErrorCode = "invalid_type"
	CodeTrailingData   ErrorCode = "trailing_data"
	CodeUnsupportedVer ErrorCode = "unsupported_version"
	CodeInvalidField   ErrorCode = "invalid_field"
)

// ValidationError 表示上报载荷没有通过严格校验。
type ValidationError struct {
	Code  ErrorCode
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Msg)
}

// HTTPStatus 把校验错误映射成 HTTP 状态码。
// 401（身份）与 429（限流）由上层在各自阶段产生，不属于这里。
func (e *ValidationError) HTTPStatus() int {
	if e.Code == CodeBodyTooLarge {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Code: CodeInvalidField, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Validate 严格校验一份载荷，不做任何宽容处理：不补默认值、不忽略异常。
// agent 发送前与 server 接收后共用这一份实现。
func (r *Report) Validate() error {
	if r.V != Version {
		return &ValidationError{
			Code:  CodeUnsupportedVer,
			Field: "v",
			Msg:   fmt.Sprintf("期望 %d，收到 %d", Version, r.V),
		}
	}
	if err := validateNodeID(r.Node); err != nil {
		return err
	}
	if r.IntervalS < MinIntervalS || r.IntervalS > MaxIntervalS {
		return invalid("interval_s", "必须在 [%d, %d] 秒之间，收到 %d", MinIntervalS, MaxIntervalS, r.IntervalS)
	}

	if len(r.Load) != 3 {
		return invalid("load", "必须恰好 3 个元素（1/5/15 分钟负载），收到 %d 个", len(r.Load))
	}
	for i, v := range r.Load {
		if !finite(v) || v < 0 {
			return invalid(fmt.Sprintf("load[%d]", i), "必须是有限的非负数，收到 %v", v)
		}
	}

	if !finite(r.CPUPct) || r.CPUPct < 0 || r.CPUPct > 100 {
		return invalid("cpu_pct", "必须是 [0, 100] 之间的有限数，收到 %v", r.CPUPct)
	}

	if r.Mem.Total == 0 {
		return invalid("mem.total", "不能为 0")
	}
	if r.Mem.Used > r.Mem.Total {
		return invalid("mem.used", "不能大于 mem.total（%d > %d）", r.Mem.Used, r.Mem.Total)
	}

	if len(r.Disk) == 0 {
		return invalid("disk", "至少需要一个挂载点")
	}
	if len(r.Disk) > MaxDiskEntries {
		return invalid("disk", "挂载点数量不能超过 %d，收到 %d", MaxDiskEntries, len(r.Disk))
	}
	seen := make(map[string]struct{}, len(r.Disk))
	for i, d := range r.Disk {
		field := fmt.Sprintf("disk[%d]", i)
		switch {
		case d.Mount == "":
			return invalid(field+".mount", "不能为空")
		case len(d.Mount) > MaxMountLen:
			return invalid(field+".mount", "长度不能超过 %d，收到 %d", MaxMountLen, len(d.Mount))
		}
		if _, dup := seen[d.Mount]; dup {
			return invalid(field+".mount", "挂载点重复：%q", d.Mount)
		}
		seen[d.Mount] = struct{}{}
		if d.Total == 0 {
			return invalid(field+".total", "不能为 0")
		}
		if d.Used > d.Total {
			return invalid(field+".used", "不能大于 total（%d > %d）", d.Used, d.Total)
		}
	}

	// net 整体可缺省（老 agent 不发），但只要出现就必须四个字段齐全——
	// 不能出现"有 rx 没 tx"这种半份数据。
	if n := r.Net; n != nil {
		switch {
		case n.RxBps == nil:
			return invalid("net.rx", "net 出现时不能缺省")
		case n.TxBps == nil:
			return invalid("net.tx", "net 出现时不能缺省")
		case n.RxTotal == nil:
			return invalid("net.rx_total", "net 出现时不能缺省")
		case n.TxTotal == nil:
			return invalid("net.tx_total", "net 出现时不能缺省")
		}
		if !finite(*n.RxBps) || *n.RxBps < 0 {
			return invalid("net.rx", "必须是有限的非负数（字节/秒），收到 %v", *n.RxBps)
		}
		if !finite(*n.TxBps) || *n.TxBps < 0 {
			return invalid("net.tx", "必须是有限的非负数（字节/秒），收到 %v", *n.TxBps)
		}
	}

	if r.UptimeS < 0 {
		return invalid("uptime_s", "不能为负数，收到 %d", r.UptimeS)
	}
	if r.AgentVersion == "" {
		return invalid("agent_version", "不能为空")
	}
	if len(r.AgentVersion) > MaxAgentVersionLen {
		return invalid("agent_version", "长度不能超过 %d，收到 %d", MaxAgentVersionLen, len(r.AgentVersion))
	}
	return nil
}

// ValidateNodeID 校验节点标识。
//
// 导出它是为了让 server 配置解析与上报校验共用同一份规则——
// 配置里能写出来的 id，一定要能通过上报校验，否则那个节点永远上不了线。
func ValidateNodeID(s string) error {
	return validateNodeID(s)
}

// validateNodeID 限制节点标识为 slug 字符集，避免它被用作路径或日志注入的载体。
func validateNodeID(s string) error {
	if s == "" {
		return invalid("node", "不能为空")
	}
	if len(s) > MaxNodeIDLen {
		return invalid("node", "长度不能超过 %d，收到 %d", MaxNodeIDLen, len(s))
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return invalid("node", "只允许字母、数字、点、下划线与连字符，收到 %q", c)
		}
	}
	return nil
}

// finite 判断是否为有限数。
//
// JSON 本身写不出 NaN/Inf，但 agent 是先在内存里构造 struct 再调用 Validate 的，
// 而 0/0 这类除零完全可能算出 NaN——所以这项检查对 agent 侧自检是必需的，
// 不是多余的防御。
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
