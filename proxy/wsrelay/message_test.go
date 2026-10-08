package wsrelay

import (
	"testing"
)

// TestMessageCreation 测试消息创建
func TestMessageCreation(t *testing.T) {
	requestID := "test-request-123"
	sessionID := "test-session-456"
	content := []byte(`{"test":"data"}`)

	msg := NewHTTPRequestMessage(requestID, sessionID, content)

	if msg.Type != MessageTypeHTTPRequest {
		t.Errorf("expected type %s, got %s", MessageTypeHTTPRequest, msg.Type)
	}
	if msg.RequestID != requestID {
		t.Errorf("expected requestID %s, got %s", requestID, msg.RequestID)
	}
	if msg.SessionID != sessionID {
		t.Errorf("expected sessionID %s, got %s", sessionID, msg.SessionID)
	}
	if string(msg.Content) != string(content) {
		t.Errorf("expected content %s, got %s", string(content), string(msg.Content))
	}
}

// TestErrorMessage 测试错误消息创建
func TestErrorMessage(t *testing.T) {
	requestID := "test-request-123"
	statusCode := 503
	message := "service unavailable"

	msg := NewErrorMessage(requestID, statusCode, message)

	if msg.Type != MessageTypeError {
		t.Errorf("expected type %s, got %s", MessageTypeError, msg.Type)
	}
	if msg.RequestID != requestID {
		t.Errorf("expected requestID %s, got %s", requestID, msg.RequestID)
	}
	if msg.StatusCode != statusCode {
		t.Errorf("expected statusCode %d, got %d", statusCode, msg.StatusCode)
	}
	if msg.Error != message {
		t.Errorf("expected error %s, got %s", message, msg.Error)
	}
}
