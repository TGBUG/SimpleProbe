package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DecodeStrict 以“任何一处不合规都拒绝”的方式解析上报载荷。
//
// 与 json.Unmarshal 的区别：
//  1. 请求体超过 MaxBodyBytes 直接报错，而不是静默截断；
//  2. 出现未知字段即报错（DisallowUnknownFields）；
//  3. JSON 文档结束后还有尾随内容即报错；
//  4. 解析完成后自动调用 Validate，调用方不必记得再校验一次。
func DecodeStrict(r io.Reader) (*Report, error) {
	// 多读一个字节，才能区分“恰好等于上限”与“超过上限”。
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, &ValidationError{Code: CodeMalformed, Msg: "读取请求体失败"}
	}
	if len(body) > MaxBodyBytes {
		return nil, &ValidationError{
			Code: CodeBodyTooLarge,
			Msg:  fmt.Sprintf("请求体超过 %d 字节上限", MaxBodyBytes),
		}
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var rep Report
	if err := dec.Decode(&rep); err != nil {
		return nil, decodeErr(err)
	}
	// 第二次 Decode 必须立刻返回 io.EOF；否则说明 JSON 文档后面还有内容。
	// 尾随空白不算违规（Decode 会返回 io.EOF）。
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &ValidationError{Code: CodeTrailingData, Msg: "JSON 文档之后存在多余内容"}
	}

	if err := rep.Validate(); err != nil {
		return nil, err
	}
	return &rep, nil
}

func decodeErr(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &ValidationError{Code: CodeInvalidType, Field: typeErr.Field, Msg: "字段类型不符"}
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return &ValidationError{
			Code: CodeMalformed,
			Msg:  fmt.Sprintf("JSON 语法错误（偏移 %d）", syntaxErr.Offset),
		}
	}
	// DisallowUnknownFields 返回的是普通 error，没有专门的类型可 errors.As，
	// 只能按文本识别。这条分支由测试用例固定住。
	//
	// 只把字段名单独取出来放进 Field，不把整条错误文本当成给客户端的描述——
	// 那段文本里含有对方提供的字段名。
	if strings.Contains(err.Error(), "unknown field") {
		return &ValidationError{
			Code:  CodeUnknownField,
			Field: unknownFieldName(err.Error()),
			Msg:   "存在未声明的字段",
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return &ValidationError{Code: CodeMalformed, Msg: "JSON 不完整"}
	}
	return &ValidationError{Code: CodeMalformed, Msg: "解析失败"}
}

// unknownFieldName 从 `json: unknown field "name"` 里取出 name。
func unknownFieldName(msg string) string {
	_, rest, ok := strings.Cut(msg, `"`)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}
