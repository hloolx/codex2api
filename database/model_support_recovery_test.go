package database

import (
	"context"
	"testing"
	"time"
)

func TestModelSupportRecovery(t *testing.T) {
	testModelSupportRecovery(t, newModelQualityDB(t), 1)
}

func testModelSupportRecovery(t *testing.T, db *DB, id int64) {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := start.Add(time.Hour)
	for _, tc := range []struct {
		reason  string
		updated time.Time
		want    bool
	}{
		{"model_not_supported", start.Add(-time.Minute), true},
		{"model_not_supported", start.Add(time.Minute), false},
		{"rate_limited_model", start.Add(-time.Minute), false},
	} {
		if err := db.SetModelCooldown(ctx, id, "recovery-fixture", tc.reason, reset); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.ExecContext(ctx, `UPDATE account_model_cooldowns SET updated_at=$1 WHERE account_id=$2 AND model=$3`, db.timeArg(tc.updated), id, "recovery-fixture"); err != nil {
			t.Fatal(err)
		}
		got, err := db.ClearUnsupportedModelSince(ctx, id, "recovery-fixture", start, reset)
		if err != nil || got != tc.want {
			t.Fatalf("%s at %s: cleared=%v err=%v", tc.reason, tc.updated, got, err)
		}
	}
}
