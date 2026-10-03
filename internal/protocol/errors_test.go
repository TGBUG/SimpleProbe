package protocol_test

import (
	"errors"
	"testing"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// TestValidationErrorError 锁住错误文案的格式。
// 这条同时是一个约束：文案里只允许出现错误码、字段名与短描述，
// 不能把载荷内容拼进去（避免日志注入与敏感数据回显）。
func TestValidationErrorError(t *testing.T) {
	withField := &protocol.ValidationError{
		Code:  protocol.CodeInvalidField,
		Field: "cpu_pct",
		Msg:   "必须是 [0, 100] 之间的有限数",
	}
	if got, want := withField.Error(), "invalid_field: cpu_pct: 必须是 [0, 100] 之间的有限数"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}

	noField := &protocol.ValidationError{Code: protocol.CodeMalformed, Msg: "JSON 不完整"}
	if got, want := noField.Error(), "malformed_json: JSON 不完整"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestDecodeStrictReaderFailure 覆盖“请求体读不出来”这条路径：
// 它必须变成 malformed_json，而不是返回 nil 错误或半个载荷。
func TestDecodeStrictReaderFailure(t *testing.T) {
	rep, err := protocol.DecodeStrict(failingReader{})
	if rep != nil {
		t.Errorf("失败时不应返回载荷，得到 %+v", rep)
	}
	var ve *protocol.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("期望 *ValidationError，得到 %v", err)
	}
	if ve.Code != protocol.CodeMalformed {
		t.Errorf("错误码 = %q，期望 %q", ve.Code, protocol.CodeMalformed)
	}
}
