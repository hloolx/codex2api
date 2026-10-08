package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/google/uuid"
)

func TestImageRetentionDeletesExpiredFilesButPreservesActiveAndText(t *testing.T) {
	db := newTestAdminDB(t)
	dir := t.TempDir()
	t.Setenv("IMAGE_ASSET_DIR", dir)
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: dir}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	ctx := context.Background()
	now := time.Now()
	makeJob := func(terminal bool) (int64, int64, string) {
		id, e := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: "keep text", ParamsJSON: `{"model":"gpt-image-2","input_images":["data:image/png;base64,aGVsbG8="]}`})
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(dir, uuid.NewString()+".png")
		if e = os.WriteFile(path, tinyPNG(t), 0600); e != nil {
			t.Fatal(e)
		}
		aid, e := db.InsertImageAsset(ctx, database.ImageAssetInput{JobID: id, StoragePath: path, Filename: "a.png", MimeType: "image/png", Bytes: 1})
		if e != nil {
			t.Fatal(e)
		}
		if terminal {
			if e = db.MarkImageJobSucceeded(ctx, id, 100); e != nil {
				t.Fatal(e)
			}
		}
		return id, aid, path
	}
	finished, asset, path := makeJob(true)
	active, _, activePath := makeJob(false)
	// Future clocks avoid altering production timestamps or adding test APIs.
	if e := h.cleanupImageStorage(ctx, imageRetention{3, 30}, now.Add(2*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(path); e != nil {
		t.Fatal("deleted unexpired image")
	}
	thumb := diskThumbnailPath(asset, 32)
	os.MkdirAll(filepath.Dir(thumb), 0755)
	os.WriteFile(thumb, []byte("thumb"), 0600)
	if e := h.cleanupImageStorage(ctx, imageRetention{3, 30}, now.Add(4*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("expired file still present")
	}
	if _, e := os.Stat(thumb); !os.IsNotExist(e) {
		t.Fatal("expired thumbnail still present")
	}
	job, e := db.GetImageGenerationJob(ctx, finished)
	if e != nil || job.Prompt != "keep text" || len(job.Assets) != 0 {
		t.Fatalf("history lost: %v", e)
	}
	if strings.Contains(job.ParamsJSON, "base64") {
		t.Fatal("expired legacy Base64 retained")
	}
	if _, e = os.Stat(activePath); e != nil {
		t.Fatal("active task image deleted")
	}
	if e = h.cleanupImageStorage(ctx, imageRetention{3, 30}, now.Add(31*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = db.GetImageGenerationJob(ctx, finished); e == nil {
		t.Fatal("expired history retained")
	}
	if _, e = db.GetImageGenerationJob(ctx, active); e != nil {
		t.Fatal("active task deleted")
	}
}

func TestImageRetentionKeepsMetadataWhenStorageCannotBeDeleted(t *testing.T) {
	db := newTestAdminDB(t)
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	ctx := context.Background()
	id, e := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: "retry deletion"})
	if e != nil {
		t.Fatal(e)
	}
	aid, e := db.InsertImageAsset(ctx, database.ImageAssetInput{JobID: id, StoragePath: "s3://unconfigured-bucket/file.png"})
	if e != nil {
		t.Fatal(e)
	}
	db.MarkImageJobSucceeded(ctx, id, 1)
	h := &Handler{db: db}
	if e = h.cleanupImageStorage(ctx, imageRetention{3, 30}, time.Now().Add(40*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = db.GetImageAsset(ctx, aid); e != nil {
		t.Fatal("lost retry metadata")
	}
	if _, e = db.GetImageGenerationJob(ctx, id); e != nil {
		t.Fatal("deleted history with undeleted asset")
	}
}

func TestOrphanInputCleanupPreservesActiveAndFreshFiles(t *testing.T) {
	db := newTestAdminDB(t)
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	ctx := context.Background()
	now := time.Now()
	makeInput := func(old bool) string {
		token := queueInputPrefix + uuid.NewString()
		path, _ := queueInputPath(token)
		os.MkdirAll(filepath.Dir(path), 0700)
		os.WriteFile(path, []byte("input"), 0600)
		if old {
			os.Chtimes(path, now.Add(-48*time.Hour), now.Add(-48*time.Hour))
		}
		return token
	}
	active, orphan, fresh := makeInput(true), makeInput(true), makeInput(false)
	params, _ := json.Marshal(map[string]any{"input_images": []string{active}})
	if _, e := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{ParamsJSON: string(params)}); e != nil {
		t.Fatal(e)
	}
	h := &Handler{db: db}
	n, e := h.cleanupOrphanImageInputs(ctx, now.Add(-24*time.Hour))
	if e != nil || n != 1 {
		t.Fatalf("removed=%d err=%v", n, e)
	}
	for _, token := range []string{active, fresh} {
		path, _ := queueInputPath(token)
		if _, e = os.Stat(path); e != nil {
			t.Fatal("removed protected input")
		}
	}
	path, _ := queueInputPath(orphan)
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("orphan retained")
	}
}

func TestImageJobSummaryIsBoundedAndPreservesOwnership(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	key, e := db.InsertAPIKey(ctx, "summary", "sk-summary")
	if e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`{"model":"gpt-image-2","input_images":["data:image/png;base64,secret"]}`, `{"input_images":["` + strings.Repeat("x", 2<<20) + `"]}`, `{broken`} {
		id, e := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: key, Prompt: "text", ParamsJSON: raw})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.InsertImageAsset(ctx, database.ImageAssetInput{JobID: id, Filename: "one.png"}); e != nil {
			t.Fatal(e)
		}
	}
	page, e := db.ListImageJobSummaries(ctx, 1, 20, key)
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Jobs) != 3 {
		t.Fatal("lost rows")
	}
	for _, j := range page.Jobs {
		if len(j.ParamsJSON) > 1000 || strings.Contains(j.ParamsJSON, "input_images") || len(j.Assets) != 1 {
			t.Fatalf("bad summary: id=%d", j.ID)
		}
	}
	page, e = db.ListImageJobSummaries(ctx, 1, 20, key+1)
	if e != nil || len(page.Jobs) != 0 || page.Total != 0 {
		t.Fatal("key isolation broken")
	}
}

func TestThumbnailDiskCacheSurvivesMemoryEviction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMAGE_ASSET_DIR", dir)
	backend, e := imagestore.NewLocalBackend(dir)
	if e != nil {
		t.Fatal(e)
	}
	var encoded bytes.Buffer
	if e = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 16, 16))); e != nil {
		t.Fatal(e)
	}
	path, e := backend.Save(context.Background(), "source.png", encoded.Bytes(), "image/png")
	if e != nil {
		t.Fatal(e)
	}
	asset := &database.ImageAsset{ID: 999001, StoragePath: path}
	thumbCache = imagestore.NewThumbnailCache(1024)
	a, _, e := cachedImageThumbnail(context.Background(), asset, backend, 32)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(diskThumbnailPath(asset.ID, 32)); e != nil {
		t.Fatal("thumbnail not persisted")
	}
	thumbCache = imagestore.NewThumbnailCache(1024)
	os.Remove(path)
	b, _, e := cachedImageThumbnail(context.Background(), asset, backend, 32)
	if e != nil || string(a) != string(b) {
		t.Fatal("thumbnail reread original after eviction")
	}
}
