package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestContinuousRetryGeminiTimeoutUsesNativeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		committed   bool
		lastFailure bool
	}{
		{name: "timeout JSON"},
		{name: "timeout SSE", committed: true},
		{name: "last failure JSON", lastFailure: true},
		{name: "last failure SSE", committed: true, lastFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", nil)
			stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, continuousRetryProtocolGemini)
			wantCode, wantStatus, wantMessage := http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", continuousRetryTimeoutMessage
			if tc.lastFailure {
				wantCode, wantStatus, wantMessage = http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "rate_limit_error · quota exhausted"
				rememberContinuousRetryFailure(c.Request.Context(), continuousRetryFailure{
					status:      wantCode,
					body:        []byte(`{"error":{"message":"quota exhausted","type":"rate_limit_error"}}`),
					contentType: "application/json",
				})
			}
			if tc.committed {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: {}\n\n")
			}
			continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
			if !writeContinuousRetryTimeoutResponse(c, continuousRetryProtocolGemini) {
				t.Fatal("timeout was not handled")
			}
			stop()
			wantHTTP := wantCode
			body := recorder.Body.String()
			if tc.committed {
				wantHTTP = http.StatusOK
				if !recorder.Flushed || strings.Count(body, "data: ") != 1 || strings.Count(body, `"error":`) != 1 {
					t.Fatalf("expected one flushed bare JSON terminal error, got %q (flushed=%v)", body, recorder.Flushed)
				}
				body = strings.TrimPrefix(body, "data: {}\n\n")
				if body != strings.TrimSpace(body) {
					t.Fatalf("terminal error must end at JSON without a trailing SSE delimiter: %q", body)
				}
			}
			if recorder.Code != wantHTTP {
				t.Fatalf("HTTP status = %d, want %d", recorder.Code, wantHTTP)
			}
			var payload struct {
				Error struct {
					Code    int    `json:"code"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &payload); err != nil {
				t.Fatalf("invalid Gemini error %q: %v", body, err)
			}
			if payload.Error.Code != wantCode || payload.Error.Status != wantStatus || payload.Error.Message != wantMessage {
				t.Fatalf("unexpected Gemini error: %+v", payload.Error)
			}
			if strings.Contains(body, "response.failed") {
				t.Fatalf("Gemini timeout contains a Responses event: %q", body)
			}
		})
	}
}

func TestGeminiHandlerContinuousRetryDeadlineUsesNativeError(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	// Install a short real retry timer on the request rather than replacing it
	// with the handler's policy timer, whose minimum duration is one second.
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = time.Hour
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })
	for _, afterThought := range []bool{false, true} {
		name := "waiting for upstream headers"
		if afterThought {
			name = "after streamed thought"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			deadline := &continuousRetryDeadline{duration: 40 * time.Millisecond, cancel: cancel}
			defer deadline.Stop()
			ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
			upstreamCanceled := make(chan struct{})
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if afterThought {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+geminiStreamTestThought+"\n\n")
					w.(http.Flusher).Flush()
				}
				deadline.Activate()
				<-r.Context().Done()
				close(upstreamCanceled)
			})
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				router.ServeHTTP(recorder, geminiStreamTestHTTPRequest(ctx, "streamGenerateContent"))
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel(nil)
				t.Fatal("handler did not terminate at continuous retry deadline")
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(time.Second):
				t.Fatal("deadline did not cancel the upstream request")
			}
			body := recorder.Body.String()
			payload := body
			wantHTTP := http.StatusGatewayTimeout
			if afterThought {
				wantHTTP = http.StatusOK
				if !strings.Contains(body, "THOUGHT_ONLY") {
					t.Fatalf("fixture did not stream thought before timeout: %s", body)
				}
				lastFrameEnd := strings.LastIndex(body, "\n\n")
				if lastFrameEnd < 0 {
					t.Fatalf("missing SSE frame before terminal error: %s", body)
				}
				payload = body[lastFrameEnd+2:]
				if !json.Valid([]byte(payload)) || payload != strings.TrimSpace(payload) || strings.Count(body, `"error":`) != 1 {
					t.Fatalf("expected one bare JSON terminal error without a trailing SSE delimiter: %s", body)
				}
				assertGeminiStreamUsageStatus(t, db, http.StatusGatewayTimeout, true)
			}
			if recorder.Code != wantHTTP || gjson.Get(payload, "error.code").Int() != http.StatusGatewayTimeout || gjson.Get(payload, "error.status").String() != "DEADLINE_EXCEEDED" {
				t.Fatalf("status=%d body=%s, want native Gemini deadline error", recorder.Code, body)
			}
			if strings.Contains(body, "response.failed") || strings.Contains(body, `"finishReason":"STOP"`) {
				t.Fatalf("timeout contains an invalid terminal: %s", body)
			}
		})
	}
}

func TestWriteGeminiNativeErrorDoesNotAppendSSEToCommittedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.JSON(http.StatusOK, gin.H{"candidates": []any{}})
	before := recorder.Body.String()
	writeGeminiNativeError(c, http.StatusGatewayTimeout, continuousRetryTimeoutMessage)
	if got := recorder.Body.String(); got != before || !json.Valid([]byte(got)) {
		t.Fatalf("error writer corrupted committed JSON: %s", got)
	}
}
