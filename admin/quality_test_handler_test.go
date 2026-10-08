package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func qualityTestRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/accounts/:id/quality-test/options", h.QualityTestOptions)
	router.POST("/accounts/:id/quality-test", h.QualityTest)
	return router
}

func qualityTestHTTPRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/accounts/42/quality-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestQualityTestPinsAccountAndPreservesPromptEffortAndHTML(t *testing.T) {
	const html = "<!doctype html><html><body><svg>\n  <text>鹈鹕</text>\n</svg></body></html>"
	const prompt = "创建 HTML\n第二行必须保留\n{{random}}"
	var gotBody []byte
	var gotAuthorization string
	h, _, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, text := range []string{"<!doctype html><html><body><svg>", "\n  ", "<text>鹈鹕</text>\n</svg></body></html>"} {
			event, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"actual-model\",\"usage\":{\"input_tokens\":24,\"output_tokens\":88,\"output_tokens_details\":{\"reasoning_tokens\":40}}}}\n\n")
	})
	h.store.AddAccount(&auth.Account{DBID: 43, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "other-account", BaseURL: "http://127.0.0.1:1", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady})
	requestBody, _ := json.Marshal(qualityTestRequest{Model: "gpt-4o-mini", Prompt: prompt, ReasoningEffort: "high"})
	recorder := httptest.NewRecorder()
	qualityTestRouter(h).ServeHTTP(recorder, qualityTestHTTPRequest(string(requestBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotAuthorization != "Bearer sk-test-secret" || gjson.GetBytes(gotBody, "model").String() != "gpt-4o-mini" {
		t.Fatal("request did not use exactly the selected account/model")
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); got != prompt {
		t.Fatalf("multiline prompt altered: %q", got)
	}
	if gjson.GetBytes(gotBody, "reasoning.effort").String() != "high" || strings.Contains(string(gotBody), "Reply briefly") {
		t.Fatalf("wrong quality-test instructions or effort: %s", gotBody)
	}
	var output strings.Builder
	completed := false
	var diagnostics *codexTestDiagnostics
	for _, event := range decodeCodexTestEvents(t, recorder.Body.String()) {
		if event.Type == "content" {
			output.WriteString(event.Text)
		}
		if event.Type == "test_complete" {
			completed = event.Success
		}
		if event.CodexDiagnostics != nil {
			diagnostics = event.CodexDiagnostics
		}
	}
	if output.String() != html || !completed {
		t.Fatalf("HTML corrupted or missing completion: output=%q completed=%v", output.String(), completed)
	}
	if diagnostics == nil || diagnostics.ResponseModel != "actual-model" || diagnostics.Usage == nil || diagnostics.Usage.ReasoningTokens == nil || *diagnostics.Usage.ReasoningTokens != 40 {
		t.Fatalf("missing final metrics: %+v", diagnostics)
	}
}

func TestQualityTestRejectsInvalidInputBeforeUpstream(t *testing.T) {
	h, _, _ := newCodexDiagnosticsTestHandler(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached upstream") })
	for _, body := range []string{
		`{`,
		`{"model":"gpt-4o-mini","prompt":" "}`,
		`{"model":"other-model","prompt":"HTML"}`,
		`{"model":"gpt-4o-mini","prompt":"HTML","reasoning_effort":"invalid"}`,
		`{"model":"gpt-4o-mini","prompt":"` + strings.Repeat("x", qualityTestPromptLimit+1) + `"}`,
	} {
		recorder := httptest.NewRecorder()
		qualityTestRouter(h).ServeHTTP(recorder, qualityTestHTTPRequest(body))
		if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
			t.Fatalf("invalid request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestQualityTestCancellationReachesSelectedUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	h, _, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		qualityTestRouter(h).ServeHTTP(recorder, qualityTestHTTPRequest(`{"model":"gpt-4o-mini","prompt":"HTML"}`).WithContext(ctx))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not stop")
	}
	for _, event := range decodeCodexTestEvents(t, recorder.Body.String()) {
		if event.Type == "test_complete" && event.Success {
			t.Fatal("cancelled stream reported success")
		}
	}
}

func TestQualityTestChannelPayloadsAndOptions(t *testing.T) {
	h := &Handler{store: auth.NewStore(nil, nil, nil)}
	codex := &auth.Account{Models: []string{"gpt-5.5", "gpt-image-1", "gpt-5.5(high)"}}
	options := h.qualityTestOptionsForAccount(context.Background(), codex)
	if len(options.Models) != 1 || options.Models[0] != "gpt-5.5" {
		t.Fatalf("invalid options: %+v", options)
	}
	ag := newAntigravityConnectionTestAccount()
	if err := h.validateQualityTestForAccount(context.Background(), ag, qualityTestRequest{Model: "gemini-3.6-flash-low", ReasoningEffort: "high"}); err == nil {
		t.Fatal("fixed-tier models must reject a conflicting effort")
	}
	claude := &auth.Account{UpstreamType: auth.UpstreamClaude}
	securityCfg := auth.DefaultClaudeSecurityConfig()
	securityCfg.MaxOutputTokens = 12000
	body, err := buildQualityTestPayload(claude, "claude-opus-4-6", qualityTestRequest{Prompt: "line 1\nline 2", ReasoningEffort: "high"}, securityCfg)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "messages.0.content").String() != "line 1\nline 2" || gjson.GetBytes(body, "max_tokens").Int() != 12000 || gjson.GetBytes(body, "output_config.effort").String() != "high" || gjson.GetBytes(body, "thinking.type").String() != "adaptive" || gjson.GetBytes(body, "input").Exists() {
		t.Fatalf("invalid native Messages payload: %s", body)
	}
	body, err = buildQualityTestPayload(codex, "gpt-5.5", qualityTestRequest{Prompt: "HTML"}, securityCfg)
	if err != nil || gjson.GetBytes(body, "reasoning").Exists() || gjson.GetBytes(body, "store").Bool() {
		t.Fatalf("default effort/store behavior changed: %s", body)
	}
}

func TestQualityTestClaudePreservesWhitespaceDeltas(t *testing.T) {
	stream := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"<svg>\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"\\n  \"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"</svg>\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	var output strings.Builder
	status, detail := readClaudeMessagesStreamObserved(context.Background(), resp, func(text string) { output.WriteString(text) }, nil, true)
	if status != "success" || output.String() != "<svg>\n  </svg>" {
		t.Fatalf("status=%s detail=%s output=%q", status, detail, output.String())
	}
}

func TestQualityTestPreviewUsesAnIndependentOpaqueSandbox(t *testing.T) {
	router := gin.New()
	router.GET("/preview", serveQualityTestPreview)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/preview?html=must-not-be-reflected", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"sandbox allow-scripts", "frame-ancestors 'self'", "connect-src 'none'", "script-src 'unsafe-inline'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("missing preview policy %q: %s", directive, policy)
		}
	}
	if strings.Contains(policy, "allow-same-origin") || strings.Contains(recorder.Body.String(), "must-not-be-reflected") || recorder.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatal("preview must remain isolated and never reflect request data")
	}
}
