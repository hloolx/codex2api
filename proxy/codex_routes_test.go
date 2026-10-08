package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestCodexRouteFailureClassification(t *testing.T) {
	sample := `{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":null}}`
	cases := []struct {
		name                   string
		status                 int
		source, body, category string
		change                 bool
	}{
		{"http", 403, "upstream_http", sample, "ambiguous_usage_rejection", true},
		{"stream", 200, "upstream_sse", sample, "ambiguous_usage_rejection", true},
		{"websocket", 200, "upstream_websocket", sample, "ambiguous_usage_rejection", true},
		{"local", 403, "local", sample, "local_or_translated", false},
		{"translated", 403, "bridge", sample, "local_or_translated", false},
		{"successful http", 200, "upstream_http", sample, "unclassified", false},
		{"unrelated text", 403, "upstream_http", `{"error":{"message":"See ticket 403","type":"server_error"}}`, "unclassified", false},
		{"wrong type", 403, "upstream_http", strings.Replace(sample, "server_error", "invalid_request_error", 1), "unclassified", false},
		{"html", 403, "upstream_http", "<html>403</html>", "network_or_waf", false},
		{"safety", 403, "upstream_http", `{"error":{"code":"content_policy_violation","message":"403: This request was blocked by our usage policy."}}`, "explicit_safety_policy", false},
		{"auth", 401, "upstream_http", sample, "authentication", false},
		{"limit", 429, "upstream_http", sample, "rate_limit", false},
		{"billing", 402, "upstream_http", sample, "billing", false},
		{"protocol", 400, "upstream_http", `{"error":{"code":"basispoints_protocol_error"}}`, "protocol", false},
		{"encrypted", 400, "upstream_http", `{"error":{"code":"invalid_encrypted_content"}}`, "encrypted_content", false},
		{"model", 403, "upstream_http", `{"error":{"code":"basispoints_model_access_changed"}}`, "model_access", true},
		{"path", 403, "upstream_http", `{"error":{"code":"codex_access_restricted"}}`, "upstream_access", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := classifyCodexRouteFailure(tt.status, tt.source, []byte(tt.body))
			if f.Category != tt.category || f.Switch != tt.change || f.HTTPStatus != tt.status {
				t.Fatalf("classification: %+v", f)
			}
			if f.Category == "ambiguous_usage_rejection" && f.ReportedStatus != 403 {
				t.Fatal("reported status lost")
			}
		})
	}
	for _, usage := range []string{`"input_tokens":1`, `"output_tokens":1`, `"total_tokens":1`} {
		body := strings.TrimSuffix(sample, "}") + `,"usage":{` + usage + "}}"
		if classifyCodexRouteFailure(403, "upstream_http", []byte(body)).Switch {
			t.Fatal("must retain billed failure")
		}
	}
}

type routeTrackedBody struct {
	io.Reader
	closed int
}

func (b *routeTrackedBody) Close() error { b.closed++; return nil }

func routeTestResponse(status int, text string) *http.Response {
	typ := "text/event-stream"
	if status >= 400 {
		typ = "application/json"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {typ}}, Body: io.NopCloser(strings.NewReader(text))}
}

func TestCodexControlledSameAccountFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP403", true: "SSE200"}[stream], func(t *testing.T) {
			enableBasispointsForTest(t)
			t.Setenv("BASISPOINTS_NATIVE_FALLBACK", "true")
			a := &auth.Account{DBID: 993001, AccessToken: "test-token", AccountID: "workspace"}
			sample := `{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":null}}`
			status := 403
			if stream {
				status = 200
				sample = "data: " + `{"type":"response.created","response":{"output":[]}}` + "\n\ndata: " + `{"type":"response.failed","response":` + sample + "}\n\n"
			}
			bpsCalls, nativeCalls := 0, 0
			tracked := &routeTrackedBody{Reader: strings.NewReader(sample)}
			installBasispointsTransport(t, a, func(r *http.Request) (*http.Response, error) {
				bpsCalls++
				resp := routeTestResponse(status, "")
				resp.Body = tracked
				return resp, nil
			})
			ctx := context.Background()
			d := newCodexRouteDecision(ctx, "alias", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: database.CodexRouteBasispointsPrefer}, 3)
			ctx = context.WithValue(ctx, codexRouteKey{}, d)
			native := func(ctx context.Context, account *auth.Account, b []byte, s, p, k string, dev *DeviceProfileConfig, h http.Header) (*http.Response, error) {
				nativeCalls++
				if account != a || gjson.GetBytes(b, "model").String() != "gpt-6-astra" || strings.Contains(string(b), "run_officejs") {
					t.Fatalf("unsafe native body: %s", b)
				}
				return routeTestResponse(200, "data: "+`{"type":"response.created","response":{"output":[]}}`+"\n\ndata: "+`{"type":"response.completed","response":{"output":[]}}`+"\n\n"), nil
			}
			resp, err := executeCodexRoute(ctx, a, []byte(`{"model":"gpt-6-astra","input":"hello"}`), "session", "", "key", nil, nil, false, native)
			if err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if bpsCalls != 1 || nativeCalls != 1 || tracked.closed != 1 || !d.Switched || len(d.Attempts) != 2 || d.Remaining != 1 {
				t.Fatalf("attempts: %+v bps=%d native=%d closed=%d", d, bpsCalls, nativeCalls, tracked.closed)
			}
			if strings.Count(string(out), "response.created") != 1 || strings.Contains(string(out), "response.failed") {
				t.Fatalf("mixed streams: %s", out)
			}
			if d.Attempts[0].HTTPStatus != status || d.Attempts[0].ReportedStatus != 403 {
				t.Fatalf("status conflation: %+v", d.Attempts)
			}
			if a.CodexPathSnapshot(database.CodexPathBasispoints, "gpt-6-astra", time.Now()).Capability != database.CapabilityUnknown {
				t.Fatal("ambiguous rejection persisted unsupported")
			}
			if a.CodexPathSnapshot(database.CodexPathNative, "gpt-6-astra", time.Now()).Capability != database.CapabilitySupported {
				t.Fatal("successful completion not observed")
			}
		})
	}
}

func TestCodexRoutePoliciesAndCommitBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, policy, first                      string
		budget                                   int
		stateMissing, disabledFallback, canceled bool
		wantNative                               int
	}{
		{name: "only", policy: "basispoints_only", budget: 3},
		{name: "budget", policy: "basispoints_prefer", budget: 1},
		{name: "fallback switch off", policy: "basispoints_prefer", budget: 3, disabledFallback: true},
		{name: "state missing", policy: "basispoints_prefer", budget: 3, stateMissing: true},
		{name: "cancel", policy: "basispoints_prefer", budget: 3, canceled: true},
		{name: "text committed", policy: "basispoints_prefer", budget: 3, first: `{"type":"response.output_text.delta","delta":"hi"}`},
		{name: "tool committed", policy: "basispoints_prefer", budget: 3, first: `{"type":"response.output_item.added","item":{"type":"function_call","name":"send_email"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enableBasispointsForTest(t)
			t.Setenv("BASISPOINTS_NATIVE_FALLBACK", map[bool]string{true: "false", false: "true"}[tt.disabledFallback])
			if tt.stateMissing {
				old := ipv6StateProvider.Load()
				t.Cleanup(func() { SetIPv6StateProvider(old) })
				SetIPv6StateProvider(&IPv6StateProvider{EligibleAccounts: func(string) (bool, map[int64]bool) { return true, map[int64]bool{} }})
			}
			a := &auth.Account{DBID: 993002, AccessToken: "token", AccountID: "workspace"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := newCodexRouteDecision(ctx, "gpt-6-astra", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: tt.policy}, tt.budget)
			ctx = context.WithValue(ctx, codexRouteKey{}, d)
			installBasispointsTransport(t, a, func(r *http.Request) (*http.Response, error) {
				if tt.canceled {
					cancel()
				}
				prefix := ""
				if tt.first != "" {
					prefix = "data: " + tt.first + "\n\n"
				}
				return routeTestResponse(200, prefix+"data: "+`{"type":"response.failed","response":{"error":{"type":"server_error","code":null,"message":"403: This request was blocked by our usage policy."}}}`+"\n\n"), nil
			})
			calls := 0
			native := func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header) (*http.Response, error) {
				calls++
				return nil, nil
			}
			resp, err := executeCodexRoute(ctx, a, []byte(`{"model":"gpt-6-astra","input":"hi"}`), "", "", "key", nil, nil, false, native)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if calls != 0 {
				t.Fatal("unsafe switch")
			}
			if err != nil && !tt.canceled {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexRouteGlobalGatesAndModelMatcher(t *testing.T) {
	enableBasispointsForTest(t)
	t.Setenv("BASISPOINTS_MODELS", "")
	for model, want := range map[string]bool{"gpt-6-astra": true, "GPT-6-ASTRA": true, "gpt-6-astra-2026-09-25": true, "gpt-6-astra-pro": false, "gpt-6-astra-2026-02-30": false, "gpt-6-astral": false, "gpt-5.5": false} {
		if basispointsModelAllowed(model) != want {
			t.Errorf("model %s", model)
		}
	}
	a := &auth.Account{DBID: 1, AccessToken: "token", AccountID: "workspace"}
	ctx := context.Background()
	d := newCodexRouteDecision(ctx, "gpt-6-astra", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: "basispoints_only"}, 3)
	settings := CurrentRuntimeSettings()
	settings.CodexBasispointsEnabled = false
	ApplyRuntimeSettings(settings)
	if d.pathEligible(a, "basispoints", "gpt-6-astra", nil) {
		t.Fatal("Key enabled globally disabled path")
	}
	if d.Policy != "basispoints_only" || len(d.Paths) != 1 {
		t.Fatal("only policy expanded")
	}
}

func TestCodexOpaqueHistoryCannotCrossUpstreams(t *testing.T) {
	for _, body := range []string{`{"previous_response_id":"resp_secret"}`, `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, `{"input":[{"type":"item_reference","id":"item"}]}`} {
		if _, err := safeCodexSwitchBody([]byte(body)); err == nil {
			t.Fatal("opaque history accepted")
		}
	}
	body := []byte(`{"input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"done"}]}`)
	got, err := safeCodexSwitchBody(body)
	if err != nil || string(got) != string(body) {
		t.Fatal("complete tool history changed")
	}
}

