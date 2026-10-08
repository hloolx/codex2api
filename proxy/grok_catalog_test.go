package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
)

func TestParseGrokModelCatalogAuthoritativeEmpty(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"data":[]}`), []byte(`{"models":[]}`), []byte(`[]`)} {
		models, err := ParseGrokModelCatalog(body, false)
		if err != nil || len(models) != 0 {
			t.Fatalf("ParseGrokModelCatalog(%s) = %#v, %v; want authoritative empty", body, models, err)
		}
	}
	if _, err := ParseGrokModelCatalog([]byte(`{"object":"list"}`), false); err == nil {
		t.Fatal("catalog without a data/models array was accepted")
	}
}

func TestParseGrokModelCatalogRichMetadata(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"ignored-id","model":"grok-4.5","name":"Grok 4.5","description":"latest","baseUrl":"https://session.example/v1","api_base_url":"https://api.example/v1","contextWindow":500000,"max_completion_tokens":32768,"apiBackend":"responses","reasoningEffort":"high","supports_reasoning_effort":true,"reasoningEfforts":["low",{"id":"high","label":"High"}],"supportsBackendSearch":true,"stream_tool_calls":true,"supportedInApi":false,"hidden":false,"extraHeaders":{"anthropic-version":"2023-06-01","Authorization":"secret","x-safe":"ok"}},
		{"_meta":{"modelId":"meta-model","totalContextTokens":12345,"apiBackend":"messages"},"hidden":true},
		"string-model"
	]}`)

	got, err := ParseGrokModelCatalog(body, false)
	if err != nil {
		t.Fatalf("ParseGrokModelCatalog: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("models len = %d, want 3: %#v", len(got), got)
	}
	model := got[0]
	if model.ID != "grok-4.5" || model.APIBackend != GrokProtocolResponses {
		t.Fatalf("identity/backend = %q/%q", model.ID, model.APIBackend)
	}
	if model.ContextWindow != 500000 || model.MaxCompletionTokens != 32768 {
		t.Fatalf("limits = %d/%d", model.ContextWindow, model.MaxCompletionTokens)
	}
	if model.ExtraHeaders.Get("Authorization") != "" || model.ExtraHeaders.Get("x-safe") != "ok" {
		t.Fatalf("extra headers not safely filtered: %#v", model.ExtraHeaders)
	}
	if model.SupportedInAPI == nil || *model.SupportedInAPI {
		t.Fatalf("supportedInApi false presence lost: %#v", model.SupportedInAPI)
	}
	if got[1].ID != "meta-model" || got[1].APIBackend != GrokProtocolMessages || got[1].ContextWindow != 12345 {
		t.Fatalf("_meta fallback failed: %#v", got[1])
	}
	if got[2].ID != "string-model" || got[2].ContextWindow != defaultGrokCatalogContextWindow {
		t.Fatalf("string fallback failed: %#v", got[2])
	}

	visibleOAuth := VisibleGrokModelIDs(got, auth.GrokAuthKindOAuth)
	if len(visibleOAuth) != 2 || visibleOAuth[0] != "grok-4.5" || visibleOAuth[1] != "string-model" {
		t.Fatalf("oauth visible = %v", visibleOAuth)
	}
	visibleAPI := VisibleGrokModelIDs(got, auth.GrokAuthKindAPIKey)
	if len(visibleAPI) != 1 || visibleAPI[0] != "string-model" {
		t.Fatalf("api visible = %v", visibleAPI)
	}
}

func TestParseGrokReasoningMenuPrefersWireValue(t *testing.T) {
	body := []byte(`{"data":[{"model":"grok-4.7","reasoning_effort":"high","reasoning_efforts":[
		{"id":"extra","value":"xhigh","label":"Extra High","default":true},
		{"id":"high"},
		{"value":"not-a-tier"},
		"low"
	]}]}`)
	got, err := ParseGrokModelCatalog(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].ReasoningEfforts) != 4 {
		t.Fatalf("efforts = %#v", got)
	}
	if got[0].ReasoningEfforts[0].ID != "xhigh" || got[0].ReasoningEfforts[1].ID != "high" || got[0].ReasoningEfforts[3].ID != "low" {
		t.Fatalf("wire values = %#v", got[0].ReasoningEfforts)
	}
	routes := grokCatalogToRoutingModels(got)
	want := []string{"xhigh", "high", "low"}
	if len(routes) != 1 || routes[0].ReasoningEffort != "high" || !slicesEqual(routes[0].ReasoningEfforts, want) {
		t.Fatalf("route menu = %#v", routes)
	}
}

func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFetchGrokModelCatalogETagsAnd304(t *testing.T) {
	var requestETag string
	var requestHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestETag = r.Header.Get("If-None-Match")
		requestHeader = r.Header.Clone()
		if requestETag == `"old"` {
			w.Header().Set("ETag", `"new"`)
			w.Header().Set("x-models-etag", "opaque-hint")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"grok-4.5","apiBackend":"responses"}]}`)
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", AccountID: "user-1", Email: "private@example.test", BaseURL: server.URL + "/v1"}
	result, err := FetchGrokModelCatalog(context.Background(), account, "", `"old"`)
	if err != nil {
		t.Fatalf("FetchGrokModelCatalog: %v", err)
	}
	if !result.NotModified || result.HTTPETag != `"new"` || result.ModelsETagHint != "opaque-hint" {
		t.Fatalf("304 result = %#v", result)
	}
	if requestETag != `"old"` {
		t.Fatalf("If-None-Match = %q", requestETag)
	}
	if requestHeader.Get("x-userid") != "user-1" || requestHeader.Get("x-email") != "private@example.test" ||
		requestHeader.Get("x-grok-session-id") != "" || requestHeader.Get("x-compaction-at") != "" {
		t.Fatalf("model discovery headers = %#v", requestHeader)
	}
}

func TestGrokHTTPErrorDoesNotExposeWholeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.Copy(w, bytes.NewBufferString(`{"error":{"message":"denied"},"secret":"do-not-leak"}`))
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: server.URL + "/v1"}
	_, err := FetchGrokModelCatalog(context.Background(), account, "", "")
	var httpErr *GrokHTTPError
	if err == nil || !AsGrokHTTPError(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %#v", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("do-not-leak")) {
		t.Fatalf("error leaked whole upstream body: %v", err)
	}
}

func TestFetchGrokModelIDsPublishesAuthoritativeEmptyCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: server.URL + "/v1"}
	models, err := FetchGrokModelIDs(context.Background(), account, "")
	if err != nil || len(models) != 0 {
		t.Fatalf("FetchGrokModelIDs = %v, %v; want empty success", models, err)
	}
	if !account.HasGrokModelCatalog() || account.GrokChannelSupportsModel("grok-4.5") {
		t.Fatal("successful empty catalog must be known and must not reopen defaults")
	}
}
