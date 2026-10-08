package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newGrokMediaBillingHandler(t *testing.T, upstreamURL string) (*Handler, *database.DB, int64) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keyID, err := db.InsertAPIKey(context.Background(), "media", "sk-media-billing-test")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstreamURL})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, db, keyID
}

func grokMediaBillingContext(method, target string, body []byte, keyID int64) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	ctx.Set(contextAPIKeyID, keyID)
	return ctx, rec
}

func grokMediaUsageLogs(t *testing.T, db *database.DB, since time.Time) []*database.UsageLog {
	t.Helper()
	db.FlushUsageLogs()
	logs, err := db.ListUsageLogsByTimeRange(context.Background(), since.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// 视频任务在状态查询首次看到 done 时结算一次:按上游秒数计成本,用户按秒计费,
// 重复轮询不重复扣费;提交行金额为 0。
func TestGrokVideoSettlesOnceOnDone(t *testing.T) {
	database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{
		"grok-imagine-video-1.5": {Source: database.ModelPricingSourceCustom, UserBillingMode: database.UserBillingModePerSecond, ImageUnitPrice: 0.1},
	})
	t.Cleanup(func() { database.SetModelPricingOverrides(nil) })

	var polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
			_, _ = w.Write([]byte(`{"request_id":"video_bill_1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_bill_1":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"pending","progress":40}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"done","progress":100,"model":"grok-imagine-video-1.5","video":{"url":"https://vidgen.x.ai/v.mp4","duration":10}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	handler, db, keyID := newGrokMediaBillingHandler(t, upstream.URL)
	before := time.Now()

	ctx, rec := grokMediaBillingContext(http.MethodPost, "/v1/videos/generations", []byte(`{"model":"grok-imagine-video-1.5","prompt":"waves","duration":10}`), keyID)
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d body = %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 4; i++ {
		ctx, rec = grokMediaBillingContext(http.MethodGet, "/v1/videos/video_bill_1", nil, keyID)
		ctx.Params = gin.Params{{Key: "request_id", Value: "video_bill_1"}}
		handler.VideosStatus(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll %d status = %d body = %s", i, rec.Code, rec.Body.String())
		}
	}

	logs := grokMediaUsageLogs(t, db, before)
	if len(logs) != 2 {
		t.Fatalf("usage rows = %d, want submit + settlement", len(logs))
	}
	var settled *database.UsageLog
	for _, l := range logs {
		if l.VideoSeconds > 0 {
			if settled != nil {
				t.Fatal("video settled twice")
			}
			settled = l
		} else if l.AccountBilled != 0 || l.UserBilled != 0 {
			t.Fatalf("submit row must be free: %+v", l)
		}
	}
	if settled == nil {
		t.Fatal("no settlement row")
	}
	if settled.VideoSeconds != 10 || !approxFloat(settled.AccountBilled, 0.8) || !approxFloat(settled.UserBilled, 1.0) ||
		settled.UserBillingMode != database.UserBillingModePerSecond || settled.BilledImageCount != 10 {
		t.Fatalf("settlement row = %+v", settled)
	}
	key, err := db.GetAPIKeyByID(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if !approxFloat(key.QuotaUsed, 1.0) {
		t.Fatalf("api key quota used = %v, want 1.0", key.QuotaUsed)
	}
}

// 失败任务不计费,但留一条失败记录;上游以非 2xx 返回 failed 状态体同样结算。
func TestGrokVideoFailedTaskIsNotBilled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"request_id":"video_fail_1"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"failed","error":{"code":"service_unavailable","message":"engine overloaded"}}`))
		}
	}))
	defer upstream.Close()
	handler, db, keyID := newGrokMediaBillingHandler(t, upstream.URL)
	before := time.Now()

	ctx, _ := grokMediaBillingContext(http.MethodPost, "/v1/videos/generations", []byte(`{"model":"grok-imagine-video","prompt":"x"}`), keyID)
	handler.VideosGenerations(ctx)
	for i := 0; i < 2; i++ {
		ctx, _ = grokMediaBillingContext(http.MethodGet, "/v1/videos/video_fail_1", nil, keyID)
		ctx.Params = gin.Params{{Key: "request_id", Value: "video_fail_1"}}
		handler.VideosStatus(ctx)
	}
	logs := grokMediaUsageLogs(t, db, before)
	failures := 0
	for _, l := range logs {
		if l.AccountBilled != 0 || l.UserBilled != 0 {
			t.Fatalf("failed task billed: %+v", l)
		}
		if l.UpstreamErrorKind == "video_failed" {
			failures++
			if l.StatusCode != http.StatusServiceUnavailable || l.ErrorMessage != "engine overloaded" {
				t.Fatalf("failure row = %+v", l)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("failure rows = %d, want 1", failures)
	}
}

// 生图按张计上游成本,上游自报成本优先;张数不再冒充输出 token。
func TestGrokImageBilledPerImage(t *testing.T) {
	database.SetModelPricingOverrides(nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://imgen.x.ai/a.png"},{"url":"https://imgen.x.ai/b.png"}],"usage":{"cost_in_usd_ticks":350000000}}`))
	}))
	defer upstream.Close()
	handler, db, keyID := newGrokMediaBillingHandler(t, upstream.URL)
	before := time.Now()

	ctx, rec := grokMediaBillingContext(http.MethodPost, "/v1/images/generations", []byte(`{"model":"grok-imagine-image","prompt":"cat","n":2}`), keyID)
	handler.ImagesGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	logs := grokMediaUsageLogs(t, db, before)
	if len(logs) != 1 {
		t.Fatalf("usage rows = %d", len(logs))
	}
	l := logs[0]
	if l.ImageCount != 2 || l.OutputTokens != 0 || !approxFloat(l.AccountBilled, 0.035) || !approxFloat(l.UserBilled, 0.035) {
		t.Fatalf("image row = %+v", l)
	}
}

func TestParseXAIImaginePricing(t *testing.T) {
	body := []byte(`
### Text API Pricing

| Model | Context | Input / 1M tokens | Cached input / 1M tokens | Output / 1M tokens |
| --- | --- | --- | --- | --- |
| grok-4.7 (< 200k prompt tokens) | 500k | $2.00 | $0.50 | $6.00 |

### Imagine Pricing

| Model | Cost |
| --- | --- |
| grok-imagine-image-quality | $0.05 / image |
| grok-imagine-image | $0.02 / image |
| grok-imagine-video-1.5 | $0.080 / sec |
| grok-imagine-video | $0.50 / image |
`)
	got, err := ParseXAIOfficialPricingMarkdown(body)
	if err != nil {
		t.Fatal(err)
	}
	if got["grok-imagine-image"].MediaUnitCost != 0.02 || got["grok-imagine-image-quality"].MediaUnitCost != 0.05 || got["grok-imagine-video-1.5"].MediaUnitCost != 0.08 {
		t.Fatalf("imagine pricing = %+v", got)
	}
	if _, ok := got["grok-imagine-video"]; ok {
		t.Fatal("unit mismatch row must be skipped")
	}
	if got["grok-4.7"].Input != 2 {
		t.Fatal("text pricing lost")
	}
}

func approxFloat(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
