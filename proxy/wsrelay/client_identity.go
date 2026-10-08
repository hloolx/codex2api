package wsrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

func websocketClientIdentity(headers http.Header) string {
	parts := []string{headers.Get("User-Agent"), headers.Get("Version"), headers.Get("Originator")}
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

// 握手头固定到连接；版本更新后须使用对应身份的连接池。
func websocketClientPoolKey(key string, headers http.Header) string {
	if key == "" {
		return key
	}
	return key + "|client:" + websocketClientIdentity(headers)
}

type websocketContinuation struct {
	responseID string
	accountID  int64
	apiKey     string
	identity   string
}

func (e *Executor) acquireClientContinuation(input websocketContinuation) (*WsConnection, *PendingRequest, string) {
	connection, pending, key := e.manager.AcquirePreferredConnection(input.responseID, input.accountID, input.apiKey)
	if connection == nil || connection.upstreamClientIdentity == input.identity {
		return connection, pending, key
	}
	if !connection.cancelUnsentReadLease(pending.RequestID) {
		e.manager.DiscardConnection(connection)
	}
	connection.session.RemovePendingRequest(pending.RequestID)
	return nil, nil, ""
}
