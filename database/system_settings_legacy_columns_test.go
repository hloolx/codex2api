package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteSystemSettingsIgnoresRetiredColumnsAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-settings.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})

	// The fork retains its BPS enable switch. Other upstream BPS settings remain
	// retired and may persist without a destructive schema migration.
	legacyColumns := []struct {
		name string
		ddl  string
	}{
		{"codex_basispoints_models", "TEXT DEFAULT ''"},
		{"codex_basispoints_403_pause_disabled", "INTEGER DEFAULT 0"},
		{"codex_basispoints_403_probe_interval_minutes", "INTEGER DEFAULT 1"},
		{"codex_basispoints_429_cooldown_seconds", "INTEGER DEFAULT 5"},
		{"codex_basispoints_cache_creation_as_input", "INTEGER DEFAULT 0"},
	}
	for _, column := range legacyColumns {
		var count int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('system_settings') WHERE name = $1`, column.name).Scan(&count); err != nil {
			t.Fatalf("check fresh column %s: %v", column.name, err)
		}
		if count != 0 {
			t.Fatalf("fresh database contains retired column %s", column.name)
		}
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN `+column.name+` `+column.ddl); err != nil {
			t.Fatalf("restore legacy column %s: %v", column.name, err)
		}
	}
	if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "Before upgrade", MaxConcurrency: 4}); err != nil {
		t.Fatalf("create settings with legacy columns: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET
		codex_basispoints_enabled = 1,
		codex_basispoints_models = 'retired-model',
		codex_basispoints_403_pause_disabled = 1,
		codex_basispoints_403_probe_interval_minutes = 60,
		codex_basispoints_429_cooldown_seconds = 90,
		codex_basispoints_cache_creation_as_input = 1
		WHERE id = 1`); err != nil {
		t.Fatalf("populate legacy settings: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.SiteName != "Before upgrade" || settings.MaxConcurrency != 4 {
		t.Fatalf("read after upgrade = %+v, %v", settings, err)
	}
	settings.SiteName = "After upgrade"
	settings.MaxConcurrency = 8
	if !settings.CodexBasispointsEnabled {
		t.Fatal("upgrade lost the fork's active BPS setting")
	}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("save after upgrade: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close upgraded database: %v", err)
	}
	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("restart upgraded database: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.SiteName != "After upgrade" || settings.MaxConcurrency != 8 {
		t.Fatalf("read saved settings after restart = %+v, %v", settings, err)
	}
	var enabled, pauseDisabled, probeMinutes, cooldownSeconds, cacheAsInput int
	var models string
	if err := db.conn.QueryRowContext(ctx, `SELECT
		codex_basispoints_enabled,
		codex_basispoints_models,
		codex_basispoints_403_pause_disabled,
		codex_basispoints_403_probe_interval_minutes,
		codex_basispoints_429_cooldown_seconds,
		codex_basispoints_cache_creation_as_input
		FROM system_settings WHERE id = 1`).Scan(&enabled, &models, &pauseDisabled, &probeMinutes, &cooldownSeconds, &cacheAsInput); err != nil {
		t.Fatalf("read preserved legacy columns: %v", err)
	}
	if enabled != 1 || models != "retired-model" || pauseDisabled != 1 || probeMinutes != 60 || cooldownSeconds != 90 || cacheAsInput != 1 {
		t.Fatalf("legacy data changed: enabled=%d models=%q pause=%d probe=%d cooldown=%d cache=%d", enabled, models, pauseDisabled, probeMinutes, cooldownSeconds, cacheAsInput)
	}
}
