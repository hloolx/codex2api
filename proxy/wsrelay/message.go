package wsrelay

import (
	"time"
)

// ==================== WebSocket 消息帧格式 ====================

// MessageType WebSocket 消息类型常量
type MessageType string

const (
	// HTTP 相关消息类型
	MessageTypeHTTPRequest  MessageType = "http_request"  // HTTP 请求
	MessageTypeHTTPResponse MessageType = "http_response" // HTTP 响应（非流式）

	// 流式消息类型
	MessageTypeStreamStart MessageType = "stream_start" // 流开始
	MessageTypeStreamChunk MessageType = "stream_chunk" // 流数据块
	MessageTypeStreamEnd   MessageType = "stream_end"   // 流结束

	// 错误消息类型
	MessageTypeError MessageType = "error" // 错误

	// 控制消息类型
	MessageTypePing MessageType = "ping" // 心跳 Ping
	MessageTypePong MessageType = "pong" // 心跳 Pong
)

// Message WebSocket 消息帧结构
type Message struct {
	// 消息类型
	Type MessageType `json:"type"`

	// 请求 ID（用于匹配请求和响应）
	RequestID string `json:"request_id,omitempty"`

	// 会话 ID（用于 prompt cache 绑定）
	SessionID string `json:"session_id,omitempty"`

	// HTTP 状态码（仅用于 http_response 和 error）
	StatusCode int `json:"status_code,omitempty"`

	// 消息内容（JSON 格式的请求体或响应体）
	Content []byte `json:"content,omitempty"`

	// 错误信息（仅用于 error 类型）
	Error string `json:"error,omitempty"`

	// 时间戳（Unix 毫秒）
	Timestamp int64 `json:"timestamp,omitempty"`
}

// NewHTTPRequestMessage 创建 HTTP 请求消息
func NewHTTPRequestMessage(requestID, sessionID string, content []byte) *Message {
	return &Message{
		Type:      MessageTypeHTTPRequest,
		RequestID: requestID,
		SessionID: sessionID,
		Content:   content,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewHTTPResponseMessage 创建 HTTP 响应消息（非流式）
func NewHTTPResponseMessage(requestID string, statusCode int, content []byte) *Message {
	return &Message{
		Type:       MessageTypeHTTPResponse,
		RequestID:  requestID,
		StatusCode: statusCode,
		Content:    content,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewStreamStartMessage 创建流开始消息
func NewStreamStartMessage(requestID string, statusCode int) *Message {
	return &Message{
		Type:       MessageTypeStreamStart,
		RequestID:  requestID,
		StatusCode: statusCode,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewStreamChunkMessage 创建流数据块消息
func NewStreamChunkMessage(requestID string, content []byte) *Message {
	return &Message{
		Type:      MessageTypeStreamChunk,
		RequestID: requestID,
		Content:   content,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewStreamEndMessage 创建流结束消息
func NewStreamEndMessage(requestID string) *Message {
	return &Message{
		Type:      MessageTypeStreamEnd,
		RequestID: requestID,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewErrorMessage 创建错误消息
func NewErrorMessage(requestID string, statusCode int, errMsg string) *Message {
	return &Message{
		Type:       MessageTypeError,
		RequestID:  requestID,
		StatusCode: statusCode,
		Error:      errMsg,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewPingMessage 创建 Ping 心跳消息
func NewPingMessage() *Message {
	return &Message{
		Type:      MessageTypePing,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewPongMessage 创建 Pong 心跳响应消息
func NewPongMessage() *Message {
	return &Message{
		Type:      MessageTypePong,
		Timestamp: time.Now().UnixMilli(),
	}
}
