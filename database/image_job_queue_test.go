package database

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageJobQueueMigrationClaimAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	ctx := context.Background()
	queued, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "preserve me", ParamsJSON: `{"input_images":["https://example.com/a.png"]}`})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = db.InitImageJobQueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, e := db.ClaimImageJob(ctx, queued, "owner", time.Now().Add(time.Minute))
			if e != nil {
				t.Error(e)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("claim winners=%d", wins.Load())
	}
	if ok, e := db.RenewImageJobLease(ctx, queued, "wrong-owner", time.Now().Add(time.Minute)); e != nil || ok {
		t.Fatalf("wrong owner renewed: %v %v", ok, e)
	}
	waiting, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "still queued"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ExpireImageJobLeases(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetImageGenerationJob(ctx, queued)
	if job.Status != ImageJobRunning {
		t.Fatal("live lease was expired")
	}
	if err = db.ExpireImageJobLeases(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishLeasedImageJob(ctx, queued, "owner", ImageJobSucceeded, "", 0); err != nil {
		t.Fatal(err)
	}
	job, _ = db.GetImageGenerationJob(ctx, queued)
	if job.Status != ImageJobFailed {
		t.Fatal("late worker overwrote recovery")
	}
	job, _ = db.GetImageGenerationJob(ctx, waiting)
	if job.Status != ImageJobQueued {
		t.Fatal("queued job lost during recovery")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db2, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err = db2.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	ids, err := db2.QueuedImageJobIDs(ctx, 2)
	if err != nil || len(ids) != 1 || ids[0] != waiting {
		t.Fatalf("restart lost queue: %v %v", ids, err)
	}
}
