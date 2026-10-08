package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/egressipv6"
)

const localIPv6RoutePrefix = "local-ipv6://"

// These internal route tokens never pass through public proxy configuration.
func IPv6RouteIP(route string) string { return strings.TrimPrefix(route, localIPv6RoutePrefix) }
func IsIPv6Route(route string) bool   { return strings.HasPrefix(route, localIPv6RoutePrefix) }
func NativeIPv6Account(account *auth.Account) bool {
	return account != nil && !account.IsRelayStyle() && !resinCarriesEgress(account)
}

func wrapIPv6Client(account *auth.Account, base *http.Client, replay, forceUTLS bool) *http.Client {
	m := egressipv6.Current()
	if m == nil || !NativeIPv6Account(account) {
		return base
	}
	return egressipv6.WrapClient(m, account.ID(), base, func(ip string) http.RoundTripper { return sourceIPv6Transport(account.ID(), ip, forceUTLS) }, replay)
}

func sourceIPv6Transport(id int64, ip string, forceUTLS bool) http.RoundTripper {
	mode := codexTransportModeFromEnv()
	if forceUTLS {
		mode = codexTransportModeUTLSChrome
	}
	key := fmt.Sprintf("ipv6|%d|%s|%s", id, ip, mode)
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client.Transport
	}
	dialer, err := egressipv6.NewDialer(ip)
	if err != nil {
		return errorTransport{err}
	}
	var rt http.RoundTripper
	if mode == codexTransportModeUTLSChrome {
		rt = &utlsRoundTripper{connections: make(map[string]*utlsConn), pending: make(map[string]*sync.Cond), dialer: dialer}
	} else {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		tr.DialContext = dialer.DialContext
		tr.ResponseHeaderTimeout = 0
		tr.TLSHandshakeTimeout = 0
		tr.MaxIdleConnsPerHost = 4
		tr.IdleConnTimeout = 90 * time.Second
		rt = tr
	}
	entry := &poolEntry{client: &http.Client{Transport: rt}, createdAt: time.Now().UnixNano()}
	entry.touch()
	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		releaseEvictedClient(entry.client)
		e := v.(*poolEntry)
		e.touch()
		return e.client.Transport
	}
	return rt
}

type errorTransport struct{ err error }

func (e errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

// OAuth refresh must never replay a potentially consumed refresh token.
func WrapOAuthIPv6Client(id int64, base *http.Client, forceUTLS bool) *http.Client {
	if m := egressipv6.Current(); m != nil && !IsResinEnabled() {
		return egressipv6.WrapClient(m, id, base, func(ip string) http.RoundTripper { return sourceIPv6Transport(id, ip, forceUTLS) }, false)
	}
	return base
}

func resolveIPv6Websocket(ctx context.Context, account *auth.Account, egress CodexEgress) CodexEgress {
	m := egressipv6.Current()
	if m == nil || !NativeIPv6Account(account) || egress.ProxyURL == ipv6StateDirectRoute {
		return egress
	}
	route, err := m.Acquire(ctx, account.ID())
	if err != nil {
		egress.Err = err
		return egress
	}
	if route.Config.Enabled {
		egress.Kind = "local_ipv6"
		egress.DialProxyURL = localIPv6RoutePrefix + route.Binding.IP
		egress.IPv6 = route
	}
	return egress
}
