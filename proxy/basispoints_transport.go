package proxy

import (
	"net"
	"net/http"
	"time"

	"github.com/codex2api/auth"
)

const basispointsTransportMode = "basispoints"

// Basispoints has a separate pool without Codex's HTTP/2 PING deadlines.
// A quiet reasoning stream must remain open until the caller cancels it.
func getBasispointsClient(account *auth.Account, proxyURL string) (*http.Client, error) {
	key := clientPoolKey(account, proxyURL, basispointsTransportMode)
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return wrapIPv6Client(account, entry.client, true, false), nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 0
	transport.TLSHandshakeTimeout = 0
	transport.MaxIdleConnsPerHost = 4
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, err
	}
	entry := &poolEntry{
		client:    &http.Client{Transport: transport},
		createdAt: time.Now().UnixNano(), rotatable: true,
	}
	entry.touch()
	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		transport.CloseIdleConnections()
		entry = v.(*poolEntry)
		entry.touch()
	}
	return wrapIPv6Client(account, entry.client, true, false), nil
}

func recycleBasispointsClient(account *auth.Account, proxyURL string) {
	if v, ok := clientPool.LoadAndDelete(clientPoolKey(account, proxyURL, basispointsTransportMode)); ok {
		releaseEvictedClient(v.(*poolEntry).client)
	}
}
