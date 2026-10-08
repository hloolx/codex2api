package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestClaudeFableHeaderUsageSurvivesMessagesProbe(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "fable-header", map[string]any{
		"upstream_type": auth.UpstreamClaude, "access_token": "test-token",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	acc := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.0")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", "2030-01-01T00:00:00Z")
	proxy.SyncClaudeUsageState(store, acc, &http.Response{StatusCode: http.StatusOK, Header: headers})
	// Setup Token sampling has no OAuth windows; it must not erase observed headers.
	h.recordClaudeUsageProbe(acc, nil, nil)
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	response := h.buildAccountResponse(row, acc, nil, nil, nil, false)
	if len(response.ClaudeUsageWindows) != 1 {
		t.Fatalf("windows = %+v, want Fable 0%%", response.ClaudeUsageWindows)
	}
	w := response.ClaudeUsageWindows[0]
	if w.Name != "7d_fable" || w.ModelFamily != "fable" || !w.ModelScoped || w.Utilization != 0 || !w.ResetAt.Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Fable window = %+v", w)
	}
	if _, ok := acc.GetUsagePercent7d(); ok {
		t.Fatal("model window must not create account-wide usage")
	}
	for _, value := range []string{"", "bad", "NaN", "+Inf", "-0.1"} {
		headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", value)
		proxy.SyncClaudeUsageState(store, acc, &http.Response{StatusCode: http.StatusOK, Header: headers})
		row, _ = db.GetAccountByID(ctx, id)
		if got := claudeAccountUsageWindows(row); len(got) != 1 || got[0].Utilization != 0 {
			t.Fatalf("missing/invalid header %q erased last observation: %+v", value, got)
		}
	}
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.63")
	headers.Del("anthropic-ratelimit-unified-7d_oi-reset")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.57")
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.14")
	proxy.SyncClaudeUsageState(store, acc, &http.Response{StatusCode: http.StatusOK, Header: headers})
	page := invokeListAccounts(t, h, "/api/admin/accounts?channel=claude&view=page")
	var payload struct {
		Accounts []accountResponse `json:"accounts"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &payload); err != nil || len(payload.Accounts) != 1 {
		t.Fatalf("page response: %s, err=%v", page.Body.String(), err)
	}
	got := payload.Accounts[0].ClaudeUsageWindows
	if len(got) != 1 || got[0].Utilization != 63 || !got[0].ResetAt.IsZero() {
		t.Fatalf("paged header windows = %+v", got)
	}
}

func TestClaudeUsageWindowsMergeKeepsNewestMatchingSource(t *testing.T) {
	observed := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(auth.ClaudeHeaderUsageSnapshot{
		ObservedAt: observed,
		Windows:    []auth.ClaudeUsageWindow{{Name: "7d_fable", Utilization: 0, ModelScoped: true, ModelFamily: "fable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		probeAt time.Time
		want    float64
	}{
		{"newer header", observed.Add(-time.Minute), 0},
		{"newer OAuth", observed.Add(time.Minute), 63},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &database.AccountRow{Credentials: map[string]any{
				auth.ClaudeUsageWindowsCredentialKey: `[{"name":"7d_fable","utilization":63,"model_scoped":true,"model_family":"fable"},{"name":"7d_mythos","utilization":20}]`,
				auth.ClaudeUsageProbeAtCredentialKey: tc.probeAt.Format(time.RFC3339Nano),
				auth.ClaudeHeaderUsageCredentialKey:  string(raw),
			}}
			got := claudeAccountUsageWindows(row)
			if len(got) != 2 || got[0].Utilization != tc.want || got[1].Utilization != 20 {
				t.Fatalf("merged windows = %+v", got)
			}
		})
	}
}
