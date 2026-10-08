package egressipv6

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type transport struct {
	manager   *Manager
	accountID int64
	fallback  http.RoundTripper
	source    func(string) http.RoundTripper
	replay    bool
}

func WrapClient(m *Manager, id int64, base *http.Client, source func(string) http.RoundTripper, replay bool) *http.Client {
	c := *base
	fallback := base.Transport
	if fallback == nil {
		fallback = http.DefaultTransport
	}
	c.Transport = &transport{m, id, fallback, source, replay}
	// Credential-bearing direct requests must not follow redirects to a new host.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if m.enabled.Load() {
			return http.ErrUseLastResponse
		}
		if base.CheckRedirect != nil {
			return base.CheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	route, err := t.manager.Acquire(req.Context(), t.accountID)
	if err != nil {
		return nil, err
	}
	if !route.Config.Enabled {
		return t.fallback.RoundTrip(req)
	}
	attemptReq := req
	for attempt := 1; ; attempt++ {
		if t.manager.ObserveSource != nil {
			t.manager.ObserveSource(req.Context(), t.accountID, route.Binding.IP)
		}
		resp, requestErr := t.source(route.Binding.IP).RoundTrip(attemptReq)
		reason := ""
		safeReplay := true
		if requestErr != nil {
			var op *net.OpError
			if !errors.Is(requestErr, context.Canceled) && !errors.Is(requestErr, context.DeadlineExceeded) && errors.As(requestErr, &op) && op.Op == "dial" {
				reason = "connect_failed"
			}
		}
		if resp != nil && resp.StatusCode >= 400 {
			// Bound inspection and restore the original bytes for the caller.
			prefix, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
			resp.Body = &prefixBody{Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body), Closer: resp.Body}
			if readErr == nil && len(prefix) <= 64*1024 {
				reason = FailureReason(resp.StatusCode, prefix, route.Config.Retry5xx)
			}
		}
		if resp != nil && resp.StatusCode == 200 && t.replay && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			reason, safeReplay = inspectSSEPrefix(resp, route.Config.Retry5xx)
			if reason == "" {
				sourceIP := route.Binding.IP
				resp.Body = &observedSSEBody{ReadCloser: resp.Body, retry5xx: route.Config.Retry5xx, onFailure: func(reason string) {
					if req.Context().Err() == nil {
						_, _ = t.manager.Rotate(req.Context(), t.accountID, sourceIP, reason)
					}
				}}
			}
		}
		if reason == "" || req.Context().Err() != nil {
			return resp, requestErr
		}
		next, rotateErr := t.manager.Rotate(req.Context(), t.accountID, route.Binding.IP, reason)
		if rotateErr != nil || !next.Config.Enabled || !t.replay || !safeReplay || attempt >= route.Config.MaxAttempts || next.Binding.IP == route.Binding.IP {
			return resp, requestErr
		}
		if req.Body != nil && req.GetBody == nil {
			return resp, requestErr
		}
		retryReq := req.Clone(req.Context())
		if req.Body != nil {
			retryReq.Body, err = req.GetBody()
			if err != nil {
				return resp, requestErr
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
		route = next
		attemptReq = retryReq
	}
}

// Do not consume past the first output event. The complete inspected prefix is
// restored even on malformed data, EOF, or a terminal failure that is not retried.
func inspectSSEPrefix(resp *http.Response, retry5xx bool) (string, bool) {
	const limit = 64 * 1024
	original := resp.Body
	reader := bufio.NewReader(io.LimitReader(original, limit))
	var prefix bytes.Buffer
	defer func() {
		resp.Body = &prefixBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader, original), Closer: original}
	}()
	var data []byte
	for prefix.Len() < limit {
		line, err := reader.ReadBytes('\n')
		prefix.Write(line)
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))...)
		}
		if len(trimmed) == 0 && len(data) > 0 {
			typ := gjson.GetBytes(data, "type").String()
			if reason := FrameFailure(data, retry5xx); reason != "" {
				usage := gjson.GetBytes(data, "response.usage")
				return reason, usage.Get("total_tokens").Int() == 0 && usage.Get("output_tokens").Int() == 0 && usage.Get("input_tokens").Int() == 0
			}
			if typ != "response.created" && typ != "response.in_progress" {
				return "", false
			}
			data = nil
		}
		if err != nil {
			return "", false
		}
	}
	return "", false
}

type prefixBody struct {
	io.Reader
	io.Closer
}