func TestCodexSharedAttemptBudgetAndNoLoop(t *testing.T) {
	d := newCodexRouteDecision(context.Background(), "m", "m", database.APIKeyLimits{CodexRoutePolicy: "codex_prefer"}, 2)
	a := &auth.Account{DBID: 1}
	if d.begin(a, "codex", "m") != nil {
		t.Fatal("initial rejected")
	}
	d.Switched = true
	d.FinalPath = "basispoints"
	if d.begin(a, "codex", "m") == nil {
		t.Fatal("route loop allowed")
	}
	if d.begin(a, "basispoints", "m") != nil {
		t.Fatal("fallback rejected")
	}
	ctx := context.WithValue(context.Background(), codexRouteKey{}, d)
	if codexRouteBudgetError(ctx) == nil || d.begin(a, "basispoints", "m") == nil {
		t.Fatal("retries exceeded shared budget")
	}
}

func TestCodexKeyGroupIntersectionAndLogicalLease(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "affinity"}[fingerprint], func(t *testing.T) {
			enableBasispointsForTest(t)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 2, TestConcurrency: 1, TestModel: "gpt-6-astra", CodexBasispointsEnabled: true})
			t.Cleanup(store.Stop)
			primary := &auth.Account{DBID: 995001, AccountID: "primary", AccessToken: "test", PlanType: "pro", GroupIDs: []int64{10}}
			split := &auth.Account{DBID: 995002, AccountID: "split", AccessToken: "test", PlanType: "pro", GroupIDs: []int64{20}}
			forbidden := &auth.Account{DBID: 995003, AccountID: "forbidden", AccessToken: "test", PlanType: "pro", GroupIDs: []int64{30}}
			for _, a := range []*auth.Account{primary, split, forbidden} {
				store.AddAccount(a)
				seedContinuousRetryLocalHealth(a)
			}
			store.SetAPIKeyAllowedGroups(9, []int64{10})
			store.SetAPIKeyNoAffinityGroups(9, []int64{20})
			handler := NewHandler(store, nil, &config.Config{}, nil)
			chosen := split
			if fingerprint {
				chosen = primary
			}
			bpsCalls, nativeCalls := 0, 0
			for _, a := range []*auth.Account{primary, split, forbidden} {
				installBasispointsTransport(t, a, func(req *http.Request) (*http.Response, error) {
					bpsCalls++
					if req.Header.Get("Chatgpt-Account-Id") != chosen.AccountID {
						t.Error("unauthorized account selected")
					}
					return routeTestResponse(403, `{"error":{"type":"server_error","code":null,"message":"403: This request was blocked by our usage policy."}}`), nil
				})
				installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
					nativeCalls++
					if req.Header.Get("Chatgpt-Account-Id") != chosen.AccountID {
						t.Error("fallback escaped selected account")
					}
					return routeTestResponse(200, "data: "+`{"type":"response.completed","response":{"id":"resp-done","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n"), nil
				})
			}
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hi","stream":false}`))
			c.Request.Header.Set("Content-Type", "application/json")
			if fingerprint {
				c.Request.Header.Set("X-Codex2API-Affinity-Key", "synthetic-session")
			}
			c.Set(contextAPIKeyID, int64(9))
			c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 9, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}, CodexRoutePolicy: "basispoints_prefer"}})
			handler.Responses(c)
			if r.Code != 200 || bpsCalls != 1 || nativeCalls != 1 {
				t.Fatalf("route result status=%d calls=%d/%d: %s", r.Code, bpsCalls, nativeCalls, r.Body.String())
			}
			if chosen.ActiveRequests.Load() != 0 || chosen.TotalRequests.Load() != 1 || forbidden.TotalRequests.Load() != 0 {
				t.Fatal("lease or request accounting duplicated")
			}
		})
	}
}

