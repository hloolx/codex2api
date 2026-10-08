package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestQualityTestJobsSurviveRequestCancellationAndLimitThreeAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html><svg>\"}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"</svg></html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	parent, stop := context.WithCancel(context.Background())
	h.StartQualityTests(parent)
	defer func() { stop(); h.WaitQualityTests() }()
	router := gin.New()
	router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	other := &Handler{db: db, store: store}
	router.POST("/quality-tests/:id/cancel", other.CancelQualityTest)
	var ids []int64
	for i := 0; i < 4; i++ {
		id, err := db.InsertAccountWithCredentials(context.Background(), fmt.Sprintf("account-%d", i), map[string]interface{}{"upstream_type": "openai_responses", "api_key": "test-key", "models": []string{"gpt-4o-mini"}, "base_url": upstream.URL}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "test-key", BaseURL: upstream.URL, Models: []string{"gpt-4o-mini"}, PlanType: "pro", Status: auth.StatusReady})
	}
	create := func(id int64) (*httptest.ResponseRecorder, database.QualityTestJob) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(`{"model":"gpt-4o-mini","reasoning_effort":"high","prompt":"HTML"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		cancel()
		var body struct {
			Job database.QualityTestJob `json:"job"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		return response, body.Job
	}
	var jobs []database.QualityTestJob
	for _, id := range ids[:3] {
		response, job := create(id)
		if response.Code != http.StatusAccepted {
			t.Fatalf("create=%d %s", response.Code, response.Body.String())
		}
		jobs = append(jobs, job)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("detached job did not reach upstream")
		}
	}
	if response, _ := create(ids[3]); response.Code != http.StatusConflict {
		t.Fatalf("fourth task accepted: %d", response.Code)
	}
	if response, _ := create(ids[0]); response.Code != http.StatusConflict {
		t.Fatalf("duplicate account accepted: %d", response.Code)
	}
	waitStatus := func(id int64, want string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			job, err := db.GetQualityTestJob(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if job.Status == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("job %d did not reach %s", id, want)
	}
	// All submitting HTTP contexts are cancelled; records still run and keep partial output.
	time.Sleep(1100 * time.Millisecond)
	for _, job := range jobs {
		stored, err := db.GetQualityTestJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "running" || stored.Output != "<html><svg>" {
			t.Fatalf("request cancellation stopped job/progress: %+v", stored)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/quality-tests/%d/cancel", jobs[0].ID), nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	waitStatus(jobs[0].ID, "stopped")
	if response, _ := create(ids[3]); response.Code != http.StatusAccepted {
		t.Fatalf("released slot unavailable: %d %s", response.Code, response.Body.String())
	}
	close(release)
	for _, job := range jobs[1:] {
		waitStatus(job.ID, "completed")
		stored, _ := db.GetQualityTestJob(context.Background(), job.ID)
		if stored.Output != "<html><svg></svg></html>" || stored.ReasoningEffort != "high" || stored.AccountName == "" || stored.CompletedAt == nil {
			t.Fatalf("missing persisted result: %+v", stored)
		}
	}
}

func TestQualityTestJobRetriesOnlyBreaksBeforeOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDelay := qualityTestRetryDelay
	qualityTestRetryDelay = 0
	defer func() { qualityTestRetryDelay = previousDelay }()
	cases := []struct {
		name        string
		attempt     func(n int, w http.ResponseWriter)
		wantStatus  string
		wantOutput  string
		wantRetries int
		wantBreak   bool
		wantCalls   int
	}{{
		name: "break before output retries",
		attempt: func(n int, w http.ResponseWriter) {
			if n == 1 {
				panic(http.ErrAbortHandler)
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html></html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		},
		wantStatus: "completed", wantOutput: "<html></html>", wantRetries: 1, wantCalls: 2,
	}, {
		name: "break after output keeps partial",
		attempt: func(_ int, w http.ResponseWriter) {
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html><svg>\"}\n\n")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		},
		wantStatus: "error", wantOutput: "<html><svg>", wantBreak: true, wantCalls: 1,
	}, {
		name: "repeated break gives up after one retry",
		attempt: func(_ int, _ http.ResponseWriter) {
			panic(http.ErrAbortHandler)
		},
		wantStatus: "error", wantRetries: 1, wantBreak: true, wantCalls: 2,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				w.Header().Set("Content-Type", "text/event-stream")
				tc.attempt(n, w)
			}))
			defer upstream.Close()
			db := newTestAdminDB(t)
			store := auth.NewStore(db, nil, nil)
			h := &Handler{db: db, store: store}
			parent, stop := context.WithCancel(context.Background())
			h.StartQualityTests(parent)
			defer func() { stop(); h.WaitQualityTests() }()
			id, err := db.InsertAccountWithCredentials(context.Background(), "account", map[string]interface{}{"upstream_type": "openai_responses", "api_key": "test-key", "models": []string{"gpt-4o-mini"}, "base_url": upstream.URL}, "")
			if err != nil {
				t.Fatal(err)
			}
			store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "test-key", BaseURL: upstream.URL, Models: []string{"gpt-4o-mini"}, PlanType: "pro", Status: auth.StatusReady})
			router := gin.New()
			router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(`{"model":"gpt-4o-mini","prompt":"HTML"}`))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusAccepted {
				t.Fatalf("create=%d %s", response.Code, response.Body.String())
			}
			var body struct {
				Job database.QualityTestJob `json:"job"`
			}
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			var stored *database.QualityTestJob
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if stored, err = db.GetQualityTestJob(context.Background(), body.Job.ID); err != nil {
					t.Fatal(err)
				}
				if stored.Status != "running" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if stored.Status != tc.wantStatus || stored.Output != tc.wantOutput || stored.Retries != tc.wantRetries || stored.Interrupted != tc.wantBreak || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("got status=%s output=%q retries=%d interrupted=%v calls=%d error=%q", stored.Status, stored.Output, stored.Retries, stored.Interrupted, calls.Load(), stored.Error)
			}
		})
	}
}

func TestQualityTestJobTimeoutBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	parent, stop := context.WithCancel(context.Background())
	h.StartQualityTests(parent)
	defer func() { stop(); h.WaitQualityTests() }()
	router := gin.New()
	router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	for _, tc := range []struct {
		body     string
		wantCode int
		wantMS   int64
	}{
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":40}`, http.StatusBadRequest, 0},
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":2}`, http.StatusBadRequest, 0},
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":30}`, http.StatusAccepted, (30 * time.Minute).Milliseconds()},
		{`{"model":"gpt-4o-mini","prompt":"HTML"}`, http.StatusAccepted, database.QualityTestDefaultTimeout.Milliseconds()},
	} {
		id, err := db.InsertAccountWithCredentials(context.Background(), "account", map[string]interface{}{"upstream_type": "openai_responses", "api_key": "test-key", "models": []string{"gpt-4o-mini"}, "base_url": "http://127.0.0.1:1"}, "")
		if err != nil {
			t.Fatal(err)
		}
		store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "test-key", BaseURL: "http://127.0.0.1:1", Models: []string{"gpt-4o-mini"}, PlanType: "pro", Status: auth.StatusReady})
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != tc.wantCode {
			t.Fatalf("%s: code=%d %s", tc.body, response.Code, response.Body.String())
		}
		if tc.wantCode != http.StatusAccepted {
			continue
		}
		var body struct {
			Job database.QualityTestJob `json:"job"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		stored, err := db.GetQualityTestJob(context.Background(), body.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if body.Job.TimeoutMS != tc.wantMS || stored.TimeoutMS != tc.wantMS {
			t.Fatalf("%s: timeout created=%d stored=%d want=%d", tc.body, body.Job.TimeoutMS, stored.TimeoutMS, tc.wantMS)
		}
	}
}
