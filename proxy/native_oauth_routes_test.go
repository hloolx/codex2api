package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

func TestNativeOAuthRoutesWithLegacyCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := proxy.CurrentRuntimeSettings()
	previousResin := proxy.GetResinConfig()
	previousWebsocket := proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.SetResinConfig(previousResin)
		proxy.WebsocketExecuteFunc = previousWebsocket
	})
	runtime := previousRuntime
	runtime.CodexForceWebsocket = false
	runtime.CodexRequestCompression = true
	runtime.CodexTelemetryEnabled = false
	runtime.CompactViaResponses = false
	proxy.ApplyRuntimeSettings(runtime)
	proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "native-routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "legacy-oauth", map[string]any{
		"access_token":             "native-route-token",
		"account_id":               "native-route-account",
		"plan_type":                "plus",
		"models":                   []string{"gpt-5.5"},
		"openai_excel_bps":         true,
		"openai_excel_bps_opt_out": true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5", MaxRetries: 1,
		FastSchedulerEnabled: true, CodexRequestCompression: true,
	})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	if account == nil || account.IsRelayStyle() || account.IsCodexAgentIdentity() {
		t.Fatal("legacy OAuth fixture did not load as a native account")
	}

	events := []string{
		`{"type":"response.created","response":{"id":"resp_native","model":"gpt-5.5","status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_native","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"native-ok"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"native-ok"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_native","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native-ok"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_native","model":"gpt-5.5","status":"completed","output":[{"id":"msg_native","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native-ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	}
	type observedRequest struct {
		path      string
		headers   http.Header
		body      []byte
		websocket bool
	}
	requests := make(chan observedRequest, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := observedRequest{path: r.URL.Path, headers: r.Header.Clone(), websocket: websocket.IsWebSocketUpgrade(r)}
		if observed.websocket {
			conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade native upstream: %v", err)
				return
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, observed.body, err = conn.ReadMessage()
			if err != nil {
				t.Errorf("read native upstream frame: %v", err)
				return
			}
			requests <- observed
			for _, event := range events {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					t.Errorf("write native upstream frame: %v", err)
					return
				}
			}
			return
		}
		var err error
		observed.body, err = io.ReadAll(r.Body)
		if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
			var decoder *zstd.Decoder
			decoder, err = zstd.NewReader(nil)
			if err == nil {
				observed.body, err = decoder.DecodeAll(observed.body, nil)
				decoder.Close()
			}
		}
		if err != nil {
			t.Errorf("read native upstream body: %v", err)
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		requests <- observed
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_compact","output":[{"type":"compaction","encrypted_content":"native-compact"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	t.Cleanup(upstream.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "native-routes"})

	handler := proxy.NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	assertNativeAttempt := func(t *testing.T, suffix string, wantWebsocket bool) {
		t.Helper()
		select {
		case request := <-requests:
			if want := "/native-routes/https/chatgpt.com/backend-api/codex/" + suffix; request.path != want {
				t.Fatalf("upstream path = %q, want %q", request.path, want)
			}
			if request.websocket != wantWebsocket {
				t.Fatalf("upstream websocket = %v, want %v", request.websocket, wantWebsocket)
			}
			if request.headers.Get("Authorization") != "Bearer native-route-token" || request.headers.Get("Chatgpt-Account-Id") != "native-route-account" || request.headers.Get("X-Resin-Account") != strconv.FormatInt(id, 10) {
				t.Fatal("native OAuth identity headers were not preserved")
			}
			if gjson.GetBytes(request.body, "model").String() != "gpt-5.5" || !strings.Contains(string(request.body), "hello") {
				t.Fatalf("native request lost model or input: %s", request.body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native upstream did not receive the request")
		}
		deadline := time.Now().Add(time.Second)
		for account.ActiveRequests.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if active := account.ActiveRequests.Load(); active != 0 {
			t.Fatalf("account concurrency was not released: %d", active)
		}
		select {
		case request := <-requests:
			t.Fatalf("unexpected extra upstream attempt: %s", request.path)
		default:
		}
	}
	tests := []struct {
		name, path, body, jsonPath, want, terminal string
	}{
		{"responses_json", "/v1/responses", `{"model":"gpt-5.5","stream":false,"input":"hello"}`, "output.0.content.0.text", "native-ok", ""},
		{"responses_stream", "/v1/responses", `{"model":"gpt-5.5","stream":true,"input":"hello"}`, "", "native-ok", "response.completed"},
		{"compact", "/v1/responses/compact", `{"model":"gpt-5.5","input":"hello"}`, "output.0.encrypted_content", "native-compact", ""},
		{"chat_json", "/v1/chat/completions", `{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hello"}]}`, "choices.0.message.content", "native-ok", ""},
		{"chat_stream", "/v1/chat/completions", `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`, "", "native-ok", "[DONE]"},
		{"messages_json", "/v1/messages", `{"model":"gpt-5.5","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, "content.0.text", "native-ok", ""},
		{"messages_stream", "/v1/messages", `{"model":"gpt-5.5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, "", "native-ok", "message_stop"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if test.jsonPath != "" {
				if got := gjson.GetBytes(recorder.Body.Bytes(), test.jsonPath).String(); got != test.want {
					t.Fatalf("%s = %q, want %q; body=%s", test.jsonPath, got, test.want, recorder.Body.String())
				}
			} else if body := recorder.Body.String(); !strings.Contains(body, test.want) || !strings.Contains(body, test.terminal) {
				t.Fatalf("stream output or terminal missing: %s", body)
			}
			suffix := "responses"
			if test.path == "/v1/responses/compact" {
				suffix += "/compact"
			}
			assertNativeAttempt(t, suffix, false)
		})
	}
	t.Run("responses_websocket", func(t *testing.T) {
		server := httptest.NewServer(router)
		defer server.Close()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello"}`)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var gotText bool
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			typeName := gjson.GetBytes(payload, "type").String()
			if typeName == "response.output_text.delta" && gjson.GetBytes(payload, "delta").String() == "native-ok" {
				gotText = true
			}
			if typeName == "response.completed" {
				if !gotText || gjson.GetBytes(payload, "response.status").String() != "completed" {
					t.Fatalf("native websocket output incomplete: %s", payload)
				}
				break
			}
		}
		assertNativeAttempt(t, "responses", true)
	})
	if dispatched := account.TotalRequests.Load(); dispatched != int64(len(tests)+1) {
		t.Fatalf("logical dispatches = %d, want %d", dispatched, len(tests)+1)
	}
}
