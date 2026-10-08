package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestImagePerImageRetryBilling(t *testing.T) {
	previousRuntime, previousResin := CurrentRuntimeSettings(), resinCfg.Load()
	t.Cleanup(func() {
		ApplyRuntimeSettings(previousRuntime)
		resinCfg.Store(previousResin)
		database.SetModelPricingOverrides(nil)
	})
	next := previousRuntime
	next.CodexForceWebsocket = false
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"future_image_failure"}}
	ApplyRuntimeSettings(next)
	database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{"gpt-image-2": {UserBillingMode: database.UserBillingModePerImage, ImageUnitPrice: .05}})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fees.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			keyID, err := db.InsertAPIKey(ctx, "retry-fees", "sk-retry-fees-test")
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Runtime background probes can share Resin routing with this mock.
				// Count only actual image requests so probes cannot consume a retry
				// or make the billing assertion depend on other tests' timing.
				if !gjson.GetBytes(readUpstreamRequestBody(r), `tools.#(type=="image_generation")`).Exists() {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					fmt.Fprint(w, "event: error\ndata: {\"type\":\"future_image_failure\",\"error\":{\"message\":\"retry me\"}}\n\n")
					return
				}
				fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"`+tinyPNGBase64+`","output_format":"png"}],"usage":{"input_tokens":1000,"output_tokens":100,"total_tokens":1100}}}`+"\n\n")
			}))
			defer upstream.Close()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "image-fee-retry-test"})
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, MaxRetries: 0})
			defer store.Stop()
			store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "plus", AccountID: "test-account"})
			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			tracker := handler.apiKeyScopeUsageTracker()
			tracker.markTracked(keyID)
			before := time.Now()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(requestCtx)
			c.Set(contextAPIKeyID, keyID)
			handler.forwardImagesRequest(c, "/v1/images/generations", "gpt-image-2", "gpt-image-2", "", []byte(`{"model":"gpt-5.4","input":"test","tools":[{"type":"image_generation","model":"gpt-image-2"}],"stream":true}`), "b64_json", "image_generation", stream)
			if calls.Load() != 2 || recorder.Code != 200 || !strings.Contains(recorder.Body.String(), tinyPNGBase64) {
				t.Fatalf("calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
			}
			if delta := tracker.deltaSince(keyID, before); delta[1].UserBilled != .05 {
				t.Fatalf("local scope fee=%+v", delta)
			}
			db.FlushUsageLogs()
			key, err := db.GetAPIKeyByID(ctx, keyID)
			if err != nil {
				t.Fatal(err)
			}
			if key.QuotaUsed != .05 {
				t.Fatalf("retry quota=%v", key.QuotaUsed)
			}
			logs, err := db.ListUsageLogsByTimeRange(ctx, before.Add(-time.Hour), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			charged := 0
			for _, l := range logs {
				if l.UserBilled > 0 {
					charged++
					if l.BilledImageCount != 1 || l.UserBilled != .05 {
						t.Fatalf("charge=%+v", l)
					}
				}
			}
			if charged != 1 || len(logs) != 2 {
				t.Fatalf("charged=%d logs=%d", charged, len(logs))
			}
		})
	}
}

func TestDeferredImageBillingFinalizesOnce(t *testing.T) {
	database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{"gpt-image-2": {UserBillingMode: database.UserBillingModePerImage, ImageUnitPrice: .05}})
	t.Cleanup(func() { database.SetModelPricingOverrides(nil) })
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "deferred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyID, err := db.InsertAPIKey(context.Background(), "deferred", "sk-deferred-test")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, db, nil, nil)
	ctx, finish := DeferImageJobBilling(context.Background())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(ctx)
	c.Set(contextAPIKeyID, keyID)
	h.logUsageForRequest(c, &database.UsageLogInput{AccountID: 1, Model: "gpt-image-2", StatusCode: 200, ImageCount: 2, InputTokens: 1000})
	db.FlushUsageLogs()
	key, _ := db.GetAPIKeyByID(ctx, keyID)
	if key.QuotaUsed != 0 {
		t.Fatal("charged before outputs saved")
	}
	database.SetModelPricingOverrides(nil)
	finish(1)
	finish(2)
	db.FlushUsageLogs()
	key, _ = db.GetAPIKeyByID(ctx, keyID)
	if key.QuotaUsed != .05 {
		t.Fatalf("deferred fee=%v", key.QuotaUsed)
	}
}
