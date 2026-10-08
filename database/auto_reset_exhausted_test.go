package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExhaustedResetSettingsPersistence(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "settings.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			settings := &SystemSettings{PromptFilterCustomPatterns: `[{"id":"fixture"}]`, PromptFilterReviewAPIKey: "fixture-key", AutoResetCreditsBeforeExpiryMin: 90}
			if err := db.UpdateSystemSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.AutoResetCreditsOnExhaustionEnabled {
				t.Fatal("new switch must default off")
			}
			for _, enabled := range []bool{true, false} {
				settings.AutoResetCreditsOnExhaustionEnabled = enabled
				settings.PreservePromptFilterCustomPatterns = true
				settings.PreservePromptFilterReviewAPIKey = true
				settings.PromptFilterCustomPatterns = "[]"
				settings.PromptFilterReviewAPIKey = ""
				if err := db.UpdateSystemSettings(ctx, settings); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetSystemSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got.AutoResetCreditsOnExhaustionEnabled != enabled || got.AutoResetCreditsEnabled || got.AutoResetCreditsBeforeExpiryMin != 90 {
					t.Fatal("reset policies not persisted independently")
				}
				if got.PromptFilterCustomPatterns != `[{"id":"fixture"}]` || got.PromptFilterReviewAPIKey != "fixture-key" {
					t.Fatal("new SQL parameter disturbed preserve flags")
				}
			}
			// Simulate upgrading an existing installation lacking the newly added column.
			if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN auto_reset_credits_on_exhaustion_enabled"); err != nil {
				t.Fatal(err)
			}
			upgraded, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			got, err = upgraded.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.AutoResetCreditsOnExhaustionEnabled || got.AutoResetCreditsBeforeExpiryMin != 90 || got.PromptFilterReviewAPIKey != "fixture-key" {
				t.Fatal("migration did not preserve old settings and default off")
			}
		})
	}
}
