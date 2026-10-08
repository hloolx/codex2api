package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexUnifiedClientIdentitySettingsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	cfg := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, cfg)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(cfg)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(cfg.GlobalRPM), "admin-secret")
	router := gin.New()
	router.GET("/api/admin/settings", h.GetSettings)
	router.PUT("/api/admin/settings", h.UpdateSettings)

	request := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	check := func(want bool) {
		t.Helper()
		rec := request(http.MethodGet, "")
		var response settingsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
			t.Fatalf("GET %d %s", rec.Code, rec.Body.String())
		}
		saved, err := db.GetSystemSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if response.CodexUnifiedClientIdentityEnabled != want || saved.CodexUnifiedClientIdentityEnabled != want || proxy.CurrentRuntimeSettings().CodexUnifiedClientIdentityEnabled != want {
			t.Fatalf("response=%v saved=%v runtime=%v, want %v", response.CodexUnifiedClientIdentityEnabled, saved.CodexUnifiedClientIdentityEnabled, proxy.CurrentRuntimeSettings().CodexUnifiedClientIdentityEnabled, want)
		}
	}

	check(false)
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]bool{"codex_unified_client_identity_enabled": enabled})
		if rec := request(http.MethodPut, string(body)); rec.Code != http.StatusOK {
			t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
		}
		check(enabled)
	}
}
