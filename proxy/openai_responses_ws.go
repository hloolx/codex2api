package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAIResponsesWebsocketBetaHeader = "responses_websockets=2026-02-06"
	openAIResponsesWSMaxConns          = 8
	openAIResponsesWSMaxAge            = 30 * time.Minute
)

// openAIResponsesRelayUsesUpstreamWebsocket 判断这一次出站是否拨中转账号自己的
// Responses WebSocket。生图仍走 HTTP。全局 codex_force_websocket 不参与这个判断。
func openAIResponsesRelayUsesUpstreamWebsocket(account *auth.Account, body []byte) bool {
	if account == nil || !account.OpenAIResponsesUsesUpstreamWebsocket() {
		return false
	}
	return !rawResponsesBodyShouldForceHTTPForImageGeneration(body)
}

func openAIResponsesWebsocketURL(baseURL string) (string, error) {
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("OpenAI Responses WebSocket 地址无效")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("OpenAI Responses WebSocket 仅支持 http/https Base URL")
	}
	return parsed.String(), nil
}

func prepareOpenAIResponsesWebsocketBody(body []byte) []byte {
	if len(body) == 0 {
		return []byte(`{"type":"response.create","stream":true}`)
	}
	wsBody := stripResponsesImageGenerationTool(bytes.Clone(body))
	wsBody, err := sjson.SetBytes(wsBody, "type", "response.create")
	if err != nil {
		return body
	}
	wsBody, err = sjson.SetBytes(wsBody, "stream", true)
	if err != nil {
		return body
	}
	return wsBody
}

type openAIResponsesWSHeaderInput struct {
	ctx        context.Context
	account    *auth.Account
	apiKey     string
	endpoint   string
	downstream http.Header
}

func openAIResponsesWebsocketHeadersChecked(input openAIResponsesWSHeaderInput) (http.Header, error) {
	if input.ctx == nil {
		input.ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(input.ctx, http.MethodGet, input.endpoint, nil)
	if err != nil {
		req, _ = http.NewRequestWithContext(input.ctx, http.MethodGet, "https://api.openai.com/v1/responses", nil)
	}
	if err := applyOpenAIResponsesRequestHeaders(req, input.account, input.apiKey, input.downstream); err != nil {
		return nil, err
	}
	headers := req.Header.Clone()
	for name := range headers {
		if openAIResponsesWSHopHeader(name) {
			headers.Del(name)
		}
	}
	// 握手鉴权固定用账号 API Key。自定义头不能把它换成别的凭据，也不能拿掉 beta 头。
	headers.Set("Authorization", "Bearer "+input.apiKey)
	headers.Set("OpenAI-Beta", openAIResponsesWebsocketBetaHeader)
	return headers, nil
}

func openAIResponsesWSHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "upgrade", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "host", "content-length", "content-type", "accept", "accept-encoding":
		return true
	default:
		return false
	}
}

func executeOpenAIResponsesWebsocket(ctx context.Context, account *auth.Account, requestBody []byte, proxyURL string, downstream http.Header, baseURL, apiKey string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(requestBody, "model").String()); err != nil {
		return nil, err
	}
	wsURL, err := openAIResponsesWebsocketURL(baseURL)
	if err != nil {
		return nil, ErrUpstream(0, "OpenAI Responses WebSocket 地址无效", err)
	}
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
	headers, err := openAIResponsesWebsocketHeadersChecked(openAIResponsesWSHeaderInput{ctx: ctx, account: account, apiKey: apiKey, endpoint: endpoint, downstream: downstream})
	if err != nil {
		return nil, err
	}
	body := prepareOpenAIResponsesWebsocketBody(requestBody)
	record := beginUpstreamTrace(ctx, account, proxyURL, true)
	resp, err := openAIResponsesWebsocketPool.roundTrip(ctx, openAIResponsesWSSpec{
		accountID: account.ID(),
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		keyHash:   openAIResponsesWSKeyHash(apiKey),
		proxyURL:  strings.TrimSpace(proxyURL),
		wsURL:     wsURL,
		headers:   headers,
	}, body)
	record(resp)
	return resp, err
}

func openAIResponsesWSKeyHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return fmt.Sprintf("%x", sum[:8])
}

type openAIResponsesWSSpec struct {
	accountID int64
	baseURL   string
	keyHash   string
	proxyURL  string
	wsURL     string
	headers   http.Header
}

