package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
)

func TestImagePipelineQueueSavesEachOutputWithoutBase64Aggregation(t *testing.T) {
	t.Setenv("IMAGE_JOB_WORKERS", "2")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	root := t.TempDir()
	t.Setenv("IMAGE_ASSET_DIR", root)
	previousStorage := imagestore.CurrentConfig()
	t.Cleanup(func() { _ = imagestore.Configure(previousStorage) })
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: root}); err != nil {
		t.Fatal(err)
	}
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previous)
		proxy.SetResinConfig(nil)
		proxy.ConfigureImagePipeline(0)
		proxy.ConfigureImageExecutionLimit(0)
	})
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	proxy.ApplyRuntimeSettings(settings)
	var calls atomic.Int32
	encoded := base64.StdEncoding.EncodeToString(tinyPNG(t))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"`+encoded+`","output_format":"png"}]}}`+"\n\n")
	}))
	defer upstream.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "queue-pipeline-test"})
	db := newTestAdminDB(t)
	ctx := context.Background()
	if err := db.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	keyID, err := db.InsertAPIKey(ctx, "queue-pipeline", "sk-pipeline")
	if err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "pipeline-test", PlanType: "plus"})
	h := NewHandler(store, db, tc, nil, "")
	h.imageProxy = proxy.NewHandler(store, db, nil, nil)
	h.imageQueue = &imageJobQueue{pipeline: true}
	proxy.ConfigureImagePipeline(1)
	proxy.ConfigureImageExecutionLimit(2)
	strict := false
	req := imageGenerationJobPayload{Model: "gpt-image-2", Prompt: "draw a cat", Size: "auto", N: 2, OutputFormat: "png", StrictSize: &strict, InputImages: []string{"data:image/png;base64," + encoded}}
	if err := prepareQueueInputs(&req); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(req)
	id, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: req.Prompt, ParamsJSON: string(params), APIKeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	jobCtx, release, err := proxy.AcquireImageExecution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	h.runNextQueuedImage(jobCtx)
	job, err := db.GetImageGenerationJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != database.ImageJobSucceeded || len(job.Assets) != 2 || calls.Load() != 2 {
		t.Fatalf("status=%s assets=%d calls=%d error=%s", job.Status, len(job.Assets), calls.Load(), job.ErrorMessage)
	}
	if strings.Contains(job.ParamsJSON, "base64") {
		t.Fatal("persisted input expanded")
	}
	for _, asset := range job.Assets {
		data, err := os.ReadFile(asset.StoragePath)
		if err != nil || string(data) != string(tinyPNG(t)) {
			t.Fatal("image changed during stream save", err)
		}
	}
	entries, err := os.ReadDir(root + "/pipeline-tmp")
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary files leaked", entries, err)
	}
}
