package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestGeminiNativeToAntigravityEnvelope(t *testing.T) {
	payload, wireModel, _, err := geminiNativeToAntigravityEnvelope([]byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"system_instruction":{"parts":[{"text":"Hermes Agent helper"}]},
		"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["missing"]}}]}],
		"generationConfig":{"temperature":0.2,"maxOutputTokens":128}
	}`), "google-project", "models/gemini-3.7-flash-high")
	if err != nil {
		t.Fatalf("geminiNativeToAntigravityEnvelope: %v", err)
	}
	if wireModel == "" {
		t.Fatal("wire model must not be empty")
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope["project"] != "google-project" {
		t.Fatalf("project = %#v", envelope["project"])
	}
	request, _ := envelope["request"].(map[string]any)
	if request == nil {
		t.Fatal("request must be present")
	}
	if _, ok := request["system_instruction"]; ok {
		t.Fatal("system_instruction must be renamed to systemInstruction")
	}
	systemInstruction, _ := request["systemInstruction"].(map[string]any)
	parts, _ := systemInstruction["parts"].([]any)
	part0, _ := parts[0].(map[string]any)
	if text, _ := part0["text"].(string); !strings.Contains(text, "\u200B") {
		t.Fatalf("system instruction must be obfuscated: %q", text)
	}
	tools, _ := request["tools"].([]any)
	tool0, _ := tools[0].(map[string]any)
	declarations, _ := tool0["functionDeclarations"].([]any)
	decl0, _ := declarations[0].(map[string]any)
	schema, _ := decl0["parametersJsonSchema"].(map[string]any)
	if required, _ := schema["required"].([]any); len(required) != 0 {
		t.Fatalf("orphan required must be dropped: %#v", required)
	}
	genConfig, _ := request["generationConfig"].(map[string]any)
	if _, ok := genConfig["maxOutputTokens"]; ok {
		t.Fatalf("Gemini wire models must omit maxOutputTokens: %#v", genConfig)
	}
}

func TestAntigravityFixGeminiToolResponseGrouping(t *testing.T) {
	request := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"functionCall": map[string]any{"name": "lookup"}},
				},
			},
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "ok"}}},
				},
			},
		},
	}
	if err := antigravityFixGeminiToolResponseGrouping(request); err != nil {
		t.Fatalf("group tool responses: %v", err)
	}
	contents := request["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents len = %d, want 2", len(contents))
	}
	group, _ := contents[1].(map[string]any)
	if group["role"] != "function" {
		t.Fatalf("group role = %#v", group["role"])
	}
}

func TestUnwrapAntigravityNativeGeminiChunk(t *testing.T) {
	got := unwrapAntigravityNativeGeminiChunk([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":3}}}`))
	if !strings.Contains(string(got), `"text":"hi"`) {
		t.Fatalf("unexpected chunk: %s", got)
	}
}

func TestAntigravityApplyNativeGeminiThinkingConfigRespectsClientThinking(t *testing.T) {
	request := map[string]any{
		"generationConfig": map[string]any{
			"thinkingConfig": map[string]any{"thinkingLevel": "LOW"},
		},
	}
	antigravityApplyNativeGeminiThinkingConfig(request, "gemini-3.7-flash-high", "gemini-3.7-flash-tiered")
	genConfig := request["generationConfig"].(map[string]any)
	thinking := genConfig["thinkingConfig"].(map[string]any)
	if thinking["thinkingLevel"] != "LOW" {
		t.Fatalf("client thinkingConfig overwritten: %#v", thinking)
	}
}

func TestAntigravityApplyNativeGeminiThinkingConfigInjectsWhenMissing(t *testing.T) {
	request := map[string]any{"generationConfig": map[string]any{"temperature": 0.2}}
	antigravityApplyNativeGeminiThinkingConfig(request, "gemini-3.7-flash-high", "gemini-3.7-flash-tiered")
	genConfig := request["generationConfig"].(map[string]any)
	thinking, ok := genConfig["thinkingConfig"].(map[string]any)
	if !ok || len(thinking) == 0 {
		t.Fatalf("expected injected thinkingConfig, got %#v", genConfig)
	}
}

