package protocol

// ErrorResponse 是 server 返回错误时的统一信封，由 agent 与 server 共享。
//
// 刻意不回显 payload：只给机器可读的 code、出错的字段名与一句短描述。
// 描述由 server 自己构造，不含请求原文（未知字段名这种“由对方提供”的内容
// 会在写出前被消毒与截断，见 api.sanitize）。规格 §4.7。
type ErrorResponse struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}
