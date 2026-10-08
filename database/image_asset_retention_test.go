package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestImageAssetRetentionMigrationDefaultsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	id, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkImageJobSucceeded(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	legacy, err := db.InsertImageAsset(ctx, ImageAssetInput{JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	temporary, err := db.InsertImageAsset(ctx, ImageAssetInput{JobID: id, ExpiresAt: expires.Unix(), DeleteAfterRead: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.ensureImageAssetRetentionSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := db.GetImageAsset(ctx, legacy)
	if err != nil || asset.ExpiresAt != 0 || asset.DeleteAfterRead {
		t.Fatal("legacy behavior changed", err)
	}
	asset, err = db.GetImageAsset(ctx, temporary)
	if err != nil || asset.ExpiresAt != expires.Unix() || !asset.DeleteAfterRead {
		t.Fatal("policy lost after restart", err)
	}
	due, err := db.DueImageAssets(ctx, expires.Add(-time.Second), 0, 100)
	if err != nil || len(due) != 0 {
		t.Fatal("expiry fired early", err)
	}
	due, err = db.DueImageAssets(ctx, expires, 0, 100)
	if err != nil || len(due) != 1 || due[0].ID != temporary {
		t.Fatal("expiry boundary incorrect", err)
	}
	// A caller's explicit TTL overrides the operator's global age-based policy.
	global, err := db.ExpiredImageAssets(ctx, time.Now().Add(24*time.Hour), 0, 100)
	if err != nil || len(global) != 1 || global[0].ID != legacy {
		t.Fatal("global retention overrode explicit TTL", err)
	}
}
