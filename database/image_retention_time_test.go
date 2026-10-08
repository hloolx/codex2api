package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestImageRetentionUsesUTCAndStrictCutoff(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	// UTC 10:00 == Shanghai 18:00. A 12:00 UTC image must NOT expire yet.
	cutoff := time.Date(2026, 9, 16, 18, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	var expired int64
	for i, created := range []string{"2026-09-16 09:59:59", "2026-09-16 10:00:00", "2026-09-16 12:00:00"} {
		id, e := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "boundary"})
		if e != nil {
			t.Fatal(e)
		}
		if e = db.MarkImageJobSucceeded(ctx, id, 1); e != nil {
			t.Fatal(e)
		}
		aid, e := db.InsertImageAsset(ctx, ImageAssetInput{JobID: id, Filename: "test.png"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.conn.ExecContext(ctx, `UPDATE image_assets SET created_at=$1 WHERE id=$2`, created, aid); e != nil {
			t.Fatal(e)
		}
		if _, e = db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET completed_at=$1 WHERE id=$2`, created, id); e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			expired = aid
		}
	}
	assets, err := db.ExpiredImageAssets(ctx, cutoff, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].ID != expired {
		t.Fatalf("wrong cutoff: %+v", assets)
	}
	if err = db.DeleteImageAsset(ctx, expired); err != nil {
		t.Fatal(err)
	}
	n, err := db.DeleteExpiredImageJobs(ctx, cutoff, 100)
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
}