func TestAntigravitySanitizeNativeGeminiThoughtSignaturesStubsFunctionCall(t *testing.T) {
	request := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{"q": "x"}}},
				},
			},
		},
	}
	antigravitySanitizeNativeGeminiThoughtSignatures(request, "gemini-3.7-flash-high")
	part := request["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != antigravityGeminiSkipThoughtSignature {
		t.Fatalf("functionCall missing skip stub: %#v", part)
	}
}

func TestAntigravitySanitizeNativeGeminiThoughtSignaturesStripsFunctionResponse(t *testing.T) {
	request := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "function",
				"parts": []any{
					map[string]any{
						"thoughtSignature": "stale",
						"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "ok"}},
					},
				},
			},
		},
	}
	antigravitySanitizeNativeGeminiThoughtSignatures(request, "gemini-3.7-flash-high")
	part := request["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if _, ok := part["thoughtSignature"]; ok {
		t.Fatalf("functionResponse must not replay thoughtSignature: %#v", part)
	}
}

func TestExecuteAntigravityResponsesRequestKeepsPublicModelIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 9003, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.8-flash-high", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "gemini-3.8-flash-high") {
		t.Fatalf("response model identity lost: %s", body)
	}
}

func TestExecuteAntigravityGeminiRequestNonStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`))
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 9001, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityGeminiRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`), false, "")
	if err != nil {
		t.Fatalf("ExecuteAntigravityGeminiRequest: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"text":"OK"`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(string(body), `"response"`) {
		t.Fatalf("native Gemini body must not keep envelope: %s", body)
	}
}

func TestExecuteAntigravityGeminiCountTokensRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:countTokens" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"contents"`) {
			t.Fatalf("countTokens body missing contents: %s", body)
		}
		if strings.Contains(string(body), `"project"`) || strings.Contains(string(body), `"sessionId"`) || strings.Contains(string(body), `"requestId"`) {
			t.Fatalf("countTokens body must strip envelope metadata: %s", body)
		}
		if !strings.Contains(string(body), `"request"`) {
			t.Fatalf("countTokens body missing request wrapper: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"totalTokens":128}}`))
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 9011, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityGeminiCountTokensRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"contents":[{"role":"model","parts":[{"text":"hi"}]}]}`), "")
	if err != nil {
		t.Fatalf("ExecuteAntigravityGeminiCountTokensRequest: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"totalTokens":128`) {
		t.Fatalf("body = %s", body)
	}
}

func TestExecuteAntigravityGeminiRequestRejectsAPIKeyAccount(t *testing.T) {
	account := &auth.Account{DBID: 9002, UpstreamType: auth.UpstreamAntigravity, APIKey: "google-key"}
	_, err := ExecuteAntigravityGeminiRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`), false, "")
	if err == nil || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("expected OAuth-only error, got %v", err)
	}
}

func TestAntigravityNativeGeminiSSEBodyUnwrapsChunks(t *testing.T) {
	input := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"STOP\"}]}}\n\ndata: [DONE]\n\n"
	body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(strings.NewReader(input)), nil)
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(out), `"text":"partial"`) {
		t.Fatalf("stream = %s", out)
	}
	if strings.Contains(string(out), `"response"`) {
		t.Fatalf("stream must unwrap response envelope: %s", out)
	}
}

func TestGeminiListModelsReturnsNativeShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID: 9010, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "project-id",
		Models: []string{"gemini-3.7-flash-tiered", "gemini-3.8-flash-tiered"},
	})
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyRow, row)
		c.Next()
	})
	router.GET("/v1beta/models", handler.GeminiListModels)
	router.GET("/v1beta/models/*action", handler.GeminiGetModel)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"models/gemini-3.7-flash-high"`) {
		t.Fatalf("list body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-3.7-flash-high", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"supportedGenerationMethods"`) {
		t.Fatalf("get body = %s", rec.Body.String())
	}
}
