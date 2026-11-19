package mcp

import (
	"encoding/json"
	"fmt"
)

// ===========================================================================
// JSON-RPC 2.0 的手写实现（不引入 MCP SDK，D5/D7 的依赖边界）
//
// # 为什么手写
//
// 协议本身很小：{jsonrpc,id,method,params} 与 {jsonrpc,id,result|error}。
// 引入 SDK 会把"零第三方依赖"（internal/vtest/no_network_test.go 的
// TestDependencyAllowlist）直接打破，而且 SDK 往往自带 HTTP transport，
// 与 D7「不自己发请求」冲突。
//
// # 三条容易做错的地方（本文件就是为此存在）
//
//  1. **参数错误必须是 error 对象**，不能 panic、不能返回 result：
//     code=-32602 invalid params。panic 在 stdio 服务里等于整个进程挂掉，
//     客户端只会看到"连接断了"，完全无从判断是哪个参数错了。
//  2. **id 可能是字符串也可能是数字**：用 json.RawMessage 原样回写，
//     不要自己转成 int（转错会让客户端匹配不上响应）。
//  3. **通知（无 id）不得回响应**：回了就是协议违规，某些客户端会直接断开。
// ===========================================================================

// JSON-RPC 2.0 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// RPCError 是 JSON-RPC 的 error 对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data 是可选的结构化补充（例如出错的字段名）。
	Data json.RawMessage `json:"data,omitempty"`
}

// Error 实现 error。
func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// invalidParams 构造参数错误（-32602）。
func invalidParams(format string, args ...any) *RPCError {
	return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// internalError 构造内部错误（-32603）。
func internalError(format string, args ...any) *RPCError {
	return &RPCError{Code: CodeInternalError, Message: fmt.Sprintf(format, args...)}
}

// methodNotFound 构造"方法不存在"（-32601）。
func methodNotFound(method string) *RPCError {
	return &RPCError{Code: CodeMethodNotFound, Message: "未知方法 " + method}
}

// Request 是一条入站报文。ID 为 nil 表示通知。
//
// 同一个结构也用于承接**入站响应**（服务端发出的 sampling 请求的回包）：
// 那时 Method 为空、ID 有值、Result/Error 有值。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Result / Error 只在"客户端回包"时出现。
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// IsNotification 报告是否为通知（无 id）。
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// IsResponse 报告是否为"对服务端请求的响应"（无 method、有 id）。
func (r Request) IsResponse() bool { return r.Method == "" && len(r.ID) > 0 }

// Response 是一条出站报文。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// newResult 构造成功响应。
func newResult(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

// newError 构造错误响应。
func newError(id json.RawMessage, err *RPCError) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: err}
}

// ToolResult 是 tools/call 的返回体（MCP 的 content 块形态）。
//
// 同时给出 content（人/模型都读得懂的 JSON 文本）与 structuredContent
// （便于程序化消费）：只给前者会让调用方被迫二次解析文本，
// 只给后者则不符合 spec 的最低要求。
type ToolResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ContentBlock 是一个内容块（本服务只用 text）。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textResult 把任意结果包成 ToolResult。
//
// 序列化失败不返回错误：tools/call 的结果本身已经是"结果"，再因为序列化
// 失败而变成 -32603 只会让调用方困惑。此时退化为一段说明文本。
func textResult(v any) ToolResult {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ToolResult{
			Content: []ContentBlock{{Type: "text", Text: "结果序列化失败: " + err.Error()}},
			IsError: true,
		}
	}
	return ToolResult{
		Content:           []ContentBlock{{Type: "text", Text: string(body)}},
		StructuredContent: v,
	}
}

// errorResult 把业务错误包成 ToolResult（isError=true）。
//
// # 为什么业务错误不走 JSON-RPC error
//
// 协议语义：JSON-RPC error 表示"请求本身没被执行"（方法不存在、参数非法）；
// 而"这条记忆不存在""这个边不可裁决"是**工具执行结果**，模型需要看到它
// 才能自我修正。因此它们走 isError=true 的 ToolResult。
// 参数校验失败仍走 -32602（那是请求层面的错）。
func errorResult(err error) ToolResult {
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}
