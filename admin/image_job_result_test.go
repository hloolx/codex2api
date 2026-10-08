package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/signedasset"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func getImageJobResult(t *testing.T, router *gin.Engine, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func assertImageJobResultFields(t *testing.T, body []byte) {
	t.Helper()
	if len(body) > 4096 {
		t.Fatalf("result response unexpectedly large: %d bytes", len(body))
	}
	for _, field := range []string{"prompt", "params_json", "api_key_id", "api_key_name", "api_key_masked"} {
		if gjson.GetBytes(body, "job."+field).Exists() {
			t.Errorf("result must omit %s", field)
		}
	}
	for _, asset := range gjson.GetBytes(body, "job.assets").Array() {
		for _, field := range []string{"revised_prompt", "cache_b64_json", "storage_path"} {
			if asset.Get(field).Exists() {
				t.Errorf("result asset must omit %s", field)
			}
		}
	}
}

func TestExternalImageJobResultOmitsLargeInputsAndPreservesLegacyQuery(t *testing.T) {
	router, _, db := newExternalImageJobRouter(t, database.APIKeyLimits{}, "sk-result-other")
	ctx := context.Background()
	ownerID, err := db.InsertAPIKey(ctx, "result-owner", "sk-result-owner")
	if err != nil {
		t.Fatal(err)
	}
	params := `{"input_images":["data:image/png;base64,` + strings.Repeat("A", 11<<20) + `"]}`
	jobID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt: "private input prompt", ParamsJSON: params, APIKeyID: ownerID,
		APIKeyName: "result-owner", APIKeyMasked: "sk-result...owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/images/jobs/" + strconv.FormatInt(jobID, 10)
	check := func(status string) []byte {
		t.Helper()
		response := getImageJobResult(t, router, path+"/result?include_cache=1", "sk-result-owner")
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		body := response.Body.Bytes()
		assertImageJobResultFields(t, body)
		if gjson.GetBytes(body, "job.status").String() != status {
			t.Fatalf("unexpected job status: %s", body)
		}
		return body
	}
	queued := check(database.ImageJobQueued)
	if gjson.GetBytes(queued, "job.assets").Raw != "[]" {
		t.Fatal("queued assets must be an empty array")
	}
	if err := db.MarkImageJobRunning(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	check(database.ImageJobRunning)
	assetID, err := db.InsertImageAsset(ctx, database.ImageAssetInput{
		JobID: jobID, Filename: "result.png", StoragePath: "not-read-by-result-api.png",
		MimeType: "image/png", Bytes: 1234, Width: 1024, Height: 1536,
		Model: "gpt-image-2", OutputFormat: "png", RevisedPrompt: "private revised prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkImageJobSucceededWithWarning(ctx, jobID, "one output warning", 45000); err != nil {
		t.Fatal(err)
	}
	completed := check(database.ImageJobSucceeded)
	if gjson.GetBytes(completed, "job.warning").String() != "one output warning" ||
		gjson.GetBytes(completed, "job.error_message").String() != "" {
		t.Fatalf("warning mapping lost: %s", completed)
	}
	asset := gjson.GetBytes(completed, "job.assets.0")
	if asset.Get("id").Int() != assetID || asset.Get("width").Int() != 1024 || asset.Get("bytes").Int() != 1234 {
		t.Fatalf("output metadata lost: %s", completed)
	}
	link, err := url.Parse(asset.Get("proxy_url").String())
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(link.Query().Get("exp"), 10, 64)
	if err != nil || !signedasset.VerifyImageAssetURL(assetID, exp, 0, link.Query().Get("sig"), time.Now()) {
		t.Fatal("result must retain a valid signed image download URL")
	}
	projection, err := db.GetImageGenerationJobResult(ctx, jobID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Prompt != "" || projection.ParamsJSON != "" || projection.APIKeyName != "" || projection.APIKeyMasked != "" {
		t.Fatal("lightweight database query loaded private input/display fields")
	}
	legacy := getImageJobResult(t, router, path, "sk-result-owner")
	if legacy.Code != http.StatusOK || gjson.GetBytes(legacy.Body.Bytes(), "job.params_json").String() != params ||
		gjson.GetBytes(legacy.Body.Bytes(), "job.prompt").String() != "private input prompt" ||
		gjson.GetBytes(legacy.Body.Bytes(), "job.api_key_id").Int() != ownerID {
		t.Fatal("legacy endpoint must retain its full response")
	}
	if err := db.MarkImageJobFailed(ctx, jobID, "provider failed", 123); err != nil {
		t.Fatal(err)
	}
	failed := check(database.ImageJobFailed)
	if gjson.GetBytes(failed, "job.error_message").String() != "provider failed" {
		t.Fatal("failure reason lost")
	}
	for _, tc := range []struct {
		name, target, key string
		want              int
	}{
		{"other owner", path + "/result", "sk-result-other", http.StatusNotFound},
		{"missing key", path + "/result", "", http.StatusUnauthorized},
		{"unknown job", "/v1/images/jobs/999999/result", "sk-result-owner", http.StatusNotFound},
		{"invalid id", "/v1/images/jobs/not-an-id/result", "sk-result-owner", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := getImageJobResult(t, router, tc.target, tc.key)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestImageJobResultPayloadExcludesCachedImagesAndPrompts(t *testing.T) {
	job := &database.ImageGenerationJob{
		ID: 42, Status: database.ImageJobSucceeded, Prompt: "secret prompt", ParamsJSON: "secret inputs",
		APIKeyID: 8, APIKeyName: "secret key name", APIKeyMasked: "secret masked key",
		Assets: []database.ImageAsset{{ID: 9, ProxyURL: "/p/img/9?exp=1&sig=test", CacheB64JSON: strings.Repeat("A", 11<<20), RevisedPrompt: "secret revision"}},
	}
	body, err := json.Marshal(gin.H{"job": imageJobResultPayload(job)})
	if err != nil {
		t.Fatal(err)
	}
	assertImageJobResultFields(t, body)
	if strings.Contains(string(body), "secret") {
		t.Fatal("private fields leaked into result")
	}
}

func TestExternalImageJobBatchResultsOwnershipAndBounds(t *testing.T) {
	router, _, db := newExternalImageJobRouter(t, database.APIKeyLimits{}, "sk-batch-owner")
	ctx := context.Background()
	key, err := db.GetAPIKeyByValue(ctx, "sk-batch-owner")
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: key.ID, Prompt: "private", ParamsJSON: `{"input_images":["secret-base64"]}`})
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: key.ID + 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/v1/images/jobs/results", "/v1/images/jobs/result"} {
		body := fmt.Sprintf(`{"ids":[%d,%d,%d]}`, ownerID, otherID, ownerID)
		w := retentionRequest(router, "POST", endpoint, "sk-batch-owner", strings.NewReader(body))
		var result externalImageJobResultsResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Jobs) != 1 || result.Jobs[0].ID != ownerID || len(result.MissingIDs) != 1 || result.MissingIDs[0] != otherID {
			t.Fatal("invalid batch", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-base64") || strings.Contains(w.Body.String(), "params_json") {
			t.Fatal("batch leaked input")
		}
	}
	for _, body := range []string{`{"ids":[]}`, `{"ids":[0]}`, `{"ids":[` + strings.Repeat("1,", 500) + `1]}`, `{"ids":[1],"padding":"` + strings.Repeat("a", 33<<10) + `"}`} {
		w := retentionRequest(router, "POST", "/v1/images/jobs/results", "sk-batch-owner", strings.NewReader(body))
		if w.Code != http.StatusBadRequest {
			t.Fatal("accepted invalid batch", w.Code)
		}
	}
}
