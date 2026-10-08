package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

func modelTraceBankWithBuiltAt(t *testing.T, builtAt string) []byte {
	t.Helper()
	body, err := sjson.SetBytes(proxy.ModelTraceEmbeddedBankForTest(), "built_at", builtAt)
	if err != nil {
		t.Fatalf("set built_at: %v", err)
	}
	return body
}

func callModelTraceBank(t *testing.T, handler *Handler, method string, fn gin.HandlerFunc, out any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, "/api/admin/modeltrace/bank", nil)
	fn(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", method, recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

func TestModelTraceBankUpdateLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "modeltrace.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	handler := &Handler{db: db}

	embedded, err := proxy.NewModelTraceDetector()
	if err != nil {
		t.Fatalf("NewModelTraceDetector: %v", err)
	}
	newerRevision := "1111111111111111111111111111111111111111"
	olderRevision := "2222222222222222222222222222222222222222"
	latest := newerRevision
	newer := modelTraceBankWithBuiltAt(t, embedded.BuiltAt().Add(24*time.Hour).Format(time.RFC3339Nano))
	newer, _ = sjson.SetBytes(newer, "models.0.display_name", "Renamed for test")
	older := modelTraceBankWithBuiltAt(t, embedded.BuiltAt().Add(-24*time.Hour).Format(time.RFC3339Nano))
	bankFetches := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/commits/main":
			_, _ = w.Write([]byte(latest))
		case "/" + newerRevision + "/bank.json":
			bankFetches++
			_, _ = w.Write(newer)
		case "/" + olderRevision + "/bank.json":
			bankFetches++
			_, _ = w.Write(older)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	defer proxy.SetModelTraceSourceURLsForTest(upstream.URL+"/commits/main", upstream.URL+"/%s/bank.json")()

	var status modelTraceBankStatus
	callModelTraceBank(t, handler, http.MethodGet, handler.GetModelTraceBank, &status)
	if status.Active.Origin != proxy.ModelTraceBankOriginEmbedded || status.Active.Revision != proxy.ModelTraceSourceRevision || status.Override != nil {
		t.Fatalf("initial status = %+v", status)
	}

	var update modelTraceBankUpdateResponse
	callModelTraceBank(t, handler, http.MethodPost, handler.UpdateModelTraceBank, &update)
	if !update.Updated || update.Active.Origin != proxy.ModelTraceBankOriginOverride || update.Active.Revision != newerRevision {
		t.Fatalf("first update = %+v", update)
	}
	detector, err := handler.currentModelTraceDetector(context.Background())
	if err != nil || detector.Revision() != newerRevision {
		t.Fatalf("detector after update = %v, %v", detector, err)
	}

	update = modelTraceBankUpdateResponse{}
	callModelTraceBank(t, handler, http.MethodPost, handler.UpdateModelTraceBank, &update)
	if update.Updated || bankFetches != 1 {
		t.Fatalf("repeat update = %+v fetches=%d, want no-op without download", update, bankFetches)
	}

	// A bank trained before the active one must not replace it.
	latest = olderRevision
	update = modelTraceBankUpdateResponse{}
	callModelTraceBank(t, handler, http.MethodPost, handler.UpdateModelTraceBank, &update)
	if update.Updated || update.Active.Revision != newerRevision {
		t.Fatalf("older upstream update = %+v", update)
	}

	status = modelTraceBankStatus{}
	callModelTraceBank(t, handler, http.MethodDelete, handler.ResetModelTraceBank, &status)
	if status.Active.Origin != proxy.ModelTraceBankOriginEmbedded || status.Override != nil {
		t.Fatalf("status after reset = %+v", status)
	}
}

func TestModelTraceBankIgnoresOverrideOlderThanEmbedded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "modeltrace.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	embedded, err := proxy.NewModelTraceDetector()
	if err != nil {
		t.Fatalf("NewModelTraceDetector: %v", err)
	}
	// Simulates upgrading to a release whose embedded bank is newer than the
	// override an operator installed earlier.
	if err := db.SaveModelTraceBankOverride(context.Background(), database.ModelTraceBankOverride{
		Revision: "3333333333333333333333333333333333333333", BuiltAt: "2026-01-01T00:00:00Z",
		BankJSON:    modelTraceBankWithBuiltAt(t, embedded.BuiltAt().Add(-time.Hour).Format(time.RFC3339Nano)),
		InstalledAt: time.Now(),
	}, ""); err != nil {
		t.Fatalf("SaveModelTraceBankOverride: %v", err)
	}
	handler := &Handler{db: db}
	var status modelTraceBankStatus
	callModelTraceBank(t, handler, http.MethodGet, handler.GetModelTraceBank, &status)
	if status.Active.Origin != proxy.ModelTraceBankOriginEmbedded || status.Override == nil || !status.OverrideStale {
		t.Fatalf("status = %+v, want embedded active with stale override", status)
	}
}

func TestModelTraceBankFallsBackWhenOverrideIsCorrupt(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "modeltrace.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SaveModelTraceBankOverride(context.Background(), database.ModelTraceBankOverride{
		Revision: "4444444444444444444444444444444444444444", BankJSON: []byte(`{"schema":"other"}`), InstalledAt: time.Now(),
	}, ""); err != nil {
		t.Fatalf("SaveModelTraceBankOverride: %v", err)
	}
	handler := &Handler{db: db}
	detector, err := handler.currentModelTraceDetector(context.Background())
	if err != nil || detector.Revision() != proxy.ModelTraceSourceRevision {
		t.Fatalf("currentModelTraceDetector() = %v, %v; want embedded fallback", detector, err)
	}
	status, err := handler.modelTraceBankStatus(context.Background())
	if err != nil || status.OverrideError == "" || status.Active.Origin != proxy.ModelTraceBankOriginEmbedded {
		t.Fatalf("status = %+v, %v", status, err)
	}
}
