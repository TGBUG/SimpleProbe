package protocol

import (
	"errors"
	"testing"
)

// TestDecodeErrFallback 覆盖 decodeErr 的兜底分支：
// 当错误既不是类型错误、也不是语法错误、也不是未知字段时，
// 必须仍然归类为 malformed_json——绝不能漏成 nil 或 panic。
func TestDecodeErrFallback(t *testing.T) {
	err := decodeErr(errors.New("某种没见过的解析错误"))

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("期望 *ValidationError，得到 %T", err)
	}
	if ve.Code != CodeMalformed {
		t.Errorf("错误码 = %q，期望 %q", ve.Code, CodeMalformed)
	}
}
