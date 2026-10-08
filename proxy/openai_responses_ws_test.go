package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesWebsocketURL(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{base: "https://api.openai.com", want: "wss://api.openai.com/v1/responses"},
		{base: "https://relay.example/v1", want: "wss://relay.example/v1/responses"},
		{base: "http://127.0.0.1:9", want: "ws://127.0.0.1:9/v1/responses"},
	}
	for _, tc := range cases {
		got, err := openAIResponsesWebsocketURL(tc.base)
		if err != nil {
			t.Fatalf("openAIResponsesWebsocketURL(%q) error = %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("openAIResponsesWebsocketURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestAccountFilterForResponsesWebSocket(t *testing.T) {
	filter := accountFilterForResponsesWebSocket("gpt-5.5")
	websocketAccount := &auth.Account{
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    "https://relay.example",
		APIKey:                     "sk-test",
		Models:                     []string{"gpt-5.5"},
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	httpAccount := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example",
		APIKey:       "sk-http",
		Models:       []string{"gpt-5.5"},
	}
	grok := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", Models: []string{"gpt-5.5"}}
	codex := &auth.Account{AccessToken: "at-test"}
	if !filter(websocketAccount) {
		t.Fatal("websocket relay account was excluded")
	}
	if filter(httpAccount) || filter(grok) {
		t.Fatal("http relay or grok account was admitted")
	}
	if !filter(codex) {
		t.Fatal("codex account was excluded")
	}
	if accountFilterForResponsesWebSocket("gpt-other")(websocketAccount) {
		t.Fatal("websocket relay account matched a model outside its whitelist")
	}
}

func TestExecuteRequestStillRejectsRelayWebsocketAccount(t *testing.T) {
	account := &auth.Account{
		DBID:                       7,
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    "https://relay.example",
		APIKey:                     "sk-test",
		AccessToken:                "should-not-send",
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	_, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "", nil, nil, true)
	if err == nil {
		t.Fatal("ExecuteRequest accepted a relay account")
	}
}

func TestExecuteOpenAIResponsesWebsocketDialAndReuse(t *testing.T) {
	resetOpenAIResponsesWebsocketPool()
	t.Cleanup(resetOpenAIResponsesWebsocketPool)

	var upgrades atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Errorf("unexpected non-websocket request %s %s", r.Method, r.URL.Path)
			http.Error(w, "websocket required", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("OpenAI-Beta"); got != openAIResponsesWebsocketBetaHeader {
			t.Errorf("OpenAI-Beta = %q", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "" {
			t.Errorf("Chatgpt-Account-Id = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		upgrades.Add(1)
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if gjson.GetBytes(payload, "type").String() != "response.create" || !gjson.GetBytes(payload, "stream").Bool() {
				t.Errorf("frame = %s", payload)
			}
			responseID := "resp_one"
			if prev := gjson.GetBytes(payload, "previous_response_id").String(); prev != "" {
				responseID = "resp_two"
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+responseID+`"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hi"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+responseID+`"}}`))
		}
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:                       41,
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    server.URL,
		APIKey:                     "sk-test",
		AccessToken:                "should-not-send",
		Models:                     []string{"gpt-5.5"},
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	first, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "response.completed") || !strings.Contains(string(first), "data: ") {
		t.Fatalf("sse = %s", first)
	}

	next := []byte(`{"model":"gpt-5.5","input":"next","previous_response_id":"resp_one"}`)
	resp, err = ExecuteOpenAIResponsesRequest(context.Background(), account, next, "", nil)
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	second, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "resp_two") {
		t.Fatalf("continuation sse = %s", second)
	}
	if got := upgrades.Load(); got != 1 {
		t.Fatalf("upgrades = %d, want 1 reused connection", got)
	}

	other := &auth.Account{
		DBID:                       42,
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    server.URL,
		APIKey:                     "sk-test",
		Models:                     []string{"gpt-5.5"},
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	resp, err = ExecuteOpenAIResponsesRequest(context.Background(), other, next, "", nil)
	if err != nil {
		t.Fatalf("other account: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := upgrades.Load(); got != 2 {
		t.Fatalf("upgrades = %d, want a separate connection for the other account", got)
	}
}

func TestExecuteOpenAIResponsesWebsocketHandshakeErrorDoesNotUseHTTP(t *testing.T) {
	resetOpenAIResponsesWebsocketPool()
	t.Cleanup(resetOpenAIResponsesWebsocketPool)

	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, `{"error":{"message":"websocket unsupported"}}`, http.StatusUnauthorized)
			return
		}
		posts.Add(1)
		http.Error(w, "http fallback", http.StatusBadGateway)
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:                       43,
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    server.URL,
		APIKey:                     "sk-test",
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", nil)
	if err != nil {
		t.Fatalf("handshake error was not returned as an upstream response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if posts.Load() != 0 {
		t.Fatalf("http posts = %d, want no HTTP fallback", posts.Load())
	}
	if !strings.Contains(string(body), "websocket unsupported") {
		t.Fatalf("body = %s", body)
	}
}

func TestOpenAIResponsesImageStaysOnHTTP(t *testing.T) {
	resetOpenAIResponsesWebsocketPool()
	t.Cleanup(resetOpenAIResponsesWebsocketPool)

	var sawUpgrade atomic.Bool
	var sawPost atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			sawUpgrade.Store(true)
			http.Error(w, "unexpected websocket", http.StatusBadGateway)
			return
		}
		sawPost.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_http","output":[]}`))
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:                       44,
		UpstreamType:               auth.UpstreamOpenAIResponses,
		BaseURL:                    server.URL,
		APIKey:                     "sk-test",
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket,
	}
	body := []byte(`{"model":"gpt-5.5","input":"draw","tool_choice":"image_generation"}`)
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("image request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, payload)
	}
	if sawUpgrade.Load() || !sawPost.Load() {
		t.Fatalf("upgrade=%v post=%v, image generation must stay on HTTP", sawUpgrade.Load(), sawPost.Load())
	}
}
