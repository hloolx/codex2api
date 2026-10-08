package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/version"
	"github.com/gin-gonic/gin"
)

func TestGetHealthIncludesRunningBuildVersionAndPreservesCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })

	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "ready-token", Status: auth.StatusReady})
	coolingDown := &auth.Account{DBID: 2, AccessToken: "cooldown-token", Status: auth.StatusReady}
	coolingDown.SetCooldownWithReason(time.Hour, "rate_limited")
	store.AddAccount(coolingDown)

	h := &Handler{store: store}
	router := gin.New()
	router.GET("/api/admin/health", h.GetHealth)

	for _, buildVersion := range []string{"3.0.5", "v3.0.6", "dev"} {
		t.Run(buildVersion, func(t *testing.T) {
			version.Version = buildVersion
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/health", nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode health response: %v", err)
			}
			if got := payload["build_version"]; got != buildVersion {
				t.Fatalf("build_version = %v, want %q", got, buildVersion)
			}
			if payload["status"] != "ok" || payload["available"] != float64(1) || payload["total"] != float64(2) {
				t.Fatalf("health response = %v, want status=ok available=1 total=2", payload)
			}
		})
	}
}
