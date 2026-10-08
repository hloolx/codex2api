package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	geminiStreamTestThought = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"THOUGHT_ONLY","thought":true}]}}]}}`
	geminiStreamTestAnswer  = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ANSWER TEST_END"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"thoughtsTokenCount":5,"totalTokenCount":35}}}`
	geminiStreamTestRequest = `{"contents":[{"role":"user","parts":[{"text":"give ANSWER TEST_END"}]}]}`
)

func newGeminiStreamTestHandler(t *testing.T, upstreamHandler http.HandlerFunc) (*gin.Engine, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "gemini-stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
	keyID, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name: "Gemini stream test", Key: "gemini-stream-test-key",
		Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := db.GetAPIKeyByID(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamHandler(w, r)
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 72301, UpstreamType: auth.UpstreamAntigravity,
		AccessToken: "test-token", AntigravityProjectID: "test-project",
		Models: []string{"gemini-3.7-flash-tiered"}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady,
	})
	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyRow, key)
		c.Next()
	})
	router.POST("/v1beta/models/*action", handler.GeminiModelsAction)
	return router, db
}

func geminiStreamTestHTTPRequest(ctx context.Context, method string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.7-flash-low:"+method+"?alt=sse", strings.NewReader(geminiStreamTestRequest)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func assertGeminiStreamUsageStatus(t *testing.T, db *database.DB, want int, stream bool) database.UsageLog {
	t.Helper()
	db.FlushUsageLogs()
	logs, err := db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage logs = %d, want 1: %+v", len(logs), logs)
	}
	if entry := logs[0]; entry.StatusCode != want || entry.Stream != stream {
		t.Fatalf("usage status=%d stream=%v, want status=%d stream=%v", entry.StatusCode, entry.Stream, want, stream)
	}
	return *logs[0]
}

func TestGeminiNativeStreamFlushesThoughtAndKeepsAliveBeforeAnswer(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 30 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })
	allowAnswer := make(chan struct{})
	var answerOnce sync.Once
	releaseAnswer := func() { answerOnce.Do(func() { close(allowAnswer) }) }
	t.Cleanup(releaseAnswer)
	upstreamCanceled := make(chan struct{})
	router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+geminiStreamTestThought+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-allowAnswer:
			_, _ = io.WriteString(w, "data: "+geminiStreamTestAnswer+"\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			close(upstreamCanceled)
		}
	})
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		close(handlerDone)
	}))
	t.Cleanup(downstream.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/v1beta/models/gemini-3.7-flash-low:streamGenerateContent?alt=sse", strings.NewReader(geminiStreamTestRequest))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := downstream.Client().Do(req)
	if err != nil {
		t.Fatalf("thought frame was not flushed while the upstream answer was blocked: %v", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		first, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read thought before releasing answer: %v", err)
		}
		if !strings.HasPrefix(first, "data: ") {
			continue
		}
		if strings.TrimSpace(first) == "data: {}" {
			continue
		}
		if !strings.Contains(first, "THOUGHT_ONLY") || !strings.Contains(first, `"thought":true`) {
			t.Fatalf("first frame=%q, want thought before releasing answer", first)
		}
		break
	}
	// The upstream remains silent until the client receives a heartbeat. This
	// proves keepalive continues during body reads, after the headers and thought.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for keepalive while the upstream answer is blocked: %v", err)
		}
		if strings.TrimSpace(line) == "data: {}" {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			t.Fatalf("unexpected generation frame before releasing answer: %s", line)
		}
	}
	select {
	case <-upstreamCanceled:
		t.Fatal("upstream was canceled before the answer")
	default:
	}
	releaseAnswer()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rest), "ANSWER TEST_END") || !strings.Contains(string(rest), `"finishReason":"STOP"`) {
		t.Fatalf("status=%d rest=%s, want complete native answer", resp.StatusCode, rest)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not release the completed stream")
	}
	assertGeminiStreamUsageStatus(t, db, http.StatusOK, true)
}

