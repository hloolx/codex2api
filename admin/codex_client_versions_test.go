package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func preserveAdminCodexVersionCache(t *testing.T) {
	t.Helper()
	settings := proxy.CurrentRuntimeSettings()
	backup := newTestAdminDB(t)
	for _, target := range proxy.CurrentCodexClientVersions() {
		writeAdminCodexVersionTarget(t, backup, target)
	}
	t.Cleanup(func() {
		if err := proxy.LoadCodexClientVersionCache(context.Background(), backup); err != nil {
			t.Error(err)
		}
		proxy.ApplyRuntimeSettings(settings)
	})
}

func writeAdminCodexVersionTarget(t *testing.T, db *database.DB, target proxy.CodexClientVersionTarget) {
	t.Helper()
	payload, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	key := database.CodexClientVersionCacheKey{ClientKind: target.ClientKind, TargetPlatform: target.TargetPlatform}
	if _, err := db.MutateCodexClientVersionCache(context.Background(), key, func(string) (string, error) { return string(payload), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCodexClientVersionCacheIsReadOnlyAndSurvivesSettingsSave(t *testing.T) {
	preserveAdminCodexVersionCache(t)
	handler, db, _ := newResponseCacheSettingsAdminHandler(t)
	target := proxy.CodexClientVersionTarget{ClientKind: "codex-desktop", TargetPlatform: "win32-x64", Status: "verified", Pairs: []proxy.CodexClientVersionPair{{AppVersion: "26.928.31416", CLIVersion: "0.158.0-alpha.2.1", Source: "official_msix_blockmap", ArtifactID: "verified-artifact"}}}
	writeAdminCodexVersionTarget(t, db, target)
	if err := proxy.LoadCodexClientVersionCache(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	before := proxy.CurrentCodexClientVersions()
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		payload := map[string]any{"site_name": "version settings", "codex_client_versions": []any{map[string]any{"pairs": []any{map[string]any{"cli_version": "9.999.0"}}}}}
		recorder := invokeResponseCacheSettingsAdmin(t, handler, method, payload)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d, %s", method, recorder.Code, recorder.Body.String())
		}
		response := decodeResponseCacheSettingsResponse(t, recorder)
		if len(response.CodexClientVersions) != 10 || response.CodexSyncedDesktopWindowsBuild != target.Pairs[0].AppVersion {
			t.Fatalf("version response: %+v", response.CodexClientVersions)
		}
		if !reflect.DeepEqual(before, proxy.CurrentCodexClientVersions()) {
			t.Fatal("settings operation modified version cache")
		}
	}
	if err := proxy.LoadCodexClientVersionCache(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, proxy.CurrentCodexClientVersions()) {
		t.Fatal("settings operation overwrote persisted cache")
	}
}
