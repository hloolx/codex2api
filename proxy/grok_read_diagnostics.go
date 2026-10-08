package proxy

import (
	"context"
	"crypto/tls"
	"log"
	"net/http/httptrace"
	"sync"
	"time"
)

// Read-only maintenance requests share the existing transport and proxy.
// This adds a total deadline, never an OAuth/token-rotation retry.
func grokReadContext(parent context.Context, accountID int64, endpoint string) (context.Context, func(int)) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	started := time.Now()
	var mu sync.Mutex
	var connectStart, tlsStart time.Time
	var connect, handshake, firstByte time.Duration
	var reused bool
	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) { mu.Lock(); connectStart = time.Now(); mu.Unlock() },
		ConnectDone: func(_, _ string, _ error) {
			mu.Lock()
			if !connectStart.IsZero() {
				connect += time.Since(connectStart)
			}
			mu.Unlock()
		},
		TLSHandshakeStart: func() { mu.Lock(); tlsStart = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			mu.Lock()
			if !tlsStart.IsZero() {
				handshake += time.Since(tlsStart)
			}
			mu.Unlock()
		},
		GotConn:              func(info httptrace.GotConnInfo) { mu.Lock(); reused = info.Reused; mu.Unlock() },
		GotFirstResponseByte: func() { mu.Lock(); firstByte = time.Since(started); mu.Unlock() },
	}
	return httptrace.WithClientTrace(ctx, trace), func(status int) {
		elapsed := time.Since(started)
		mu.Lock()
		// No URL, headers, response payload or transport errors (which may contain credentials).
		log.Printf("[grok-read] account=%d endpoint=%s status=%d elapsed_ms=%d connect_ms=%d tls_ms=%d first_byte_ms=%d reused=%t timeout=%t",
			accountID, endpoint, status, elapsed.Milliseconds(), connect.Milliseconds(), handshake.Milliseconds(), firstByte.Milliseconds(), reused, ctx.Err() == context.DeadlineExceeded)
		mu.Unlock()
		cancel()
	}
}
