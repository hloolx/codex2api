package wsrelay

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/egressipv6"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

type ipv6WSAttempt struct {
	ctx       context.Context
	manager   *egressipv6.Manager
	accountID int64
	route     egressipv6.Route
	attempts  int
	execute   func(string) (*WsResponse, error)
}

func (e *Executor) ExecuteRequestViaWebsocket(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*WsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m := egressipv6.Current()
	if m == nil || !proxy.NativeIPv6Account(account) {
		return e.executeRequestViaWebsocket(ctx, account, body, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
	}
	route, err := m.Acquire(ctx, account.ID())
	if err != nil {
		return nil, err
	}
	if !route.Config.Enabled {
		return e.executeRequestViaWebsocket(ctx, account, body, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
	}
	a := &ipv6WSAttempt{ctx: ctx, manager: m, accountID: account.ID(), route: route}
	a.execute = func(ip string) (*WsResponse, error) {
		return e.executeRequestViaWebsocket(ctx, account, body, sessionID, "local-ipv6://"+ip, apiKey, deviceCfg, headers, poolRouteKey)
	}
	return a.run()
}
func (a *ipv6WSAttempt) run() (*WsResponse, error) {
	for {
		a.attempts++
		if a.manager.ObserveSource != nil {
			a.manager.ObserveSource(a.ctx, a.accountID, a.route.Binding.IP)
		}
		r, err := a.execute(a.route.Binding.IP)
		if err == nil {
			r.ipv6Attempt = a
			return r, nil
		}
		reason := ""
		var hs *HandshakeHTTPError
		var op *net.OpError
		if errors.As(err, &hs) {
			reason = egressipv6.FailureReason(hs.StatusCode, []byte(hs.Body), a.route.Config.Retry5xx)
		} else if errors.As(err, &op) && op.Op == "dial" {
			reason = "connect_failed"
		}
		if !a.rotate(reason) {
			return nil, err
		}
	}
}
func (a *ipv6WSAttempt) rotate(reason string) bool {
	if reason == "" || a.ctx.Err() != nil {
		return false
	}
	next, err := a.manager.Rotate(a.ctx, a.accountID, a.route.Binding.IP, reason)
	if err != nil || !next.Config.Enabled || next.Binding.IP == a.route.Binding.IP || a.attempts >= a.route.Config.MaxAttempts {
		return false
	}
	a.route = next
	return true
}

// Buffer only creation metadata. Once any output is forwarded the request is
// never replayed, including tool calls and reasoning deltas.
func (r *WsResponse) ReadStream(callback func([]byte) bool) error {
	a := r.ipv6Attempt
	if a == nil {
		return r.readStream(callback)
	}
	buffered := [][]byte{}
	size := 0
	committed := false
	reason := ""
	retry := false
	err := r.readStream(func(payload []byte) bool {
		reason = egressipv6.FrameFailure(payload, a.route.Config.Retry5xx)
		if reason != "" {
			rotated := a.rotate(reason)
			if !committed && rotated && gjson.GetBytes(payload, "response.usage.total_tokens").Int() == 0 && gjson.GetBytes(payload, "response.usage.output_tokens").Int() == 0 && gjson.GetBytes(payload, "response.usage.input_tokens").Int() == 0 {
				retry = true
				return false
			}
		}
		typ := gjson.GetBytes(payload, "type").String()
		if !committed && (typ == "response.created" || typ == "response.in_progress") && size+len(payload) <= 64*1024 {
			buffered = append(buffered, append([]byte(nil), payload...))
			size += len(payload)
			return true
		}
		committed = true
		for _, p := range buffered {
			if !callback(p) {
				return false
			}
		}
		buffered = nil
		return callback(payload)
	})
	if retry {
		r.markConnBroken()
		r.Close()
		replacement, retryErr := a.run()
		if retryErr != nil {
			return retryErr
		}
		r.mu.Lock()
		r.ipv6Replacement = replacement
		r.mu.Unlock()
		defer replacement.Close()
		return replacement.ReadStream(callback)
	}
	if err != nil && a.ctx.Err() == nil {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if !closed {
			_, _ = a.manager.Rotate(a.ctx, a.accountID, a.route.Binding.IP, "stream_disconnected")
		}
	}
	return err
}