func (s openAIResponsesWSSpec) id() string {
	return fmt.Sprintf("%d\n%s\n%s\n%s", s.accountID, s.baseURL, s.keyHash, s.proxyURL)
}

type openAIResponsesWSConn struct {
	spec     openAIResponsesWSSpec
	conn     *websocket.Conn
	created  time.Time
	reused   bool
	busy     bool
	dead     bool
	waiters  []chan struct{}
	response map[string]struct{}
}

type openAIResponsesWSPool struct {
	mu       sync.Mutex
	conns    []*openAIResponsesWSConn
	dialing  map[string]int
	bindings map[string]*openAIResponsesWSConn
	idleWait map[string][]chan struct{}
}

var openAIResponsesWebsocketPool = newOpenAIResponsesWSPool()

func resetOpenAIResponsesWebsocketPool() {
	openAIResponsesWebsocketPool.mu.Lock()
	conns := openAIResponsesWebsocketPool.conns
	openAIResponsesWebsocketPool.conns = nil
	openAIResponsesWebsocketPool.dialing = map[string]int{}
	openAIResponsesWebsocketPool.bindings = map[string]*openAIResponsesWSConn{}
	openAIResponsesWebsocketPool.idleWait = map[string][]chan struct{}{}
	openAIResponsesWebsocketPool.mu.Unlock()
	for _, conn := range conns {
		if conn != nil && conn.conn != nil {
			_ = conn.conn.Close()
		}
	}
}

func newOpenAIResponsesWSPool() *openAIResponsesWSPool {
	return &openAIResponsesWSPool{
		dialing:  map[string]int{},
		bindings: map[string]*openAIResponsesWSConn{},
		idleWait: map[string][]chan struct{}{},
	}
}

func (p *openAIResponsesWSPool) roundTrip(ctx context.Context, spec openAIResponsesWSSpec, body []byte) (*http.Response, error) {
	prevID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	p.retireStale(spec)
	conn, handshake, err := p.acquire(ctx, spec, prevID)
	if handshake != nil || err != nil || conn == nil {
		return handshake, err
	}
	if err := writeOpenAIResponsesWS(ctx, conn.conn, body); err != nil {
		reused := conn.reused
		p.retire(conn)
		if !reused {
			return nil, fmt.Errorf("OpenAI Responses WebSocket 发送失败: %w", err)
		}
		conn, handshake, err = p.dial(ctx, spec)
		if handshake != nil || err != nil || conn == nil {
			return handshake, err
		}
		if err := writeOpenAIResponsesWS(ctx, conn.conn, body); err != nil {
			p.retire(conn)
			return nil, fmt.Errorf("OpenAI Responses WebSocket 发送失败: %w", err)
		}
	}
	return openAIResponsesWSStream(ctx, p, conn), nil
}

func writeOpenAIResponsesWS(ctx context.Context, conn *websocket.Conn, body []byte) error {
	deadline := time.Now().Add(30 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, body)
}

func openAIResponsesWSStream(ctx context.Context, pool *openAIResponsesWSPool, conn *openAIResponsesWSConn) *http.Response {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		Body:          pr,
		ContentLength: -1,
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Set("Cache-Control", "no-cache")
	resp.Header.Set("Connection", "keep-alive")

	go func() {
		defer pw.Close()
		var finished atomic.Bool
		stop := make(chan struct{})
		socket := conn.conn
		go func() {
			select {
			case <-ctx.Done():
				if !finished.Load() && socket != nil {
					_ = socket.Close()
				}
			case <-stop:
			}
		}()
		defer close(stop)

		terminal := false
		err := readOpenAIResponsesWS(conn.conn, func(frame []byte) bool {
			if id := strings.TrimSpace(gjson.GetBytes(frame, "response.id").String()); id != "" {
				pool.bind(conn, id)
			}
			switch gjson.GetBytes(frame, "type").String() {
			case "response.completed", "response.failed", "response.incomplete", "error":
				terminal = true
			}
			line := frame
			if bytes.IndexByte(frame, '\n') >= 0 {
				var compacted bytes.Buffer
				if json.Compact(&compacted, frame) == nil {
					line = compacted.Bytes()
				} else {
					line = bytes.ReplaceAll(frame, []byte("\n"), []byte(" "))
				}
			}
			if _, err := pw.Write([]byte("data: ")); err != nil {
				return false
			}
			if _, err := pw.Write(line); err != nil {
				return false
			}
			if _, err := pw.Write([]byte("\n\n")); err != nil {
				return false
			}
			return !terminal
		})
		finished.Store(true)
		if err != nil || !terminal || ctx.Err() != nil {
			pool.retire(conn)
			if err != nil && err != io.EOF {
				_ = pw.CloseWithError(err)
			}
			return
		}
		pool.release(conn)
	}()
	return resp
}

