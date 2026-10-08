package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestModelDetectorSupportsClaudeChannel(t *testing.T) {
	var requests int
	var seenPath, seenKey, seenModel, seenPrompt string
	numbers := make([]string, 220)
	for index := range numbers {
		numbers[index] = strconv.Itoa(index%355 + 1)
	}
	answer := strings.Join(numbers, ",")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		seenPath = r.URL.Path
		seenKey = r.Header.Get("X-Api-Key")
		body, _ := io.ReadAll(r.Body)
		seenModel = gjson.GetBytes(body, "model").String()
		seenPrompt = gjson.GetBytes(body, "messages.0.content").String()
		if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "max_tokens").Int() <= 0 {
			t.Errorf("invalid Claude detector payload: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":"+strconv.Quote(answer)+"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	account := &auth.Account{
		DBID: 7, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey,
		AccessToken: "claude-api-key", ClaudeBaseURL: upstream.URL, Models: []string{"claude-sonnet-4-5"},
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(account)
	handler := &Handler{store: store}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/7/model-detector?model=claude-sonnet-4-5", nil)
	handler.DetectCodexModel(c)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"complete"`) || !strings.Contains(recorder.Body.String(), `"report"`) {
		t.Fatalf("Claude detector response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if requests != proxy.ModelTraceTargetOutputs || seenPath != "/v1/messages" || seenKey != "claude-api-key" || seenModel != "claude-sonnet-4-5" {
		t.Fatalf("requests=%d path=%q key=%q model=%q", requests, seenPath, seenKey, seenModel)
	}
	if !strings.Contains(seenPrompt, "1 到 355") {
		t.Fatalf("Claude detector did not send ModelTrace challenge: %q", seenPrompt)
	}
}

func TestCodexModelDetectorSupportsOpenAIResponsesAPIAccount(t *testing.T) {
	var seenPath, seenAuth, seenModel string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = body
		seenModel = strings.TrimSpace(gjsonString(body, "model"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"3\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()

	account := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: upstream.URL, APIKey: "relay-key", Models: []string{"gpt-api-codex"},
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(account)
	detector, err := proxy.NewModelTraceDetector()
	if err != nil {
		t.Fatalf("NewModelTraceDetector() error = %v", err)
	}
	challenges, err := detector.GenerateChallenges(1)
	if err != nil || len(challenges) != 1 {
		t.Fatalf("GenerateChallenges(1) = %d items, err %v", len(challenges), err)
	}
	handler := &Handler{store: store}
	if _, err := handler.executeModelDetectorProbe(context.Background(), account, "gpt-api-codex", challenges[0]); err != nil {
		t.Fatalf("executeModelDetectorProbe() error = %v", err)
	}
	if seenPath != "/v1/responses" || seenAuth != "Bearer relay-key" || seenModel != "gpt-api-codex" {
		t.Fatalf("upstream request path=%q auth=%q model=%q", seenPath, seenAuth, seenModel)
	}
	if role := gjson.GetBytes(seenBody, "input.0.role").String(); role != "user" {
		t.Fatalf("upstream role = %q, want user", role)
	}
	if gjson.GetBytes(seenBody, "reasoning").Exists() || gjson.GetBytes(seenBody, "instructions").Exists() || gjson.GetBytes(seenBody, "max_output_tokens").Exists() {
		t.Fatalf("ModelTrace request unexpectedly overrides provider defaults: %s", seenBody)
	}
}

func TestParseCodexDetectorConcurrency(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: 1},
		{raw: " 2 ", want: 2},
		{raw: "3", want: 3},
		{raw: "0", wantErr: true},
		{raw: "4", wantErr: true},
		{raw: "1.5", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseCodexDetectorConcurrency(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseCodexDetectorConcurrency(%q) = %d, want error", test.raw, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("parseCodexDetectorConcurrency(%q) = %d, %v; want %d", test.raw, got, err, test.want)
		}
	}
}

func TestParseCodexDetectorSamples(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: proxy.ModelTraceTargetOutputs},
		{raw: " 1 ", want: 1},
		{raw: "2", want: 2},
		{raw: "3", want: 3},
		{raw: "0", wantErr: true},
		{raw: "4", wantErr: true},
		{raw: "x", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseCodexDetectorSamples(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseCodexDetectorSamples(%q) = %d, want error", test.raw, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("parseCodexDetectorSamples(%q) = %d, %v; want %d", test.raw, got, err, test.want)
		}
	}
}

func TestModelDetectorSingleSampleSendsOneProbe(t *testing.T) {
	var requests int
	numbers := make([]string, 220)
	for index := range numbers {
		numbers[index] = strconv.Itoa(index%355 + 1)
	}
	answer := strings.Join(numbers, ",")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":"+strconv.Quote(answer)+"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	account := &auth.Account{
		DBID: 8, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey,
		AccessToken: "claude-api-key", ClaudeBaseURL: upstream.URL, Models: []string{"claude-sonnet-4-5"},
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(account)
	handler := &Handler{store: store}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: "8"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/8/model-detector?model=claude-sonnet-4-5&samples=1", nil)
	handler.DetectCodexModel(c)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || requests != 1 {
		t.Fatalf("single-sample detector status=%d requests=%d body=%s", recorder.Code, requests, body)
	}
	if !strings.Contains(body, `"total":1,"max_attempts":2`) || !strings.Contains(body, `"calibration":{"queries":1`) {
		t.Fatalf("single-sample detector did not report a 1-sample run: %s", body)
	}
}

func gjsonString(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}
