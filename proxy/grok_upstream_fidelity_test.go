package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestApplyGrokRequestHeadersAlignsOfficialCLI(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at"}
	applyGrokRequestHeaders(req, account, "tok", nil, nil)
	checks := map[string]string{
		"Accept":                        "text/event-stream",
		"x-grok-doom-loop-check":        "1024",
		"x-compactions-remaining":       "1",
		"x-grok-client-identifier":      grokClientIdentifier,
		"x-grok-client-version":         grokClientVersion,
		"x-grok-exact-repetition-check": "64",
	}
	for key, want := range checks {
		if got := req.Header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	// 官方 CLI 的 agent-id / req-id 都是带连字符的 UUID，traceparent 每请求必带。
	for _, key := range []string{"x-grok-agent-id", "x-grok-req-id"} {
		if _, err := uuid.Parse(req.Header.Get(key)); err != nil || len(req.Header.Get(key)) != 36 {
			t.Fatalf("%s = %q, want hyphenated UUID", key, req.Header.Get(key))
		}
	}
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(req.Header.Get("traceparent")) {
		t.Fatalf("traceparent = %q", req.Header.Get("traceparent"))
	}
	ua := req.Header.Get("User-Agent")
	if strings.Contains(ua, "arm64") || strings.Contains(ua, "amd64") || !strings.Contains(ua, "/"+grokClientVersion+" ") {
		t.Fatalf("User-Agent = %q, want Rust arch names and client version", ua)
	}
}

func TestEnsureGrokPromptCacheKeyMatchesSession(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Session-Id", "sess-stable")
	body := []byte(`{"model":"grok-4.7","input":"hi"}`)
	got := ensureGrokPromptCacheKey(body, headers, body)
	if gjson.GetBytes(got, "prompt_cache_key").String() != "sess-stable" {
		t.Fatalf("prompt_cache_key = %s", got)
	}
	again := ensureGrokPromptCacheKey(got, headers, body)
	if gjson.GetBytes(again, "prompt_cache_key").String() != "sess-stable" {
		t.Fatalf("existing key rewritten: %s", again)
	}

	explicit := []byte(`{"model":"grok-4.7","prompt_cache_key":"memory-capture-1","input":"hi"}`)
	kept := ensureGrokPromptCacheKey(explicit, headers, explicit)
	if gjson.GetBytes(kept, "prompt_cache_key").String() != "memory-capture-1" {
		t.Fatalf("explicit side key overwritten: %s", kept)
	}
}

func TestCompressGrokRequestBodyMatchesCLIThreshold(t *testing.T) {
	t.Setenv("GROK_REQUEST_COMPRESSION", "")
	oauth := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at"}
	apiKey := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "sk"}
	small := bytes.Repeat([]byte("a"), 4<<10)
	large := bytes.Repeat([]byte("a"), grokRequestCompressionMinBytes+1024)

	if _, encoding := compressGrokRequestBody(oauth, small); encoding != "" {
		t.Fatalf("small oauth body encoding = %q", encoding)
	}
	compressed, encoding := compressGrokRequestBody(oauth, large)
	if encoding != "zstd" || len(compressed) == 0 || len(compressed) >= len(large) {
		t.Fatalf("large oauth body encoding=%q len=%d plain=%d", encoding, len(compressed), len(large))
	}
	if _, encoding := compressGrokRequestBody(apiKey, large); encoding != "" {
		t.Fatalf("api key body encoding = %q, want plain", encoding)
	}

	t.Setenv("GROK_REQUEST_COMPRESSION", "off")
	if _, encoding := compressGrokRequestBody(oauth, large); encoding != "" {
		t.Fatalf("disabled compression still encoded: %q", encoding)
	}
	t.Setenv("GROK_REQUEST_COMPRESSION", "zstd")
	if _, encoding := compressGrokRequestBody(apiKey, large); encoding != "zstd" {
		t.Fatalf("forced compression encoding = %q", encoding)
	}
}

