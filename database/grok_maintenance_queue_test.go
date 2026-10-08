package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGrokMaintenance64ClaimsCapabilityIsolationAndBackoff(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	ids := []int64{}
	for i := 0; i < 70; i++ {
		id, err := db.InsertAccountWithUpstream(ctx, "job", "xai", "grok", map[string]any{"upstream_type": "grok", "api_key": "test"}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	jobs, err := db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "a", now, 20*time.Minute, 64)
	if err != nil || len(jobs) != 64 {
		t.Fatalf("claim count=%d err=%v", len(jobs), err)
	}
	next, err := db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "b", now, 20*time.Minute, 64)
	if err != nil || len(next) != 6 {
		t.Fatalf("remaining=%d err=%v", len(next), err)
	}
	seen := map[int64]bool{}
	for _, j := range jobs {
		seen[j.EntityID] = true
	}
	for _, j := range next {
		if seen[j.EntityID] {
			t.Fatal("duplicate cross-instance lease")
		}
	}
	for _, id := range ids {
		if err := db.EnqueueGrokCapabilityMaintenance(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	caps, err := db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokCapability, "cap", now, time.Hour, 4)
	if err != nil || len(caps) != 4 {
		t.Fatal("separate capability claim", err)
	}
	// A slow capability lease has no bearing on the control-plane job.
	if err = db.CompleteMaintenanceJob(ctx, jobs[0].EntityID, MaintenanceJobGrokFreshness, "a", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	retry := now.Add(15 * time.Minute)
	if err = db.FailMaintenanceJob(ctx, caps[0].EntityID, MaintenanceJobGrokCapability, "cap", retry, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	if err = db.EnqueueGrokCapabilityMaintenance(ctx, caps[0].EntityID, now); err != nil {
		t.Fatal(err)
	}
	var raw any
	if err = db.conn.QueryRowContext(ctx, "SELECT due_at FROM maintenance_jobs WHERE entity_id=$1 AND job_kind=$2", caps[0].EntityID, MaintenanceJobGrokCapability).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	got, err := parseDBTimeValue(raw)
	if err != nil || !got.Equal(retry) {
		t.Fatalf("enqueue erased retry: %v %v", got, err)
	}
}
