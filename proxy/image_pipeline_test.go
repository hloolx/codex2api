package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestImagePipelineUploadsContinueWhileEarlierJobsWait(t *testing.T) {
	previousRuntime, previousResin := CurrentRuntimeSettings(), resinCfg.Load()
	t.Cleanup(func() {
		ApplyRuntimeSettings(previousRuntime)
		resinCfg.Store(previousResin)
		ConfigureImagePipeline(0)
		ConfigureImageExecutionLimit(0)
	})
	settings := previousRuntime
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	ConfigureImageExecutionLimit(10)
	ConfigureImagePipeline(1)
	received := make(chan bool, 10)
	respond := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(respond) }) }
	defer unblock()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- validatePipelineUpload(r)
		select {
		case <-respond:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"`+tinyPNGBase64+`","output_format":"png"}]}}`+"\n\n")
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "pipeline-test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 20, TestConcurrency: 20, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "pipeline-test", PlanType: "plus"})
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(id int64) {
			jobCtx, release, err := AcquireImageExecution(ctx)
			if err != nil {
				results <- err
				return
			}
			defer release()
			jobCtx, p, err := NewImagePipeline(jobCtx, root, id)
			if err != nil {
				results <- err
				return
			}
			p.Output = func(_ context.Context, result QueuedImageResult) error {
				bytes, err := io.ReadAll(result.File)
				if err != nil {
					return err
				}
				want, ok := decodeImageBase64(tinyPNGBase64)
				if !ok || string(bytes) != string(want) {
					return fmt.Errorf("saved image bytes changed")
				}
				return nil
			}
			result, status, err := h.GenerateQueuedImage(jobCtx, largePipelineInput, true, nil)
			if err == nil && (status != 200 || !strings.Contains(string(result), `"saved":true`) || strings.Contains(string(result), tinyPNGBase64)) {
				err = fmt.Errorf("unexpected output %d", status)
			}
			// Completion includes cleanup; a buffered send before deferred Close
			// lets the assertion race the final worker's file removal.
			p.Close()
			results <- err
		}(int64(i))
	}
	for i := 0; i < 10; i++ {
		select {
		case valid := <-received:
			if !valid {
				t.Error("request bytes changed")
			}
		case <-ctx.Done():
			unblock()
			t.Fatal("later uploads blocked behind earlier responses")
		}
	}
	runtime.GC()
	var waiting runtime.MemStats
	runtime.ReadMemStats(&waiting)

	growth := int64(waiting.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("ten waiting requests, each 8 MiB reference: live heap growth=%d", growth)
	// Ten retained input copies alone would exceed 80 MiB. Allow transport/GC
	// metadata headroom but reject payload retention across the waiting phase.
	if growth > 24<<20 {
		t.Errorf("waiting requests retain large input buffers: %d", growth)
	}
	unblock()
	for i := 0; i < 10; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	entries, err := os.ReadDir(root + "/pipeline-tmp")
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}
func largePipelineInput() ([]byte, error) {
	return []byte(`{"model":"gpt-image-2","prompt":"pipeline memory test","images":[{"image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"}]}`), nil
}
func validatePipelineUpload(r *http.Request) bool {
	b := readUpstreamRequestBody(r)
	return len(gjson.GetBytes(b, "input.0.content.1.image_url").String()) == (8<<20)+len("data:image/png;base64,")
}

func TestImagePipelineResponseSpoolsLargeLineAndStopsAtTerminal(t *testing.T) {
	ConfigureImagePipeline(1)
	defer ConfigureImagePipeline(0)
	ctx, p, err := NewImagePipeline(context.Background(), t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	payload := `event: response.completed` + "\n" + `data: {"type":"response.completed","result":"` + strings.Repeat("A", 2<<20) + `"}` + "\n\n"
	done := make(chan error, 1)
	go func() { _, e := io.WriteString(w, payload); done <- e }()
	file, err := p.collectResponse(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	defer removePipelineFile(file)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil || string(got) != payload {
		t.Fatal("response truncated or changed", err)
	}
	_ = w.Close()
}

func TestImagePipelineCancellationAndLocalFailureDoNotLeakOrRetry(t *testing.T) {
	ConfigureImagePipeline(1)
	defer ConfigureImagePipeline(0)
	root := t.TempDir()
	ctx, p, err := NewImagePipeline(context.Background(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	otherCtx, other, err := NewImagePipeline(ctx, root, 2)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(otherCtx)
	cancel()
	if err = other.Acquire(cancelled); err == nil {
		t.Fatal("cancelled waiter acquired busy gate")
	}
	other.Close()
	p.Release()
	if err = other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	other.Release()
	p.Output = func(context.Context, QueuedImageResult) error { return fmt.Errorf("disk full") }
	_, err = p.saveResults(ctx, []imageCallResult{{Result: tinyPNGBase64}})
	if !isPipelineOutputError(err) {
		t.Fatalf("missing local failure: %v", err)
	}
	retries := 0
	if shouldRetryImageStreamError(err, &retries, 5, 0, 5) {
		t.Fatal("local storage failure resubmits paid generation")
	}
	p.Close()
	entries, err := os.ReadDir(root + "/pipeline-tmp")
	if err != nil || len(entries) != 0 {
		t.Fatal("cleanup", entries, err)
	}
}

func TestImagePipelineRequestReplayPreservesReferenceAndModelChanges(t *testing.T) {
	ConfigureImagePipeline(1)
	defer ConfigureImagePipeline(0)
	ctx, p, err := NewImagePipeline(context.Background(), t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	file, err := p.spool([]byte(`{"model":"first","input":[{"image_url":"data:image/png;base64,abc"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer removePipelineFile(file)
	for _, model := range []string{"first", "fallback"} {
		_, err = executePipelineImage(ctx, file, model, func(body []byte) (*http.Response, error) {
			if gjson.GetBytes(body, "model").String() != model || gjson.GetBytes(body, "input.0.image_url").String() != "data:image/png;base64,abc" {
				t.Fatal("replay altered image")
			}
			p.Release()
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestImagePipelineOrphanCleanupKeepsFreshFiles(t *testing.T) {
	ConfigureImagePipeline(1)
	defer ConfigureImagePipeline(0)
	root := t.TempDir()
	ctx, p, err := NewImagePipeline(context.Background(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	old := root + "/pipeline-tmp/job-old"
	if err = os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(old+"/body", []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	count, err := CleanupImagePipelineFiles(ctx, root, time.Now().Add(-24*time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("%d %v", count, err)
	}
	if _, err = os.Stat(p.dir); err != nil {
		t.Fatal("active directory removed")
	}
}
