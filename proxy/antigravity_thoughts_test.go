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
	"github.com/tidwall/gjson"
)

func withAntigravityExposeThoughts(t *testing.T, enabled bool) {
	t.Helper()
	previous := auth.ConfiguredAntigravitySettings()
	next := previous
	next.ExposeThoughts = enabled
	auth.SetConfiguredAntigravitySettings(next)
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
}

func readAntigravityTestEvents(t *testing.T, body io.ReadCloser) []gjson.Result {
	t.Helper()
	defer body.Close()
	var events []gjson.Result
	if err := ReadSSEStream(body, func(data []byte) bool {
		events = append(events, gjson.ParseBytes(append([]byte(nil), data...)))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func antigravityTestThinkingConfig(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	cfg, _ := payload["request"].(map[string]any)["generationConfig"].(map[string]any)
	thinking, ok := cfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing thinkingConfig: %#v", payload["request"])
	}
	return thinking
}

const antigravityThoughtStream = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Let me think\",\"thought\":true}]}}]}}\n\n" +
	"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" more\",\"thought\":true,\"thoughtSignature\":\"sig\"}]}}]}}\n\n" +
	"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]}}]}}\n\n" +
	"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"late thought\",\"thought\":true},{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"q\":\"v\"},\"id\":\"call_1\"}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"thoughtsTokenCount\":4,\"totalTokenCount\":7}}}\n\n"

func TestAntigravityExposeThoughtsRequestIncludeThoughts(t *testing.T) {
	body := []byte(`{"input":"hello"}`)

	withAntigravityExposeThoughts(t, false)
	payload, err := responsesToGeminiInternal(body, "project", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := antigravityTestThinkingConfig(t, payload)["includeThoughts"]; exists {
		t.Fatal("includeThoughts must stay absent while the switch is off")
	}

	withAntigravityExposeThoughts(t, true)
	payload, err = responsesToGeminiInternal(body, "project", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	thinking := antigravityTestThinkingConfig(t, payload)
	if thinking["includeThoughts"] != true || thinking["thinkingLevel"] != "HIGH" {
		t.Fatalf("thinkingConfig = %#v, want HIGH with includeThoughts", thinking)
	}

	payload, err = responsesToGeminiInternal([]byte(`{"input":"hello","reasoning":{"summary":"none"}}`), "project", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := antigravityTestThinkingConfig(t, payload)["includeThoughts"]; exists {
		t.Fatal("reasoning.summary none must opt out of includeThoughts")
	}

	native := map[string]any{}
	antigravityApplyNativeGeminiThinkingConfig(native, "gemini-3.8-flash-high", "gemini-3.8-flash-tiered")
	nativeThinking := native["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if nativeThinking["includeThoughts"] != true {
		t.Fatalf("native injected thinkingConfig = %#v, want includeThoughts", nativeThinking)
	}
}

func TestAntigravitySSEExposesThoughtsAsReasoning(t *testing.T) {
	events := readAntigravityTestEvents(t, newAntigravitySSEResponseBodyWithThoughts(io.NopCloser(strings.NewReader(antigravityThoughtStream)), nil, true, "gemini-test"))

	var deltas []string
	var added, done []string
	reasoningDoneAt, messageAddedAt := -1, -1
	var completed gjson.Result
	for index, event := range events {
		switch event.Get("type").String() {
		case "response.reasoning_summary_text.delta":
			if event.Get("output_index").Int() != 0 || event.Get("summary_index").Int() != 0 {
				t.Fatalf("reasoning delta index = %s", event.Raw)
			}
			deltas = append(deltas, event.Get("delta").String())
		case "response.output_item.added":
			added = append(added, event.Get("item.type").String()+"@"+event.Get("output_index").String())
			if event.Get("item.type").String() == "message" {
				messageAddedAt = index
			}
		case "response.output_item.done":
			done = append(done, event.Get("item.type").String()+"@"+event.Get("output_index").String())
			if event.Get("item.type").String() == "reasoning" {
				reasoningDoneAt = index
			}
		case "response.output_text.delta":
			if event.Get("output_index").Int() != 1 {
				t.Fatalf("message delta index = %s", event.Raw)
			}
		case "response.completed":
			completed = event
		}
	}
	if strings.Join(deltas, "") != "Let me think more" {
		t.Fatalf("reasoning deltas = %q (late thoughts after text must not stream)", deltas)
	}
	if strings.Join(added, ",") != "reasoning@0,message@1,function_call@2" || strings.Join(done, ",") != "reasoning@0,message@1,function_call@2" {
		t.Fatalf("item order added=%v done=%v", added, done)
	}
	if reasoningDoneAt < 0 || messageAddedAt < reasoningDoneAt {
		t.Fatalf("reasoning item must close before the message opens (done=%d added=%d)", reasoningDoneAt, messageAddedAt)
	}
	output := completed.Get("response.output").Array()
	if len(output) != 3 || output[0].Get("type").String() != "reasoning" || output[0].Get("summary.0.text").String() != "Let me think more" || output[0].Get("encrypted_content").Exists() {
		t.Fatalf("completed output = %s", completed.Get("response.output").Raw)
	}
	if completed.Get("response.output_text").String() != "answer" || completed.Get("response.usage.output_tokens_details.reasoning_tokens").Int() != 4 {
		t.Fatalf("completed response = %s", completed.Raw)
	}
}

func TestAntigravitySSEHidesThoughtsWhenDisabled(t *testing.T) {
	events := readAntigravityTestEvents(t, newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(antigravityThoughtStream)), "gemini-test"))
	for _, event := range events {
		if strings.Contains(event.Get("type").String(), "reasoning") || event.Get("item.type").String() == "reasoning" {
			t.Fatalf("reasoning leaked while disabled: %s", event.Raw)
		}
		if event.Get("type").String() == "response.output_text.delta" && event.Get("output_index").Int() != 0 {
			t.Fatalf("message index shifted while disabled: %s", event.Raw)
		}
	}
}

func TestAntigravitySSEThoughtOnlyKeepsEmptyMessage(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"pondering\",\"thought\":true}]},\"finishReason\":\"STOP\"}]}\n\n"
	events := readAntigravityTestEvents(t, newAntigravitySSEResponseBodyWithThoughts(io.NopCloser(strings.NewReader(input)), nil, true, "gemini-test"))
	last := events[len(events)-1]
	output := last.Get("response.output").Array()
	if last.Get("type").String() != "response.completed" || len(output) != 2 || output[0].Get("type").String() != "reasoning" || output[1].Get("type").String() != "message" {
		t.Fatalf("terminal = %s", last.Raw)
	}
}

func TestAntigravitySSEThoughtsTranslateToChatReasoningContent(t *testing.T) {
	events := readAntigravityTestEvents(t, newAntigravitySSEResponseBodyWithThoughts(io.NopCloser(strings.NewReader(antigravityThoughtStream)), nil, true, "gemini-test"))
	var reasoning, content strings.Builder
	for _, event := range events {
		chunk, _ := TranslateStreamChunk([]byte(event.Raw), "gemini-test", "chatcmpl-test", 1)
		if len(chunk) == 0 {
			continue
		}
		reasoning.WriteString(gjson.GetBytes(chunk, "choices.0.delta.reasoning_content").String())
		if event.Get("type").String() == "response.output_text.delta" {
			content.WriteString(gjson.GetBytes(chunk, "choices.0.delta.content").String())
		}
	}
	if reasoning.String() != "Let me think more" || content.String() != "answer" {
		t.Fatalf("chat reasoning=%q content=%q", reasoning.String(), content.String())
	}
}

func TestAntigravityJSONExposesThoughtsAsReasoning(t *testing.T) {
	upstream := `{"response":{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true},{"text":"answer"}]},"finishReason":"STOP"}]}}`
	decode := func(exposeThoughts bool, raw string) map[string]any {
		t.Helper()
		body, err := newAntigravityJSONResponseBodyWithThoughts(io.NopCloser(strings.NewReader(raw)), "gemini-test", nil, exposeThoughts)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		var response map[string]any
		if err := json.NewDecoder(body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	output := decode(true, upstream)["output"].([]any)
	first := output[0].(map[string]any)
	summary := first["summary"].([]any)[0].(map[string]any)
	if len(output) != 2 || first["type"] != "reasoning" || summary["text"] != "thinking" {
		t.Fatalf("output = %#v", output)
	}
	if output := decode(false, upstream)["output"].([]any); len(output) != 1 || output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("disabled output = %#v", output)
	}
	failed := decode(true, `{"response":{"candidates":[{"content":{"parts":[{"text":"unsafe thinking","thought":true}]},"finishReason":"SAFETY"}]}}`)
	raw, _ := json.Marshal(failed)
	if failed["status"] != "failed" || strings.Contains(string(raw), "unsafe thinking") {
		t.Fatalf("failed response leaked thoughts: %s", raw)
	}
}

func TestAntigravityExecutorExposesThoughtsEndToEnd(t *testing.T) {
	withAntigravityExposeThoughts(t, true)
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, antigravityThoughtStream)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 7521, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.8-flash-high", []byte(`{"input":"hello"}`), true, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	thinking, _ := received["request"].(map[string]any)["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if thinking["includeThoughts"] != true {
		t.Fatalf("upstream thinkingConfig = %#v", thinking)
	}
	if !strings.Contains(string(body), `"type":"response.reasoning_summary_text.delta"`) || !strings.Contains(string(body), `"delta":"Let me think"`) {
		t.Fatalf("downstream stream lacks reasoning deltas: %s", body)
	}
}

func TestAntigravityBufferedExecutorReplaysUnstreamedAnswerAsSSE(t *testing.T) {
	withAntigravityExposeThoughts(t, true)
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		// generateContent answers with indented JSON; it must not break SSE framing.
		_, _ = io.WriteString(w, "{\n  \"response\": {\n    \"candidates\": [{\"content\": {\"parts\": [\n      {\"text\": \"Let me think\", \"thought\": true},\n      {\"text\": \"391\"}\n    ]}, \"finishReason\": \"STOP\"}],\n    \"usageMetadata\": {\"promptTokenCount\": 2, \"candidatesTokenCount\": 1, \"thoughtsTokenCount\": 4, \"totalTokenCount\": 7}\n  }\n}\n")
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 7522, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}}
	resp, err := ExecuteAntigravityResponsesRequestBuffered(context.Background(), account, "gemini-3.8-flash-high", []byte(`{"input":"hello"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSuffix(path, "?"), ":generateContent") {
		t.Fatalf("buffered call hit %q, want generateContent", path)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var reasoning, text string
	var completed bool
	for _, event := range readAntigravityTestEvents(t, resp.Body) {
		switch event.Get("type").String() {
		case "response.reasoning_summary_text.delta":
			reasoning += event.Get("delta").String()
		case "response.output_text.delta":
			text += event.Get("delta").String()
		case "response.completed":
			completed = true
		}
	}
	if reasoning != "Let me think" || text != "391" || !completed {
		t.Fatalf("replayed stream reasoning=%q text=%q completed=%v", reasoning, text, completed)
	}
}

func TestAntigravityBuffersUpstreamOnlyForNonStreamOAuthWithThoughts(t *testing.T) {
	oauth := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "token"}
	apiKey := &auth.Account{UpstreamType: auth.UpstreamAntigravity, APIKey: "key"}
	body := []byte(`{"input":"hi"}`)
	withAntigravityExposeThoughts(t, false)
	if AntigravityBuffersUpstream(oauth, false, body) {
		t.Fatal("buffered with thoughts disabled")
	}
	withAntigravityExposeThoughts(t, true)
	if !AntigravityBuffersUpstream(oauth, false, body) {
		t.Fatal("non-stream OAuth request with thoughts must buffer")
	}
	if AntigravityBuffersUpstream(oauth, true, body) {
		t.Fatal("streaming clients must keep upstream streaming")
	}
	if AntigravityBuffersUpstream(apiKey, false, body) {
		t.Fatal("API-key accounts must keep streaming")
	}
	if AntigravityBuffersUpstream(oauth, false, []byte(`{"input":"hi","reasoning":{"summary":"none"}}`)) {
		t.Fatal("reasoning.summary none opts out of buffering")
	}
}