func TestCodexEmptyAuthorizedIntersectionReturnsImmediately(t *testing.T) {
	enableBasispointsForTest(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-6-astra", CodexBasispointsEnabled: true})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token", AccountID: "workspace", PlanType: "pro", GroupIDs: []int64{30}})
	store.SetAPIKeyAllowedGroups(9, []int64{10})
	h := NewHandler(store, nil, &config.Config{}, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 9, Limits: database.APIKeyLimits{CodexRoutePolicy: "basispoints_only"}})
	f := h.withCodexRouteFilter(c, "gpt-6-astra", "gpt-6-astra", nil, nil)
	if h.codexRouteHasCandidates(c.Request.Context(), 9, f) {
		t.Fatal("unauthorized candidate admitted")
	}
	_, _, _, err := h.nextRetryAccountWithGuard(c.Request.Context(), "", 9, newRetryAccountExclusions(), f, false, auth.DispatchPolicyStandard)
	if err == nil || !strings.Contains(err.Error(), "codex_route_no_candidates") {
		t.Fatalf("missing explicit selection reason: %v", err)
	}
}

func TestCodexRouteFailedSecondPathCannotLoop(t *testing.T) {
	enableBasispointsForTest(t)
	a := &auth.Account{DBID: 995004, AccessToken: "token", AccountID: "workspace"}
	bps := 0
	installBasispointsTransport(t, a, func(*http.Request) (*http.Response, error) {
		bps++
		return routeTestResponse(403, `{"error":{"code":"basispoints_model_access_changed"}}`), nil
	})
	nativeCalls := 0
	native := func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header) (*http.Response, error) {
		nativeCalls++
		return routeTestResponse(403, `{"error":{"code":"codex_access_restricted"}}`), nil
	}
	d := newCodexRouteDecision(context.Background(), "gpt-6-astra", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: "basispoints_prefer"}, 8)
	ctx := context.WithValue(context.Background(), codexRouteKey{}, d)
	resp, err := executeCodexRoute(ctx, a, []byte(`{"model":"gpt-6-astra","input":"hi"}`), "", "", "key", nil, nil, false, native)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || bps != 1 || nativeCalls != 1 || !strings.Contains(string(body), codexUsageRejectedCode) {
		t.Fatalf("lost final failure or loop: %d %d %s", bps, nativeCalls, body)
	}
	if a.CodexPathSnapshot("basispoints", "gpt-6-astra", time.Now()).Capability != "unsupported" || a.CodexPathSnapshot("basispoints", "other", time.Now()).Capability != "unknown" {
		t.Fatal("model denial broadened")
	}
	if a.CodexPathSnapshot("codex", "gpt-6-astra", time.Now()).Health != "cooldown" {
		t.Fatal("explicit access failure lacks temporary path cooldown")
	}
}

