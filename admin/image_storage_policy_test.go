package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func configureRetentionTestStorage(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("IMAGE_ASSET_DIR", root)
	previous := imagestore.CurrentConfig()
	t.Cleanup(func() { _ = imagestore.Configure(previous) })
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: root}); err != nil {
		t.Fatal(err)
	}
}

func TestImageStoragePolicyValidation(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		seconds    int64
		valid      bool
	}{
		{"default", "", 0, true}, {"legacy", "legacy", 0, true},
		{"two hours", "temporary", 7200, true}, {"three days", "temporary", 259200, true},
		{"read cleanup", "delete_after_read", 0, true},
		{"negative", "temporary", -1, false}, {"too short", "temporary", 59, false},
		{"too long", "temporary", maxImageRetentionSeconds + 1, false},
		{"ambiguous", "legacy", 7200, false}, {"unknown", "never", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := imageGenerationJobPayload{Prompt: "test", StorageMode: test.mode, RetentionSeconds: test.seconds}
			_, err := normalizeExternalImageJobFields(&req)
			if (err == nil) != test.valid {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestImageStoragePolicyDeliveryAndAcknowledgement(t *testing.T) {
	for _, mode := range []string{"", imageStorageLegacy, imageStorageTemporary, imageStorageDeleteAfterRead} {
		for _, ack := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ack=%t", mode, ack), func(t *testing.T) {
				configureRetentionTestStorage(t)
				router, _, db := newExternalImageJobRouter(t, database.APIKeyLimits{}, "sk-retention")
				ctx := context.Background()
				key, err := db.GetAPIKeyByValue(ctx, "sk-retention")
				if err != nil {
					t.Fatal(err)
				}
				jobID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: key.ID})
				if err != nil {
					t.Fatal(err)
				}
				h := &Handler{db: db}
				req := imageGenerationJobPayload{StorageMode: mode, StrictSize: new(bool)}
				if err := normalizeImageStoragePolicy(&req); err != nil {
					t.Fatal(err)
				}
				encoded := base64.StdEncoding.EncodeToString(tinyPNG(t))
				assets, _, err := h.saveImageJobAssets(ctx, jobID, req, []byte(`{"data":[{"b64_json":"`+encoded+`"}]}`))
				if err != nil || len(assets) != 1 {
					t.Fatalf("save: %v", err)
				}
				asset := assets[0]
				if (asset.ExpiresAt > 0) != (mode == imageStorageTemporary || mode == imageStorageDeleteAfterRead) {
					t.Fatal("unexpected expiry", asset.ExpiresAt)
				}
				if err := db.MarkImageJobSucceeded(ctx, jobID, 1); err != nil {
					t.Fatal(err)
				}
				path := fmt.Sprintf("/v1/images/jobs/%d", jobID)
				for _, suffix := range []string{"", "/result", "?include_cache=1"} {
					w := retentionRequest(router, "GET", path+suffix, "sk-retention", nil)
					if w.Code != http.StatusOK {
						t.Fatal(w.Code, w.Body.String())
					}
				}
				if _, err := os.Stat(asset.StoragePath); err != nil {
					t.Fatal("poll consumed output", err)
				}
				if _, err := db.InsertAPIKey(ctx, "other", "sk-other-retention"); err != nil {
					t.Fatal(err)
				}
				for _, route := range []struct{ method, suffix string }{{"GET", "/output"}, {"POST", "/ack"}} {
					w := retentionRequest(router, route.method, path+route.suffix, "sk-other-retention", nil)
					if w.Code != http.StatusNotFound {
						t.Fatal("cross-key access", w.Code)
					}
				}
				if ack {
					for i := 0; i < 2; i++ {
						w := retentionRequest(router, "POST", path+"/ack", "sk-retention", nil)
						if w.Code != http.StatusNoContent {
							t.Fatal(w.Code, w.Body.String())
						}
					}
				} else {
					if mode == imageStorageDeleteAfterRead {
						request := httptest.NewRequest("GET", path+"/output", nil)
						request.Header.Set("Authorization", "Bearer sk-retention")
						router.ServeHTTP(&failingImageHTTPWriter{header: make(http.Header)}, request)
						if _, err := db.GetImageAsset(ctx, asset.ID); err != nil {
							t.Fatal("failed delivery consumed metadata", err)
						}
						if _, err := os.Stat(asset.StoragePath); err != nil {
							t.Fatal("failed delivery consumed file", err)
						}
					}
					w := retentionRequest(router, "GET", path+"/output", "sk-retention", nil)
					var output struct {
						Data []struct {
							B64 string `json:"b64_json"`
						} `json:"data"`
					}
					if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &output) != nil || len(output.Data) != 1 || output.Data[0].B64 != encoded {
						t.Fatal("invalid output", w.Code, w.Body.String())
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("cacheable output")
					}
				}
				_, err = db.GetImageAsset(ctx, asset.ID)
				if mode == imageStorageDeleteAfterRead {
					if !errors.Is(err, sql.ErrNoRows) {
						t.Fatal("metadata retained", err)
					}
					if _, err := os.Stat(asset.StoragePath); !os.IsNotExist(err) {
						t.Fatal("file retained", err)
					}
				} else if err != nil {
					t.Fatal("non-consuming mode was deleted", err)
				}
				if _, err := db.GetImageGenerationJob(ctx, jobID); err != nil {
					t.Fatal("job history deleted", err)
				}
			})
		}
	}
}

