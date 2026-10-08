package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/liveattestation"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestValidateLiveCallRequest(t *testing.T) {
	if err := ValidateLiveCallRequest(nil); err == nil {
		t.Fatal("nil request should fail")
	}
	if err := ValidateLiveCallRequest(&liveCallRequest{SDP: "", Session: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("empty sdp should fail")
	}
	if err := ValidateLiveCallRequest(&liveCallRequest{SDP: "v=0", Session: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("array session should fail")
	}
	if err := ValidateLiveCallRequest(&liveCallRequest{SDP: "v=0", Session: json.RawMessage(`{"model":"gpt-live"}`)}); err != nil {
		t.Fatalf("valid request: %v", err)
	}
}

func TestLiveCallIDFromLocation(t *testing.T) {
	id, err := liveCallIDFromLocation("https://chatgpt.com/backend-api/codex/rtc_abc")
	if err != nil || id != "rtc_abc" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := liveCallIDFromLocation(""); err == nil {
		t.Fatal("empty location should fail")
	}
	if _, err := liveCallIDFromLocation("/backend-api/codex/"); err == nil {
		t.Fatal("missing call id should fail")
	}
}

func TestEncryptLiveAttestationRoundTrip(t *testing.T) {
	cipher, err := encryptLiveAttestation(`{"v":1,"s":0,"t":"token"}`, "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decryptLiveAttestation(cipher, "admin-secret")
	if err != nil || plain != `{"v":1,"s":0,"t":"token"}` {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
	if _, err := decryptLiveAttestation(cipher, "other"); err == nil {
		t.Fatal("wrong secret should fail")
	}
}

func TestPrepareLiveAttestationPassthrough(t *testing.T) {
	previous := generateLiveAttestation
	t.Cleanup(func() { generateLiveAttestation = previous })
	generateLiveAttestation = func(context.Context) (string, error) {
		t.Fatal("should not generate when the client already sent attestation")
		return "", errors.New("unused")
	}
	handler := NewHandler(nil, nil, &config.Config{AdminSecret: "secret"}, nil)
	plain, cipher, err := handler.prepareLiveAttestation(context.Background(), `{"v":1,"s":0,"t":"client"}`)
	if err != nil {
		t.Fatal(err)
	}
	if plain != `{"v":1,"s":0,"t":"client"}` {
		t.Fatalf("plain=%q", plain)
	}
	got, err := decryptLiveAttestation(cipher, "secret")
	if err != nil || got != plain {
		t.Fatalf("decrypt=%q err=%v", got, err)
	}
}

func TestPrepareLiveAttestationMissingOnUnsupported(t *testing.T) {
	previous := generateLiveAttestation
	t.Cleanup(func() { generateLiveAttestation = previous })
	generateLiveAttestation = func(context.Context) (string, error) {
		return "", liveattestation.ErrUnsupportedPlatform
	}
	handler := NewHandler(nil, nil, &config.Config{AdminSecret: "secret"}, nil)
	_, _, err := handler.prepareLiveAttestation(context.Background(), "")
	if err == nil {
		t.Fatal("expected attestation error")
	}
	var attErr *liveAttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(attErr.Error(), "X-Oai-Attestation") {
		t.Fatalf("message=%q", attErr.Error())
	}
}

func TestPrepareLiveAttestationGenerate(t *testing.T) {
	previous := generateLiveAttestation
	t.Cleanup(func() { generateLiveAttestation = previous })
	generateLiveAttestation = func(context.Context) (string, error) {
		return `{"v":1,"s":0,"t":"generated"}`, nil
	}
	handler := NewHandler(nil, nil, &config.Config{AdminSecret: "secret"}, nil)
	plain, _, err := handler.prepareLiveAttestation(context.Background(), "")
	if err != nil || plain != `{"v":1,"s":0,"t":"generated"}` {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestLiveCreateRequiresAllowLive(t *testing.T) {
	handler := NewHandler(auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1}), nil, nil, nil)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/live", []byte(`{"sdp":"v=0","session":{}}`), &database.APIKeyRow{ID: 9})
	handler.LiveCreate(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API key") || strings.Contains(rec.Body.String(), "group") {
		t.Fatalf("forbidden message should mention the key, not a group: %s", rec.Body.String())
	}
}

func TestLiveCreateForwardsSDPAndLocation(t *testing.T) {
	var gotAttestation string
	var gotAlpha string
	var gotAccept string
	var gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAttestation = r.Header.Get(liveAttestationHeader)
		gotAlpha = r.Header.Get("OpenAI-Alpha")
		gotAccept = r.Header.Get("Accept")
		gotBeta = r.Header.Get("OpenAI-Beta")
		w.Header().Set("Location", "/backend-api/codex/rtc_test_1")
		w.Header().Set("Content-Type", "application/sdp")
		_, _ = w.Write([]byte("v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\n"))
	}))
	t.Cleanup(upstream.Close)
	previousURL := liveCallsURLForTest
	previousGen := generateLiveAttestation
	t.Cleanup(func() {
		liveCallsURLForTest = previousURL
		generateLiveAttestation = previousGen
	})
	liveCallsURLForTest = upstream.URL
	generateLiveAttestation = func(context.Context) (string, error) {
		return `{"v":1,"s":0,"t":"generated"}`, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-live"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))

	body := []byte(`{"sdp":"v=0\r\n","session":{"model":"gpt-live-1"}}`)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/live", body, &database.APIKeyRow{
		ID: 11, Limits: database.APIKeyLimits{AllowLive: true},
	})
	handler.LiveCreate(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "/v1/live/rtc_test_1" {
		t.Fatalf("Location=%q", rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "v=0") {
		t.Fatalf("sdp body=%q", rec.Body.String())
	}
	if gotAttestation != `{"v":1,"s":0,"t":"generated"}` {
		t.Fatalf("attestation=%q", gotAttestation)
	}
	if gotAlpha != "quicksilver=v2" || gotAccept != "application/sdp" || gotBeta != "" {
		t.Fatalf("alpha=%q accept=%q beta=%q", gotAlpha, gotAccept, gotBeta)
	}
	if account.ActiveRequests.Load() != 1 {
		t.Fatalf("account reservation = %d, want 1 until finalize", account.ActiveRequests.Load())
	}
	record, _, err := handler.liveCalls().get(context.Background(), "rtc_test_1")
	if err != nil || record == nil || record.Model != "gpt-live-1" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	handler.liveCalls().finalize(record)
	if account.ActiveRequests.Load() != 0 {
		t.Fatalf("account reservation after finalize = %d", account.ActiveRequests.Load())
	}
}

func TestLiveCreateRejectsRelayAccounts(t *testing.T) {
	previousURL := liveCallsURLForTest
	previousGen := generateLiveAttestation
	t.Cleanup(func() {
		liveCallsURLForTest = previousURL
		generateLiveAttestation = previousGen
	})
	liveCallsURLForTest = "http://127.0.0.1:1/unused"
	generateLiveAttestation = func(context.Context) (string, error) {
		return `{"v":1}`, nil
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai"})
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/live", []byte(`{"sdp":"v=0","session":{}}`), &database.APIKeyRow{
		ID: 3, Limits: database.APIKeyLimits{AllowLive: true},
	})
	handler.LiveCreate(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLiveCreateMissingAttestationIs503(t *testing.T) {
	previous := generateLiveAttestation
	t.Cleanup(func() { generateLiveAttestation = previous })
	generateLiveAttestation = func(context.Context) (string, error) {
		return "", liveattestation.ErrUnsupportedPlatform
	}
	handler := NewHandler(auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1}), nil, nil, nil)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/live", []byte(`{"sdp":"v=0","session":{}}`), &database.APIKeyRow{
		ID: 4, Limits: database.APIKeyLimits{AllowLive: true},
	})
	handler.LiveCreate(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLiveFinalizeIdempotent(t *testing.T) {
	var logs atomic.Int32
	previous := liveUsageLogForTest
	t.Cleanup(func() { liveUsageLogForTest = previous })
	liveUsageLogForTest = func(input *database.UsageLogInput) {
		if input.Endpoint != "/v1/live" || input.Model != "gpt-live" {
			t.Errorf("usage=%+v", input)
		}
		logs.Add(1)
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	account := &auth.Account{DBID: 8, AccessToken: "at"}
	store.AddAccount(account)
	account.ActiveRequests.Store(1)
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	record := &liveCallRecord{
		CallID:     "rtc_final",
		CallHash:   hashLiveCallID("rtc_final"),
		AccountID:  account.ID(),
		APIKeyID:   5,
		LeaseID:    "lease-1",
		Model:      "gpt-live",
		CreatedAt:  time.Now().Add(-2 * time.Second),
		ExpiresAt:  time.Now().Add(time.Hour),
		Controller: liveControllerPending,
	}
	if err := handler.liveCalls().save(context.Background(), record, account, nil); err != nil {
		t.Fatal(err)
	}
	handler.liveCalls().finalize(record)
	handler.liveCalls().finalize(record)
	if logs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", logs.Load())
	}
	if account.ActiveRequests.Load() != 0 {
		t.Fatalf("ActiveRequests=%d", account.ActiveRequests.Load())
	}
}

func TestLiveSidebandForwardsTextAndBinary(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(msgType, append([]byte("up:"), payload...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	previousDial := liveSidebandDialForTest
	t.Cleanup(func() { liveSidebandDialForTest = previousDial })
	liveSidebandDialForTest = func(ctx context.Context, _ *liveCallRecord, _ http.Header) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		return conn, err
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	account := &auth.Account{DBID: 9, AccessToken: "at"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	record := &liveCallRecord{
		CallID:     "rtc_ws",
		CallHash:   hashLiveCallID("rtc_ws"),
		AccountID:  account.ID(),
		APIKeyID:   6,
		LeaseID:    "lease-ws",
		Model:      "gpt-live",
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
		Controller: liveControllerPending,
	}
	cipher, err := encryptLiveAttestation(`{"v":1}`, "secret")
	if err != nil {
		t.Fatal(err)
	}
	record.AttestationCiphertext = cipher
	if err := handler.liveCalls().save(context.Background(), record, account, nil); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/live/:call_id", func(c *gin.Context) {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 6, Limits: database.APIKeyLimits{AllowLive: true}})
		c.Set(contextAPIKeyID, int64(6))
		handler.LiveSideband(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/live/rtc_ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	msgType, payload, err := conn.ReadMessage()
	if err != nil || msgType != websocket.TextMessage || string(payload) != "up:hello" {
		t.Fatalf("text type=%d payload=%q err=%v", msgType, payload, err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	msgType, payload, err = conn.ReadMessage()
	if err != nil || msgType != websocket.BinaryMessage || !bytes.Equal(payload, []byte("up:\x01\x02\x03")) {
		t.Fatalf("binary type=%d payload=%q err=%v", msgType, payload, err)
	}
	_ = conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _, _ := handler.liveCalls().get(context.Background(), "rtc_ws")
		if got == nil || got.Controller == liveControllerClosed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("sideband did not finalize the call")
}

func TestScopedModelsExposesLiveWhenAllowed(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus"})
	handler := NewHandler(store, nil, nil, nil)
	hidden := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1})
	for _, model := range hidden {
		if isLiveModelName(model.ID) {
			t.Fatalf("live model leaked without allow_live: %+v", hidden)
		}
	}
	visible := listScopedModelsForTest(t, handler, &database.APIKeyRow{
		ID: 1,
		Limits: database.APIKeyLimits{
			AllowLive:  true,
			ModelAllow: []string{"gpt-5.4"},
			ModelDeny:  []string{"gpt-live-1-mini"},
		},
	})
	ids := make([]string, 0, len(visible))
	for _, model := range visible {
		ids = append(ids, model.ID)
	}
	if !containsAll(ids, "gpt-live", "gpt-live-1") {
		t.Fatalf("models=%v, want gpt-live aliases even when model_allow omits them", ids)
	}
	for _, id := range ids {
		if id == "gpt-live-1-mini" {
			t.Fatal("explicit deny should hide gpt-live-1-mini")
		}
	}
}

func liveTestContext(t *testing.T, method, path string, body []byte, row *database.APIKeyRow) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if row != nil {
		c.Set(contextAPIKeyRow, row)
		c.Set(contextAPIKeyID, row.ID)
	}
	return c, rec
}

func containsAll(items []string, want ...string) bool {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	for _, item := range want {
		if _, ok := set[item]; !ok {
			return false
		}
	}
	return true
}

func TestLiveAccountFilterExcludesRelay(t *testing.T) {
	if liveAccountFilter(&auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai"}) {
		t.Fatal("grok should be excluded")
	}
	if liveAccountFilter(&auth.Account{UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "sk", BaseURL: "https://api.openai.com"}) {
		t.Fatal("openai api key account should be excluded")
	}
	if !liveAccountFilter(&auth.Account{AccessToken: "at", PlanType: "plus"}) {
		t.Fatal("chatgpt oauth should be included")
	}
}