func TestGeminiNativeStreamFailureIsNotSuccessfulStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		truncated bool
	}{
		{name: "EOF without terminal"},
		{name: "HTTP body interrupted", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.truncated {
					w.Header().Set("Content-Length", "10000")
				}
				_, _ = io.WriteString(w, "data: "+geminiStreamTestThought+"\n\n")
				w.(http.Flusher).Flush()
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, geminiStreamTestHTTPRequest(context.Background(), "streamGenerateContent"))
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(body, "THOUGHT_ONLY") {
				t.Fatalf("status=%d body=%s, want partial content followed by a stream error", recorder.Code, body)
			}
			if strings.Contains(body, `"finishReason":"STOP"`) {
				t.Fatalf("failed stream was reported as successful STOP: %s", body)
			}
			// The Google SDK expects a bare JSON error after the SSE data
			// events. A data-prefixed error is silently discarded by older SDKs.
			errorOffset := strings.Index(body, "\n\n{\"error\":")
			if errorOffset < 0 {
				t.Fatalf("missing native Gemini stream error: %s", body)
			}
			payload := body[errorOffset+2:]
			if !gjson.Valid(payload) || gjson.Get(payload, "error.code").Int() != http.StatusBadGateway || gjson.Get(payload, "error.status").String() != "UNAVAILABLE" {
				t.Fatalf("invalid native Gemini stream error: %s", body)
			}
			assertGeminiStreamUsageStatus(t, db, logStatusUpstreamStreamBreak, true)
		})
	}
}

func TestGeminiNativeStreamFailureBeforeContentReturnsHTTPError(t *testing.T) {
	router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, geminiStreamTestHTTPRequest(context.Background(), "streamGenerateContent"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || gjson.Get(body, "error.code").Int() != http.StatusBadGateway || gjson.Get(body, "error.status").String() != "UNAVAILABLE" {
		t.Fatalf("status=%d body=%s, want Gemini HTTP 502 error", recorder.Code, body)
	}
	assertGeminiStreamUsageStatus(t, db, logStatusUpstreamStreamBreak, true)
}

type geminiStreamDisconnectWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	fail   bool
	wrote  bool
}

func (w *geminiStreamDisconnectWriter) Write(p []byte) (int, error) {
	w.wrote = true
	if w.fail {
		return 0, errors.New("test downstream disconnected")
	}
	n, err := w.ResponseRecorder.Write(p)
	w.cancel()
	return n, err
}

func (w *geminiStreamDisconnectWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestGeminiNativeStreamDownstreamDisconnectReleasesUpstream(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "client cancellation"
		if failWrite {
			name = "downstream write failure"
		}
		t.Run(name, func(t *testing.T) {
			upstreamCanceled := make(chan struct{})
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+geminiStreamTestThought+"\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamCanceled)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &geminiStreamDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, fail: failWrite}
			handlerDone := make(chan struct{})
			go func() {
				router.ServeHTTP(writer, geminiStreamTestHTTPRequest(ctx, "streamGenerateContent"))
				close(handlerDone)
			}()
			select {
			case <-handlerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not terminate after downstream disconnect")
			}
			if !writer.wrote {
				t.Fatal("fixture did not reach downstream write")
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(time.Second):
				t.Fatal("upstream request was not canceled after downstream disconnect")
			}
			assertGeminiStreamUsageStatus(t, db, logStatusClientClosed, true)
		})
	}
}

func TestGeminiNativeNonStreamStillReturnsCompleteAnswer(t *testing.T) {
	router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiStreamTestAnswer)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, geminiStreamTestHTTPRequest(context.Background(), "generateContent"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || gjson.Get(body, "candidates.0.content.parts.0.text").String() != "ANSWER TEST_END" || gjson.Get(body, "candidates.0.finishReason").String() != "STOP" {
		t.Fatalf("status=%d body=%s, want complete native answer", recorder.Code, body)
	}
	if gjson.Get(body, "response").Exists() {
		t.Fatalf("upstream envelope was not removed: %s", body)
	}
	assertGeminiStreamUsageStatus(t, db, http.StatusOK, false)
}
