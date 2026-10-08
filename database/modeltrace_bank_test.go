package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestModelTraceBankOverrideLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "modeltrace.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if revision, err := db.GetModelTraceBankRevision(ctx); err != nil || revision != "" {
		t.Fatalf("fresh revision = %q, %v", revision, err)
	}
	if override, err := db.GetModelTraceBankOverride(ctx); err != nil || override != nil {
		t.Fatalf("fresh override = %+v, %v", override, err)
	}

	installedAt := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	save := func(revision, expected string) error {
		return db.SaveModelTraceBankOverride(ctx, ModelTraceBankOverride{
			Revision: revision, BuiltAt: "2026-09-30T22:05:57Z", BankJSON: []byte(`{"rev":"` + revision + `"}`), InstalledAt: installedAt,
		}, expected)
	}
	if err := save("aaa", ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A writer that read "no override" must not clobber the one just installed.
	if err := save("zzz", ""); !errors.Is(err, ErrModelTraceBankConflict) {
		t.Fatalf("stale insert error = %v, want conflict", err)
	}
	if err := save("zzz", "old"); !errors.Is(err, ErrModelTraceBankConflict) {
		t.Fatalf("stale update error = %v, want conflict", err)
	}
	if err := save("bbb", "aaa"); err != nil {
		t.Fatalf("CAS update: %v", err)
	}
	override, err := db.GetModelTraceBankOverride(ctx)
	if err != nil || override == nil {
		t.Fatalf("GetModelTraceBankOverride() = %+v, %v", override, err)
	}
	if override.Revision != "bbb" || string(override.BankJSON) != `{"rev":"bbb"}` || override.BuiltAt != "2026-09-30T22:05:57Z" || !override.InstalledAt.Equal(installedAt) {
		t.Fatalf("override = %+v", override)
	}
	if revision, err := db.GetModelTraceBankRevision(ctx); err != nil || revision != "bbb" {
		t.Fatalf("revision = %q, %v", revision, err)
	}

	if err := db.DeleteModelTraceBankOverride(ctx); err != nil {
		t.Fatalf("DeleteModelTraceBankOverride: %v", err)
	}
	if revision, err := db.GetModelTraceBankRevision(ctx); err != nil || revision != "" {
		t.Fatalf("revision after delete = %q, %v", revision, err)
	}
	// An update that started before the reset must not reinstall its bank.
	if err := save("ccc", "bbb"); !errors.Is(err, ErrModelTraceBankConflict) {
		t.Fatalf("update after reset error = %v, want conflict", err)
	}
}