func TestExecuteGrokProtocolRequestPinsCacheKeyAndCompresses(t *testing.T) {
	t.Setenv("GROK_REQUEST_COMPRESSION", "")
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody = readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: server.URL + "/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{
		ModelID: "grok-4.5", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolResponses,
	}}})
	payload, err := json.Marshal(map[string]any{
		"model":  "grok-4.5",
		"stream": true,
		"input":  strings.Repeat("a", grokRequestCompressionMinBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Set("Session-Id", "sess-har")
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolResponses, payload, payload, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHeader.Get("Content-Encoding") != "zstd" {
		t.Fatalf("Content-Encoding = %q", gotHeader.Get("Content-Encoding"))
	}
	if gotHeader.Get("x-grok-session-id") != "sess-har" {
		t.Fatalf("session = %q", gotHeader.Get("x-grok-session-id"))
	}
	if gotHeader.Get("x-grok-client-version") != "1.0.46" && os.Getenv("GROK_CLIENT_VERSION") == "" {
		t.Fatalf("client version = %q", gotHeader.Get("x-grok-client-version"))
	}
	if gjson.GetBytes(gotBody, "prompt_cache_key").String() != "sess-har" {
		t.Fatalf("prompt_cache_key = %s", gotBody)
	}
	if gotHeader.Get("x-compaction-at") != "400000" && os.Getenv("GROK_COMPACTION_AT") == "" {
		t.Fatalf("x-compaction-at = %q", gotHeader.Get("x-compaction-at"))
	}
}

func TestGrokClientVersionDefaultNotOutdated(t *testing.T) {
	// 上游对 1.0.13 以下的 CLI 版本回 426；默认值回退到 0.x 会让全部 Grok 请求失败。
	if os.Getenv("GROK_CLIENT_VERSION") != "" {
		t.Skip("GROK_CLIENT_VERSION overridden")
	}
	if strings.HasPrefix(grokClientVersion, "0.") {
		t.Fatalf("grokClientVersion = %q is below the upstream minimum", grokClientVersion)
	}
	if grokClientVersion != "1.0.46" {
		t.Fatalf("grokClientVersion = %q, want 1.0.46", grokClientVersion)
	}
}

func TestResolveGrokConversationIDStableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{
		"model":"grok-4.6",
		"system":[{"type":"text","text":"CLAUDE.md project rules"}],
		"messages":[{"role":"user","content":"分析一下这个是什么项目呢"}]
	}`)
	turn2 := []byte(`{
		"model":"grok-4.6",
		"system":[{"type":"text","text":"CLAUDE.md project rules"}],
		"messages":[
			{"role":"user","content":"分析一下这个是什么项目呢"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)
	got1 := resolveGrokConversationID(nil, turn1)
	got2 := resolveGrokConversationID(nil, turn2)
	if got1 == "" || got1 != got2 {
		t.Fatalf("conversation id must stay stable across tool turns: %q vs %q", got1, got2)
	}
	other := resolveGrokConversationID(nil, []byte(`{
		"model":"grok-4.6",
		"system":[{"type":"text","text":"CLAUDE.md project rules"}],
		"messages":[{"role":"user","content":"换一个完全不同的问题"}]
	}`))
	if other == "" || other == got1 {
		t.Fatalf("different first user must not share conv id: %q", other)
	}

	dynamic1 := []byte(`{
		"model":"grok-4.6",
		"system":[
			{"type":"text","text":"CLAUDE.md project rules","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"git dirty at 00:01"}
		],
		"messages":[{"role":"user","content":"分析一下这个是什么项目呢"}]
	}`)
	dynamic2 := []byte(`{
		"model":"grok-4.6",
		"system":[
			{"type":"text","text":"CLAUDE.md project rules","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"git dirty at 00:03"}
		],
		"messages":[{"role":"user","content":"分析一下这个是什么项目呢"}]
	}`)
	if got := resolveGrokConversationID(nil, dynamic1); got == "" || got != resolveGrokConversationID(nil, dynamic2) {
		t.Fatalf("dynamic uncached system must not drift conv id: %q vs %q", resolveGrokConversationID(nil, dynamic1), resolveGrokConversationID(nil, dynamic2))
	}

	headers := make(http.Header)
	headers.Set("Session-Id", "claude-session-stable")
	headers.Set("Idempotency-Key", "per-request-"+got1)
	if got := resolveGrokConversationID(headers, turn2); got != "claude-session-stable" {
		t.Fatalf("Session-Id should win, got %q", got)
	}
	if got := resolveGrokConversationID(http.Header{"Idempotency-Key": []string{"only-once"}}, turn1); got == "only-once" {
		t.Fatal("Idempotency-Key must not become the Grok conversation id")
	}

	keyHeaders := make(http.Header)
	keyHeaders.Set("X-Api-Key", "sk-test-shared")
	empty1 := resolveGrokConversationID(keyHeaders, nil)
	empty2 := resolveGrokConversationID(keyHeaders, nil)
	if empty1 == "" || empty1 != empty2 {
		t.Fatalf("API key fallback must be deterministic: %q vs %q", empty1, empty2)
	}
}

func TestApplyGrokRequestHeadersReusesConversationID(t *testing.T) {
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at"}
	body := []byte(`{"model":"grok-4.6","system":"rules","messages":[{"role":"user","content":"hello"}]}`)
	req1, _ := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
	req2, _ := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
	applyGrokRequestHeaders(req1, account, "tok", nil, body)
	applyGrokRequestHeaders(req2, account, "tok", nil, body)
	if req1.Header.Get("x-grok-session-id") == "" || req1.Header.Get("x-grok-session-id") != req2.Header.Get("x-grok-session-id") {
		t.Fatalf("session-id %q vs %q", req1.Header.Get("x-grok-session-id"), req2.Header.Get("x-grok-session-id"))
	}
	if req1.Header.Get("x-grok-session-id") != req1.Header.Get("x-grok-conv-id") {
		t.Fatalf("session-id and conv-id should match, got %q / %q", req1.Header.Get("x-grok-session-id"), req1.Header.Get("x-grok-conv-id"))
	}
	if group := req1.Header.Get("x-grok-conv-group-id"); group == "" || group != req2.Header.Get("x-grok-conv-group-id") {
		t.Fatalf("conv-group-id must stay stable with the session, got %q vs %q", group, req2.Header.Get("x-grok-conv-group-id"))
	}
	if req1.Header.Get("x-grok-req-id") == req2.Header.Get("x-grok-req-id") {
		t.Fatal("req-id must stay unique per request")
	}
}

func TestGrokTurnIndex(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"string input", `{"input":"hello"}`, 1},
		{"one user turn", `{"input":[{"type":"message","role":"developer","content":[]},{"type":"message","role":"user","content":[]}]}`, 1},
		{"three user turns", `{"input":[{"type":"message","role":"user"},{"type":"reasoning"},{"type":"message","role":"assistant"},{"type":"message","role":"user"},{"type":"message","role":"user"}]}`, 3},
		{"no user messages", `{"input":[{"type":"reasoning"}]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokTurnIndex([]byte(tc.body)); got != tc.want {
				t.Fatalf("grokTurnIndex = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGrokBlobDetection(t *testing.T) {
	if !grokBodyHasBlobs([]byte(`{"input":[{"type":"reasoning","encrypted_content":"x"}]}`)) {
		t.Fatal("should detect encrypted_content")
	}
	if !grokBodyHasBlobs([]byte(`{"input":[{"type":"compaction"}]}`)) {
		t.Fatal("should detect compaction")
	}
	if grokBodyHasBlobs([]byte(`{"input":[{"type":"message"}]}`)) {
		t.Fatal("should not detect blobs in plain body")
	}

	if !grokIsBlobDecodeFailure([]byte(`{"code":"400","error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`)) {
		t.Fatal("should recognize compaction blob decode failure")
	}
	if !grokIsBlobDecodeFailure([]byte(`{"error":"could not decrypt the provided encrypted_content"}`)) {
		t.Fatal("should recognize decrypt failure")
	}
	if grokIsBlobDecodeFailure([]byte(`{"error":"Argument not supported: external_web_access"}`)) {
		t.Fatal("unrelated 400 should not be treated as blob decode failure")
	}
}

func TestDecodeContentEncoding(t *testing.T) {
	original := []byte(`{"hello":"world","n":42}`)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(original)
	_ = gw.Close()
	if out, err := decodeContentEncoding(gz.Bytes(), "gzip"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("gzip decode failed: %v out=%s", err, out)
	}

	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write(original)
	_ = bw.Close()
	if out, err := decodeContentEncoding(br.Bytes(), "br"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("br decode failed: %v out=%s", err, out)
	}

	// identity / 空编码原样返回。
	if out, err := decodeContentEncoding(original, "identity"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("identity should pass through: %v", err)
	}
}