func readOpenAIResponsesWS(conn *websocket.Conn, emit func([]byte) bool) error {
	conn.SetReadLimit(32 << 20)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		if !emit(data) {
			return nil
		}
	}
}

func (p *openAIResponsesWSPool) acquire(ctx context.Context, spec openAIResponsesWSSpec, prevID string) (*openAIResponsesWSConn, *http.Response, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		if prevID != "" {
			if conn := p.bindings[openAIResponsesWSBindingKey(spec.accountID, prevID)]; conn != nil && conn.spec.id() == spec.id() && !conn.dead {
				if conn.expired() {
					socket := p.retireLocked(conn)
					p.mu.Unlock()
					if socket != nil {
						_ = socket.Close()
					}
					continue
				} else if !conn.busy {
					conn.busy = true
					conn.reused = true
					p.mu.Unlock()
					return conn, nil, nil
				} else {
					ch := make(chan struct{}, 1)
					conn.waiters = append(conn.waiters, ch)
					p.mu.Unlock()
					select {
					case <-ctx.Done():
						return nil, nil, ctx.Err()
					case <-ch:
						continue
					}
				}
			}
		}
		if conn := p.popIdleLocked(spec); conn != nil {
			conn.busy = true
			conn.reused = true
			p.mu.Unlock()
			return conn, nil, nil
		}
		if p.liveCountLocked(spec.id())+p.dialing[spec.id()] >= openAIResponsesWSMaxConns {
			ch := make(chan struct{}, 1)
			p.idleWait[spec.id()] = append(p.idleWait[spec.id()], ch)
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-ch:
				continue
			}
		}
		p.dialing[spec.id()]++
		p.mu.Unlock()

		conn, handshake, err := p.dial(ctx, spec)
		p.mu.Lock()
		p.dialing[spec.id()]--
		if p.dialing[spec.id()] < 0 {
			p.dialing[spec.id()] = 0
		}
		p.wakeSpecLocked(spec.id())
		p.mu.Unlock()
		return conn, handshake, err
	}
}

func (p *openAIResponsesWSPool) dial(ctx context.Context, spec openAIResponsesWSSpec) (*openAIResponsesWSConn, *http.Response, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout:  30 * time.Second,
		EnableCompression: true,
		Proxy:             http.ProxyFromEnvironment,
	}
	if spec.proxyURL != "" {
		if err := configureOpenAIResponsesWSProxy(dialer, spec.proxyURL); err != nil {
			return nil, nil, err
		}
	}
	conn, resp, err := dialer.DialContext(ctx, spec.wsURL, spec.headers)
	if err != nil {
		handshake, handshakeErr := openAIResponsesWSHandshakeResponse(err, resp)
		if handshake != nil {
			return nil, handshake, nil
		}
		if handshakeErr != nil {
			return nil, nil, handshakeErr
		}
		return nil, nil, fmt.Errorf("OpenAI Responses WebSocket 握手失败: %w", err)
	}
	wrapped := &openAIResponsesWSConn{
		spec:     spec,
		conn:     conn,
		created:  time.Now(),
		busy:     true,
		response: map[string]struct{}{},
	}
	p.mu.Lock()
	p.conns = append(p.conns, wrapped)
	p.mu.Unlock()
	return wrapped, nil, nil
}

func configureOpenAIResponsesWSProxy(dialer *websocket.Dialer, rawProxyURL string) error {
	parsed, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL failed: %w", err)
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme == "socks5h" {
		parsed.Scheme = "socks5"
	}
	dialer.Proxy = http.ProxyURL(parsed)
	return nil
}

