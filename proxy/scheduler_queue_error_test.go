package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func saturatedSchedulerQueue(t *testing.T) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	store.SetSchedulerWaitLimits(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = store.WaitForDispatchAvailable(ctx, "", time.Minute, 0, nil, nil, false, auth.DispatchPolicyStandard)
	}()
	t.Cleanup(func() { cancel(); <-done })
	until := time.Now().Add(time.Second)
	for store.GetSchedulerMetrics().Waiters != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if store.GetSchedulerMetrics().Waiters != 1 {
		t.Fatal("queue did not fill")
	}
	return store
}

func TestSchedulerQueueOverloadStopsContinuousRetry(t *testing.T) {
	s := saturatedSchedulerQueue(t)
	h := &Handler{store: s}
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(99)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	if !errors.Is(err, auth.ErrSchedulerQueueFull) || ctx.Err() != nil {
		t.Fatalf("overload was retried: %v (context %v)", err, ctx.Err())
	}
	if !exclusions.ForSelection()[99] {
		t.Fatal("queue rejection reset upstream retry exclusions")
	}
	m := s.GetSchedulerMetrics()
	if m.WaitRejected != 1 || m.WaitRejectedPerKey != 1 || m.Waiters != 1 {
		t.Fatalf("rejection metrics = %+v", m)
	}
}

func TestSchedulerQueueOverloadHTTPRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, body string
		invoke           func(*Handler, *gin.Context)
	}{
		{"responses", "/v1/responses", `{"model":"gpt-5.5","input":"hello"}`, (*Handler).Responses},
		{"compact", "/v1/responses/compact", `{"model":"gpt-5.5","input":"hello"}`, (*Handler).ResponsesCompact},
		{"chat", "/v1/chat/completions", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`, (*Handler).ChatCompletions},
		{"messages", "/v1/messages", `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, (*Handler).Messages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := saturatedSchedulerQueue(t)
			h := NewHandler(s, nil, &config.Config{AllowAnonymousV1: true}, nil)
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			tc.invoke(h, c)
			if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" || !strings.Contains(r.Body.String(), schedulerQueueFullMessage) {
				t.Fatalf("overload response = %d, retry-after=%q, %s", r.Code, r.Header().Get("Retry-After"), r.Body.String())
			}
		})
	}
}

func TestSchedulerQueueOverloadCommittedSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		protocol continuousRetryHTTPProtocol
		marker   string
	}{
		{continuousRetryProtocolResponses, `"type":"response.failed"`},
		{continuousRetryProtocolChat, `"error"`},
		{continuousRetryProtocolAnthropic, "event: error"},
		{continuousRetryProtocolGemini, `"status":"UNAVAILABLE"`},
	} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Header("Content-Type", "text/event-stream")
		keepalive := ": keepalive\n\n"
		if tc.protocol == continuousRetryProtocolGemini {
			keepalive = "data: {}\n\n"
		}
		_, _ = c.Writer.WriteString(keepalive)
		c.Writer.Flush()
		if !writeSchedulerQueueError(c, auth.ErrSchedulerQueueFull, tc.protocol) {
			t.Fatal("overload not handled")
		}
		body := r.Body.String()
		if r.Code != http.StatusOK || !strings.HasPrefix(body, keepalive) || !strings.Contains(body, tc.marker) || !strings.Contains(body, schedulerQueueFullMessage) {
			t.Fatalf("committed overload response = %d %s", r.Code, body)
		}
		if tc.protocol == continuousRetryProtocolGemini && (strings.Contains(body, "response.failed") || !strings.Contains(body, `"code":503`)) {
			t.Fatalf("overload response must use the Gemini error envelope: %s", body)
		}
		if tc.protocol == continuousRetryProtocolGemini && (!json.Valid([]byte(strings.TrimPrefix(body, keepalive))) || strings.HasSuffix(body, "\n")) {
			t.Fatalf("Gemini SDK expects a terminal bare JSON error without an SSE delimiter: %q", body)
		}
	}
}

func TestSchedulerQueueOverloadGeminiJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:generateContent", nil)
	if !writeSchedulerQueueError(c, auth.ErrSchedulerQueueFull, continuousRetryProtocolGemini) {
		t.Fatal("overload not handled")
	}
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid Gemini JSON response: %s", r.Body.String())
	}
	if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" || payload.Error.Code != http.StatusServiceUnavailable || payload.Error.Status != "UNAVAILABLE" || payload.Error.Message != schedulerQueueFullMessage {
		t.Fatalf("Gemini overload response = %d, retry-after=%q, %s", r.Code, r.Header().Get("Retry-After"), r.Body.String())
	}
}

func TestSelectionTimeoutHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		protocol continuousRetryHTTPProtocol
		marker   string
	}{
		{continuousRetryProtocolResponses, `"type":"response.failed"`},
		{continuousRetryProtocolChat, `"error"`},
		{continuousRetryProtocolAnthropic, "event: error"},
		{continuousRetryProtocolGemini, `"status":"UNAVAILABLE"`},
	} {
		for _, committed := range []bool{false, true} {
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			keepalive := ": keepalive\n\n"
			if tc.protocol == continuousRetryProtocolGemini {
				keepalive = "data: {}\n\n"
			}
			if committed {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString(keepalive)
				c.Writer.Flush()
			}
			if !writeSchedulerQueueError(c, context.DeadlineExceeded, tc.protocol) {
				t.Fatal("selection timeout would fall through to another account scan")
			}
			body := r.Body.String()
			if !strings.Contains(body, schedulerSelectionTimeoutMessage) {
				t.Fatalf("missing selection timeout: %s", body)
			}
			if tc.protocol == continuousRetryProtocolGemini && (strings.Contains(body, "response.failed") || !strings.Contains(body, `"code":503`) || !strings.Contains(body, tc.marker)) {
				t.Fatalf("selection timeout must use the Gemini error envelope: %s", body)
			}
			if tc.protocol == continuousRetryProtocolGemini && committed && (!json.Valid([]byte(strings.TrimPrefix(body, keepalive))) || strings.HasSuffix(body, "\n")) {
				t.Fatalf("Gemini SDK expects a terminal bare JSON error without an SSE delimiter: %q", body)
			}
			if committed {
				if r.Code != http.StatusOK || !strings.HasPrefix(body, keepalive) || !strings.Contains(body, tc.marker) {
					t.Fatalf("committed timeout response = %d %s", r.Code, body)
				}
			} else if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "1" {
				t.Fatalf("timeout response = %d, retry-after=%q", r.Code, r.Header().Get("Retry-After"))
			}
		}
	}
}

func TestSchedulerQueueOverloadResponsesWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := saturatedSchedulerQueue(t)
	h := NewHandler(s, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.5", "input": "hello"}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil || event["type"] != "error" || !strings.Contains(string(payload), schedulerQueueFullMessage) {
		t.Fatalf("WS overload frame = %s", payload)
	}
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("WS overload close = %v", err)
	}
}

func TestSchedulerWaitHeartbeatPreservesOneAdmission(t *testing.T) {
	previous := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previous })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	defer store.Stop()
	h := &Handler{store: store}
	keepalive := &recordingContinuousRetryKeepalive{active: true}
	ctx, cancel := context.WithTimeout(contextWithContinuousRetryKeepalive(keepalive), 35*time.Millisecond)
	defer cancel()
	_, _, _, _ = h.waitForRetryAccountAvailableWithGuard(ctx, "", 0, nil, nil, false, auth.DispatchPolicyStandard)
	m := store.GetSchedulerMetrics()
	if keepalive.writes < 2 || m.WaitStarted != 1 || m.Waiters != 0 || m.SelectionTotal != 1 {
		t.Fatalf("heartbeats disturbed queue: writes=%d metrics=%+v", keepalive.writes, m)
	}
}
