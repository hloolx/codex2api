package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestModelQualityGrader(t *testing.T) {
	for _, tc := range []struct {
		output    string
		complete  bool
		err, want string
	}{
		{`{"answer":21}`, true, "", "pass"},
		{"```json\n{\"answer\":21}\n```", true, "", "pass"},
		{`{"answer":29}`, true, "", "fail"},
		{`{"answer":21}`, false, "", "error"},
		{`{"answer":29}`, true, "HTTP 503", "error"},
		{`{"answer":21,"answer":29}`, true, "", "error"},
		{`{"answer":21} or {"answer":29}`, true, "", "error"},
		{`{"answer":21.0}`, true, "", "error"},
		{`{"answer":021}`, true, "", "error"},
		{``, true, "", "error"},
		{`<html>21</html>`, true, "", "error"},
	} {
		t.Run(tc.output+tc.err, func(t *testing.T) {
			got, _ := gradeModelQuality(tc.output, tc.complete, tc.err)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestModelQualityImportedAccountIsCheckedWithoutFallback(t *testing.T) {
	db := newTestAdminDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	cfg := database.ModelQualityConfig{Enabled: true, Models: []string{"model-a", "model-b"}, Revision: 1}
	if err := db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccountWithCredentials(ctx, "quality-fixture", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Account{DBID: id, AccessToken: "fixture", Status: auth.StatusReady, PlanType: "plus", CredentialGeneration: 1}
	store.AddAccount(a)
	started := make(chan string, 2)
	r := &modelQualityRunner{h: h, wake: make(chan struct{}, 1)}
	r.probe = func(ctx context.Context, account *auth.Account, model string) (string, string) {
		if account != a {
			t.Error("probe changed account")
		}
		started <- model
		if model == "model-a" {
			return "fail", "wrong answer"
		}
		<-ctx.Done()
		return "error", "cancelled"
	}
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker failed to drain")
		}
	})
	for _, want := range cfg.Models {
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("model=%s want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("import was not checked promptly")
		}
	}
	if !a.IsModelQualityBlocked("model-a") || a.IsModelQualityBlocked("model-b") || !a.IsAvailable() {
		t.Fatal("verdict did not preserve other model/account availability")
	}
	cancel()
	<-done
	if a.ActiveRequests.Load() != 0 || a.OccupiedRequests.Load() != 0 {
		t.Fatal("probe leaked account capacity")
	}
}

func TestModelQualityConfigAPIKeepsOtherRestrictionsAndFencesOldProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	store := auth.NewStore(db, nil, nil)
	id, err := db.InsertAccountWithCredentials(ctx, "quality-fixture", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Account{DBID: id, AccessToken: "fixture", Status: auth.StatusReady, CredentialGeneration: 1}
	store.AddAccount(a)
	h := &Handler{db: db, store: store}
	r := &modelQualityRunner{h: h, wake: make(chan struct{}, 1)}
	h.modelQuality = r
	cfg := database.ModelQualityConfig{Enabled: true, Models: []string{"gpt-test"}, Revision: 1}
	if err = db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Revision++
	if err = db.EnsureModelQualityState(ctx, id, 1, "gpt-test"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimModelQuality(ctx, id, "gpt-test", "owner", cfg.Revision, time.Now().Unix()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r.probe = func(context.Context, *auth.Account, string) (string, string) {
		atomic.StoreInt32(&a.Disabled, 1)
		atomic.StoreInt32(&a.DispatchPaused, 1)
		body := `{"enabled":false,"models":["gpt-test"],"revision":2}`
		router := gin.New()
		router.PUT("/guard", h.UpdateModelQuality)
		response := httptest.NewRecorder()
		request := httptest.NewRequest("PUT", "/guard", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("disable: %d %s", response.Code, response.Body.String())
		}
		return "fail", "late failure"
	}
	r.execute(ctx, a, "gpt-test", "owner", cfg.Revision, 1)
	_, states, err := db.ModelQualitySnapshot(ctx, time.Now().Unix())
	if err != nil || len(states) != 0 || a.IsModelQualityBlocked("gpt-test") {
		t.Fatalf("late result resurrected a gate: %+v %v", states, err)
	}
	if atomic.LoadInt32(&a.Disabled) != 1 || atomic.LoadInt32(&a.DispatchPaused) != 1 {
		t.Fatal("manual restrictions were cleared")
	}
}

// Exhaust all possible draws to independently verify the benchmark's minimum.
func TestModelQualityCandyMinimum(t *testing.T) {
	bad := map[[2]int]bool{}
	for ra := 0; ra <= 7; ra++ {
		for rp := 0; rp <= 9; rp++ {
			for rw := 0; rw <= 8; rw++ {
				for sa := 0; sa <= 7; sa++ {
					for sp := 0; sp <= 6; sp++ {
						for sw := 0; sw <= 4; sw++ {
							if !(ra > 0 && sp > 0 || rp > 0 && sa > 0) {
								bad[[2]int{ra + rp + rw, sa + sp + sw}] = true
							}
						}
					}
				}
			}
		}
	}
	minimum := 42
	for r := 0; r <= 24; r++ {
		for s := 0; s <= 17; s++ {
			if !bad[[2]int{r, s}] && r+s < minimum {
				minimum = r + s
			}
		}
	}
	if minimum != 21 || bad[[2]int{9, 12}] {
		t.Fatalf("minimum=%d, 9/12 failure=%v", minimum, bad[[2]int{9, 12}])
	}
}

func TestModelQualityPayloadDoesNotInheritHTMLInstructions(t *testing.T) {
	body, err := buildQualityTestPayload(&auth.Account{}, "gpt-test", qualityTestRequest{Prompt: modelQualityPrompt, ReasoningEffort: "high", modelQualityProbe: true}, auth.ClaudeSecurityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	instructions := payload["instructions"].(string)
	if strings.Contains(instructions, "HTML") || !strings.Contains(instructions, "JSON") || payload["model"] != "gpt-test" {
		t.Fatalf("bad payload: %s", body)
	}
}
