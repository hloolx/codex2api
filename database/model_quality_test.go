package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPostgresModelQuality(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "quality-lease-fixture", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := db.ModelQualitySnapshot(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.Models = []string{"model-a", "model-b"}
	if err = db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Revision++
	for _, model := range cfg.Models {
		if err = db.EnsureModelQualityState(ctx, id, 1, model); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var claimed atomic.Int64
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := db.ClaimModelQuality(ctx, id, cfg.Models[i%2], fmt.Sprintf("owner-%d", i), cfg.Revision, 1000)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("concurrent leases=%d", claimed.Load())
	}
	// Reclaim an expired lease and verify PostgreSQL's result/snapshot semantics.
	ok, err := db.ClaimModelQuality(ctx, id, "model-a", "final", cfg.Revision, 1120)
	if err != nil || !ok {
		t.Fatalf("reclaim: %v %v", ok, err)
	}
	ok, err = db.FinishModelQuality(ctx, ModelQualityState{AccountID: id, Model: "model-a", Generation: 1, LastOutcome: "fail"}, "final", cfg.Revision, 1121)
	if err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	_, states, err := db.ModelQualitySnapshot(ctx, 1122)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, state := range states {
		if state.AccountID == id && state.Model == "model-a" {
			found = state.Status == "fail" && state.NextCheckAt == 1721
		}
	}
	if !found {
		t.Fatalf("persisted verdict missing: %+v", states)
	}
	cfg.Enabled = false
	if err = db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func newModelQualityDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "quality.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = db.conn.Exec(`INSERT INTO accounts(id,name,credentials,status,credential_generation) VALUES(1,'quality-test','{}','active',7),(2,'quality-test-2','{}','active',7)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestModelQualityIndependentVerdictsErrorsAndRecovery(t *testing.T) {
	db := newModelQualityDB(t)
	ctx := context.Background()
	cfg, states, err := db.ModelQualitySnapshot(ctx, 1000)
	if err != nil || cfg.Enabled || len(states) != 0 {
		t.Fatalf("initial: %+v %v", cfg, err)
	}
	cfg.Enabled = true
	cfg.Models = []string{"a", "b"}
	if err = db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Revision++
	for _, m := range cfg.Models {
		if err = db.EnsureModelQualityState(ctx, 1, 7, m); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(model, outcome string, now int64) {
		t.Helper()
		ok, err := db.ClaimModelQuality(ctx, 1, model, "owner", cfg.Revision, now)
		if err != nil || !ok {
			t.Fatalf("claim %s: %v %v", model, ok, err)
		}
		ok, err = db.FinishModelQuality(ctx, ModelQualityState{AccountID: 1, Model: model, Generation: 7, LastOutcome: outcome, Reason: outcome}, "owner", cfg.Revision, now+1)
		if err != nil || !ok {
			t.Fatalf("finish: %v %v", ok, err)
		}
		if err = db.ReleaseModelQuality(ctx, 1, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	finish("a", "fail", 1000)
	finish("b", "pass", 1000)
	_, states, err = db.ModelQualitySnapshot(ctx, 1002)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Model == "a" && s.Status != "fail" || s.Model == "b" && s.Status != "pass" || s.NextCheckAt != 1601 {
			t.Fatalf("independent verdict: %+v", s)
		}
	}
	if ok, err := db.ClaimModelQuality(ctx, 1, "a", "early", cfg.Revision, 1600); err != nil || ok {
		t.Fatalf("ran before ten minutes: %v %v", ok, err)
	}
	finish("a", "error", 1601)
	_, states, _ = db.ModelQualitySnapshot(ctx, 1603)
	for _, s := range states {
		if s.Model == "a" && (s.Status != "fail" || s.LastOutcome != "error") {
			t.Fatalf("error cleared red: %+v", s)
		}
	}
	finish("a", "pass", 2202)
	_, states, _ = db.ModelQualitySnapshot(ctx, 2204)
	for _, s := range states {
		if s.Status != "pass" {
			t.Fatalf("recovery failed: %+v", s)
		}
	}
	finish("a", "fail", 2803)
	if ok, err := db.ClaimModelQuality(ctx, 1, "a", "before-refresh", cfg.Revision, 3404); err != nil || !ok {
		t.Fatalf("claim before refresh: %v %v", ok, err)
	}
	if _, err = db.conn.Exec(`UPDATE accounts SET credential_generation=8 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.FinishModelQuality(ctx, ModelQualityState{AccountID: 1, Model: "a", Generation: 7, LastOutcome: "pass"}, "before-refresh", cfg.Revision, 3405); err != nil || ok {
		t.Fatalf("old credential result accepted: %v %v", ok, err)
	}
	if err = db.EnsureModelQualityState(ctx, 1, 8, "a"); err != nil {
		t.Fatal(err)
	}
	if err = db.EnsureModelQualityState(ctx, 1, 7, "a"); err != nil {
		t.Fatal(err)
	}
	_, states, _ = db.ModelQualitySnapshot(ctx, 2205)
	for _, s := range states {
		if s.Model == "a" && (s.Status != "fail" || s.NextCheckAt != 0 || s.Generation != 8) {
			t.Fatalf("credential refresh lost the last verdict or fence: %+v", s)
		}
	}
}

func TestModelQualityLeaseAndConfigurationFences(t *testing.T) {
	db := newModelQualityDB(t)
	ctx := context.Background()
	cfg := ModelQualityConfig{Enabled: true, Models: []string{"a", "b"}, Revision: 1}
	if err := db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Revision++
	for _, m := range cfg.Models {
		if err := db.EnsureModelQualityState(ctx, 1, 7, m); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(model, owner string, now int64, want bool) {
		t.Helper()
		ok, err := db.ClaimModelQuality(ctx, 1, model, owner, cfg.Revision, now)
		if err != nil || ok != want {
			t.Fatalf("claim %s: %v %v", owner, ok, err)
		}
	}
	claim("a", "old", 1000, true)
	claim("b", "other", 1001, false)
	claim("a", "new", 1120, true)
	if ok, err := db.RenewModelQuality(ctx, 1, "old", cfg.Revision, 1121); err != nil || ok {
		t.Fatalf("expired owner renewed: %v %v", ok, err)
	}
	s := ModelQualityState{AccountID: 1, Model: "a", Generation: 7, LastOutcome: "fail"}
	if ok, err := db.FinishModelQuality(ctx, s, "old", cfg.Revision, 1121); err != nil || ok {
		t.Fatalf("expired owner wrote: %v %v", ok, err)
	}
	if err := db.ReleaseModelQuality(ctx, 1, "old"); err != nil {
		t.Fatal(err)
	}
	claim("b", "third", 1122, false)
	if ok, err := db.FinishModelQuality(ctx, s, "new", cfg.Revision, 1122); err != nil || !ok {
		t.Fatalf("new owner: %v %v", ok, err)
	}
	cfg.Models = []string{"b"}
	if err := db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelQualityConfig(ctx, cfg); !errors.Is(err, ErrModelQualityConflict) {
		t.Fatalf("stale config accepted: %v", err)
	}
	if ok, err := db.FinishModelQuality(ctx, s, "new", cfg.Revision, 1123); err != nil || ok {
		t.Fatalf("old config result wrote: %v %v", ok, err)
	}
	cfg, states, err := db.ModelQualitySnapshot(ctx, 1124)
	if err != nil || len(states) != 1 || states[0].Model != "b" {
		t.Fatalf("deselected: %+v %v", states, err)
	}
	cfg.Enabled = false
	if err = db.SaveModelQualityConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, states, err = db.ModelQualitySnapshot(ctx, 1125)
	if err != nil || len(states) != 0 {
		t.Fatalf("disabled retained state: %+v %v", states, err)
	}
}