func openAIResponsesWSHandshakeResponse(err error, resp *http.Response) (*http.Response, error) {
	if resp == nil {
		return nil, nil
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil && err == nil {
		err = readErr
	}
	status := resp.StatusCode
	if status == 0 || status == http.StatusSwitchingProtocols {
		if err != nil {
			return nil, fmt.Errorf("OpenAI Responses WebSocket 握手失败: %w", err)
		}
		return nil, nil
	}
	header := resp.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode:    status,
		Status:        resp.Status,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}, nil
}

func (p *openAIResponsesWSPool) bind(conn *openAIResponsesWSConn, responseID string) {
	if conn == nil || strings.TrimSpace(responseID) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn.dead {
		return
	}
	if conn.response == nil {
		conn.response = map[string]struct{}{}
	}
	conn.response[responseID] = struct{}{}
	p.bindings[openAIResponsesWSBindingKey(conn.spec.accountID, responseID)] = conn
}

func openAIResponsesWSBindingKey(accountID int64, responseID string) string {
	return fmt.Sprintf("%d\n%s", accountID, responseID)
}

func (p *openAIResponsesWSPool) release(conn *openAIResponsesWSConn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn.dead {
		p.wakeLocked(conn)
		p.wakeSpecLocked(conn.spec.id())
		return
	}
	if conn.expired() {
		p.retireLocked(conn)
		return
	}
	conn.busy = false
	conn.reused = false
	p.wakeLocked(conn)
	p.wakeSpecLocked(conn.spec.id())
}

func (p *openAIResponsesWSPool) retire(conn *openAIResponsesWSConn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	socket := p.retireLocked(conn)
	p.mu.Unlock()
	if socket != nil {
		_ = socket.Close()
	}
}

func (p *openAIResponsesWSPool) retireLocked(conn *openAIResponsesWSConn) *websocket.Conn {
	if conn == nil || conn.dead {
		return nil
	}
	conn.dead = true
	conn.busy = false
	filtered := p.conns[:0]
	for _, candidate := range p.conns {
		if candidate != conn {
			filtered = append(filtered, candidate)
		}
	}
	p.conns = filtered
	for id := range conn.response {
		key := openAIResponsesWSBindingKey(conn.spec.accountID, id)
		if p.bindings[key] == conn {
			delete(p.bindings, key)
		}
	}
	p.wakeLocked(conn)
	p.wakeSpecLocked(conn.spec.id())
	socket := conn.conn
	conn.conn = nil
	return socket
}

func (p *openAIResponsesWSPool) retireStale(spec openAIResponsesWSSpec) {
	p.mu.Lock()
	snapshot := append([]*openAIResponsesWSConn(nil), p.conns...)
	var sockets []*websocket.Conn
	for _, conn := range snapshot {
		if conn.spec.accountID == spec.accountID && conn.spec.id() != spec.id() && !conn.busy && !conn.dead {
			if socket := p.retireLocked(conn); socket != nil {
				sockets = append(sockets, socket)
			}
		}
	}
	p.mu.Unlock()
	for _, socket := range sockets {
		_ = socket.Close()
	}
}

func (p *openAIResponsesWSPool) popIdleLocked(spec openAIResponsesWSSpec) *openAIResponsesWSConn {
	var expired []*openAIResponsesWSConn
	var found *openAIResponsesWSConn
	for _, conn := range p.conns {
		if conn.dead || conn.busy || conn.spec.id() != spec.id() {
			continue
		}
		if conn.expired() {
			expired = append(expired, conn)
			continue
		}
		found = conn
		break
	}
	for _, conn := range expired {
		if socket := p.retireLocked(conn); socket != nil {
			_ = socket.Close()
		}
	}
	return found
}

func (p *openAIResponsesWSPool) liveCountLocked(specID string) int {
	count := 0
	for _, conn := range p.conns {
		if !conn.dead && conn.spec.id() == specID {
			count++
		}
	}
	return count
}

func (c *openAIResponsesWSConn) expired() bool {
	return c == nil || time.Since(c.created) > openAIResponsesWSMaxAge
}

func (p *openAIResponsesWSPool) wakeLocked(conn *openAIResponsesWSConn) {
	if conn == nil {
		return
	}
	waiters := conn.waiters
	conn.waiters = nil
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *openAIResponsesWSPool) wakeSpecLocked(specID string) {
	waiters := p.idleWait[specID]
	delete(p.idleWait, specID)
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