func retentionRequest(router *gin.Engine, method, path, key string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func TestImageStorageExpiryAndDeletionRetry(t *testing.T) {
	configureRetentionTestStorage(t)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	ctx := context.Background()
	id, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{})
	if err != nil {
		t.Fatal(err)
	}
	req := imageGenerationJobPayload{StorageMode: imageStorageTemporary, RetentionSeconds: 7200, StrictSize: new(bool)}
	assets, _, err := h.saveImageJobAssets(ctx, id, req, []byte(`{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(tinyPNG(t))+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	asset := assets[0]
	expires := time.Unix(asset.ExpiresAt, 0)
	if err := h.cleanupDueImageAssets(ctx, expires.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(asset.StoragePath); err != nil {
		t.Fatal("early cleanup", err)
	}
	asset.ExpiresAt = time.Now().Add(-time.Second).Unix()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/image.png", nil)
	h.serveImageAssetFile(c, &asset, imageAssetFileOptions{})
	if w.Code != http.StatusGone {
		t.Fatal("expired image served", w.Code)
	}
	if err := h.cleanupDueImageAssets(ctx, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetImageAsset(ctx, asset.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("expired row retained", err)
	}
	if _, err := os.Stat(asset.StoragePath); !os.IsNotExist(err) {
		t.Fatal("expired file retained", err)
	}
	badID, err := db.InsertImageAsset(ctx, database.ImageAssetInput{JobID: id, StoragePath: "s3://unconfigured-bucket/image.png", ExpiresAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cleanupDueImageAssets(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetImageAsset(ctx, badID); err != nil {
		t.Fatal("lost deletion retry metadata", err)
	}
}

type failingImageOutputWriter struct{}

func (failingImageOutputWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type failingImageHTTPWriter struct{ header http.Header }

func (w *failingImageHTTPWriter) Header() http.Header     { return w.header }
func (*failingImageHTTPWriter) WriteHeader(int)           {}
func (*failingImageHTTPWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestImageOutputStreamingDetectsIncompleteDelivery(t *testing.T) {
	job := &database.ImageGenerationJob{ID: 1, Assets: []database.ImageAsset{{ID: 2, Bytes: 3}}}
	for _, w := range []io.Writer{failingImageOutputWriter{}, &bytes.Buffer{}} {
		err := streamImageJobOutput(context.Background(), w, job, []io.ReadCloser{io.NopCloser(strings.NewReader("x"))})
		if err == nil {
			t.Fatal("partial delivery accepted")
		}
	}
}

func TestPipelineImageStoragePolicy(t *testing.T) {
	configureRetentionTestStorage(t)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := tinyPNG(t)
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	_, err = h.savePipelineImage(ctx, id, imageGenerationJobPayload{StorageMode: imageStorageTemporary, RetentionSeconds: 7200, StrictSize: new(bool)}, proxy.QueuedImageResult{File: f, Bytes: int64(len(data)), Width: 1, Height: 1, Format: "png"})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := db.ListImageAssetsByJobID(ctx, id)
	if err != nil || len(assets) != 1 || assets[0].ExpiresAt < time.Now().Add(7190*time.Second).Unix() {
		t.Fatal("pipeline expiry missing", err)
	}
}
