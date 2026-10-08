package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestModelQualityExecutionExplainsBlockedWork(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		setup func(*auth.Account, *database.ModelQualityState)
		want  string
	}{
		{"ready", func(*auth.Account, *database.ModelQualityState) {}, "queued"},
		{"unsupported", func(a *auth.Account, _ *database.ModelQualityState) { a.Models = []string{"other"} }, "unsupported"},
		{"disabled", func(a *auth.Account, _ *database.ModelQualityState) { a.Disabled = 1 }, "account_unavailable"},
		{"cooldown", func(a *auth.Account, _ *database.ModelQualityState) {
			a.SetModelCooldownUntil("model-a", "model_not_supported", now.Add(time.Hour))
		}, "model_cooldown"},
		{"busy", func(a *auth.Account, _ *database.ModelQualityState) { a.OccupiedRequests.Store(100) }, "waiting_capacity"},
		{"scheduled", func(_ *auth.Account, s *database.ModelQualityState) { s.NextCheckAt = now.Add(time.Minute).Unix() }, "scheduled"},
		{"running", func(_ *auth.Account, s *database.ModelQualityState) { s.Running = true }, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := auth.NewStore(nil, nil, nil)
			a := &auth.Account{DBID: 1, AccessToken: "fixture", Status: auth.StatusReady, CredentialGeneration: 1}
			store.AddAccount(a)
			s := database.ModelQualityState{AccountID: 1, Model: "model-a", Generation: 1, Status: "fail"}
			tc.setup(a, &s)
			h := &Handler{store: store}
			v := h.modelQualityView(a, s, true, false, now)
			if v.Execution != tc.want || v.Status != "fail" {
				t.Fatalf("unexpected view: %+v", v)
			}
			if tc.want == "model_cooldown" && (v.CooldownReason != "model_not_supported" || v.ResumeAt == 0 || !v.CanRetest) {
				t.Fatalf("missing cooldown context: %+v", v)
			}
			if off := h.modelQualityView(a, s, false, false, now); off.Execution != "disabled" || off.CanRetest {
				t.Fatalf("disabled view: %+v", off)
			}
		})
	}
}

func TestModelQualityRetestQueuesWithoutBypassingCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "queue-fixture", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	a := &auth.Account{DBID: id, AccessToken: "fixture", Status: auth.StatusReady, CredentialGeneration: 1}
	store.AddAccount(a)
	a.SetModelCooldownUntil("model-a", "model_not_supported", time.Now().Add(time.Hour))
	if err = db.SaveModelQualityConfig(ctx, database.ModelQualityConfig{Enabled: true, Models: []string{"model-a"}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	h.modelQuality = &modelQualityRunner{h: h, wake: make(chan struct{}, 1)}
	router := gin.New()
	router.POST("/retest", h.RetestModelQuality)
	router.GET("/state", h.GetModelQuality)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/retest", strings.NewReader(fmt.Sprintf(`{"account_id":%d,"model":"model-a"}`, id))))
	if w.Code != 202 || !a.IsModelRateLimited("model-a") {
		t.Fatalf("retest bypassed cooldown: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/state", nil))
	var response struct {
		Accounts []modelQualityAccountView `json:"accounts"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Accounts) != 1 || response.Accounts[0].States[0].Execution != "model_cooldown" {
		t.Fatalf("missing blocking reason: %s", w.Body.String())
	}
}

func TestCompletedModelTestRecoversOnlyUnsupportedCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body string
		clear      bool
	}{
		{"success", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`, true},
		{"incomplete", `{"type":"response.completed","response":{"status":"incomplete"}}`, false},
		{"empty", `{"type":"response.completed","response":{"status":"completed","output":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", tc.body)
			}))
			defer server.Close()
			store := auth.NewStore(nil, nil, nil)
			a := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "fixture", Models: []string{"gpt-test"}, Status: auth.StatusReady}
			store.AddAccount(a)
			a.RestoreModelCooldown("gpt-test", "model_not_supported", time.Now().Add(time.Hour), time.Now().Add(-time.Minute))
			a.RestoreModelCooldown("other", "rate_limited_model", time.Now().Add(time.Hour), time.Now().Add(-time.Minute))
			h := &Handler{store: store}
			router := gin.New()
			router.GET("/accounts/:id/test", h.TestConnection)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/accounts/42/test?model=gpt-test", nil))
			if a.IsModelRateLimited("gpt-test") == tc.clear || !a.IsModelRateLimited("other") {
				t.Fatalf("wrong recovery after %s: %s", tc.name, w.Body.String())
			}
		})
	}
}