func TestCodexPrefixPreservesTransportError(t *testing.T) {
	expected := errors.New("websocket close 1009")
	for _, status := range []int{200, 502} {
		d := newCodexRouteDecision(context.Background(), "m", "m", database.APIKeyLimits{}, 3)
		a := &codexRouteAttemptState{decision: d, path: "codex", websocket: true}
		ctx := context.WithValue(context.Background(), codexAttemptKey{}, a)
		resp := routeTestResponse(status, "")
		resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(`data: {"type":"response.created"}`+"\n\n"), &codexReadError{err: expected}))
		inspectCodexRouteResponse(ctx, resp)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !errors.Is(err, expected) || !strings.Contains(string(body), "response.created") {
			t.Fatalf("status=%d lost prefix/error: %q %v", status, body, err)
		}
	}
}

func TestCodexSwitchRejectsIncompleteToolHistory(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"function_call","call_id":"a","name":"f"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"a","output":"done"}]}`,
		`{"input":[{"type":"custom_tool_call","call_id":"a"},{"type":"function_call_output","call_id":"a"}]}`,
	} {
		if _, err := safeCodexSwitchBody([]byte(body)); err == nil {
			t.Fatal("incomplete tool history accepted")
		}
	}
	for _, code := range []string{`""`, `0`, `false`} {
		body := []byte(`{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","code":` + code + `}}`)
		if classifyCodexRouteFailure(403, "upstream_http", body).Switch {
			t.Fatal("non-null code matched ambiguous rule")
		}
	}
}

func TestCodexCompactFallbackUsesNativeJSON(t *testing.T) {
	enableBasispointsForTest(t)
	a := &auth.Account{DBID: 995006, AccessToken: "test", AccountID: "workspace"}
	installBasispointsTransport(t, a, func(*http.Request) (*http.Response, error) {
		return routeTestResponse(403, `{"error":{"code":null,"type":"server_error","message":"403: This request was blocked by our usage policy."}}`), nil
	})
	native := func(ctx context.Context, account *auth.Account, b []byte, s, p, k string, dev *DeviceProfileConfig, h http.Header) (*http.Response, error) {
		if account != a || strings.Contains(string(b), "run_officejs") {
			t.Fatal("compact changed account or used envelope")
		}
		resp := routeTestResponse(200, `{"object":"response.compaction","output":[]}`)
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	}
	d := newCodexRouteDecision(context.Background(), "gpt-6-astra", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: "basispoints_prefer"}, 3)
	ctx := context.WithValue(context.Background(), codexRouteKey{}, d)
	resp, err := executeCodexRoute(ctx, a, []byte(`{"model":"gpt-6-astra","input":"hello"}`), "", "", "", nil, nil, true, native)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !gjson.ValidBytes(body) || !d.Switched || len(d.Attempts) != 2 {
		t.Fatalf("compact fallback: %s %v", body, err)
	}
	if effectiveReasoningEffortForAccount(a, "max", ctx) != "max" {
		t.Fatal("native accounting normalized using global BPS switch")
	}
}

func TestCodexPreferReverseSwitchIsBounded(t *testing.T) {
	enableBasispointsForTest(t)
	a := &auth.Account{DBID: 995007, AccessToken: "test", AccountID: "workspace"}
	sample := `{"error":{"code":null,"type":"server_error","message":"403: This request was blocked by our usage policy."}}`
	bps := 0
	nativeCalls := 0
	installBasispointsTransport(t, a, func(*http.Request) (*http.Response, error) { bps++; return routeTestResponse(403, sample), nil })
	native := func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header) (*http.Response, error) {
		nativeCalls++
		return routeTestResponse(403, sample), nil
	}
	d := newCodexRouteDecision(context.Background(), "gpt-6-astra", "gpt-6-astra", database.APIKeyLimits{CodexRoutePolicy: "codex_prefer"}, 3)
	ctx := context.WithValue(context.Background(), codexRouteKey{}, d)
	resp, err := executeCodexRoute(ctx, a, []byte(`{"model":"gpt-6-astra","input":"hello"}`), "", "", "", nil, nil, false, native)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if bps != 1 || nativeCalls != 1 || d.FinalPath != "basispoints" || len(d.Attempts) != 2 {
		t.Fatalf("reverse attempts: %+v", d.Attempts)
	}
}
