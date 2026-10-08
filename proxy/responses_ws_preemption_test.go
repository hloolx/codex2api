package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWSSessionPreemptRegistryCancelsOnlySameScopedSession(t *testing.T) {
	var registry responsesWSSessionPreemptRegistry
	key := responsesWSSessionPreemptKey{apiKeyID: 11, scopeHash: "scope-a", sessionHash: "session"}
	other := responsesWSSessionPreemptKey{apiKeyID: 12, scopeHash: "scope-a", sessionHash: "session"}

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstCleanup, _, replaced := registry.Begin(key, firstCancel)
	if replaced {
		t.Fatal("first registration unexpectedly replaced an owner")
	}
	otherCtx, otherCancel := context.WithCancel(context.Background())
	otherCleanup, _, replaced := registry.Begin(other, otherCancel)
	if replaced {
		t.Fatal("different API key unexpectedly replaced an owner")
	}
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondCleanup, firstDone, replaced := registry.Begin(key, secondCancel)
	if !replaced || firstDone == nil {
		t.Fatal("same scoped session did not replace the first owner")
	}
	if firstCtx.Err() == nil {
		t.Fatal("first owner was not canceled")
	}
	if otherCtx.Err() != nil || secondCtx.Err() != nil {
		t.Fatal("replacement canceled a differently scoped or current owner")
	}

	firstCleanup()
	select {
	case <-firstDone:
	default:
		t.Fatal("previous cleanup did not signal handoff completion")
	}
	thirdCtx, thirdCancel := context.WithCancel(context.Background())
	thirdCleanup, _, replaced := registry.Begin(key, thirdCancel)
	if !replaced || secondCtx.Err() == nil {
		t.Fatal("stale cleanup removed the replacement registration")
	}
	if thirdCtx.Err() != nil {
		t.Fatal("newest owner was canceled")
	}

	thirdCleanup()
	secondCleanup()
	otherCleanup()
}

func newResponsesWSPreemptTestContext(apiKeyID int64, row *database.APIKeyRow) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set(contextAPIKeyID, apiKeyID)
	c.Set(contextAPIKeyRow, row)
	return c
}

func TestResponsesWSSessionPreemptKeyIsolationAndStreamMultiplexing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bodyA := []byte(`{"model":"gpt-5.5","prompt_cache_key":"conversation","stream_id":"stream-a","input":"hello"}`)
	bodyB := []byte(`{"model":"gpt-5.5","prompt_cache_key":"conversation","stream_id":"stream-b","input":"hello"}`)
	row := &database.APIKeyRow{ID: 11, AllowedGroupIDs: []int64{7, 3}}
	c := newResponsesWSPreemptTestContext(11, row)
	identityA := resolveRequestSessionIdentity(c.Request.Header, bodyA)
	keyA, ok := newResponsesWSSessionPreemptKey(c, bodyA, identityA)
	if !ok {
		t.Fatal("explicit prompt_cache_key did not arm preemption")
	}
	keyB, ok := newResponsesWSSessionPreemptKey(c, bodyB, resolveRequestSessionIdentity(c.Request.Header, bodyB))
	if !ok || keyA.sessionHash == keyB.sessionHash {
		t.Fatal("different stream_id values did not isolate multiplexed streams")
	}

	otherAPIKey := newResponsesWSPreemptTestContext(12, &database.APIKeyRow{ID: 12, AllowedGroupIDs: []int64{7, 3}})
	keyOther, ok := newResponsesWSSessionPreemptKey(otherAPIKey, bodyA, resolveRequestSessionIdentity(otherAPIKey.Request.Header, bodyA))
	if !ok || keyA.apiKeyID == keyOther.apiKeyID {
		t.Fatal("different API keys were not isolated")
	}

	otherScope := newResponsesWSPreemptTestContext(11, &database.APIKeyRow{ID: 11, AllowedGroupIDs: []int64{9}})
	keyOtherScope, ok := newResponsesWSSessionPreemptKey(otherScope, bodyA, resolveRequestSessionIdentity(otherScope.Request.Header, bodyA))
	if !ok || keyA.scopeHash == keyOtherScope.scopeHash {
		t.Fatal("different effective group scopes were not isolated")
	}

	if _, ok := newResponsesWSSessionPreemptKey(c, []byte(`{}`), requestSessionIdentity{affinityID: "api-key-fallback"}); ok {
		t.Fatal("API-key-only fallback identity must not collapse unrelated sessions")
	}
}

func TestResponsesWSSessionPreemptKeySeparatesSubagentThreads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	row := &database.APIKeyRow{ID: 11, AllowedGroupIDs: []int64{7}}
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	newThreadContext := func(threadID string) *gin.Context {
		c := newResponsesWSPreemptTestContext(11, row)
		c.Request.Header.Set("Session-Id", "shared-session")
		if threadID != "" {
			c.Request.Header.Set("Thread-Id", threadID)
		}
		return c
	}

	parentContext := newThreadContext("parent-thread")
	parentKey, ok := newResponsesWSSessionPreemptKey(
		parentContext,
		body,
		resolveRequestSessionIdentity(parentContext.Request.Header, body),
	)
	if !ok {
		t.Fatal("parent session did not arm preemption")
	}
	childContext := newThreadContext("child-thread")
	childKey, ok := newResponsesWSSessionPreemptKey(
		childContext,
		body,
		resolveRequestSessionIdentity(childContext.Request.Header, body),
	)
	if !ok {
		t.Fatal("child session did not arm preemption")
	}
	if parentKey.sessionHash == childKey.sessionHash {
		t.Fatal("different subagent threads collapsed onto one preemption key")
	}

	repeatContext := newThreadContext("child-thread")
	repeatKey, ok := newResponsesWSSessionPreemptKey(
		repeatContext,
		body,
		resolveRequestSessionIdentity(repeatContext.Request.Header, body),
	)
	if !ok || repeatKey.sessionHash != childKey.sessionHash {
		t.Fatal("same child thread did not keep a stable preemption key")
	}

	missingA := newThreadContext("")
	missingB := newThreadContext("")
	missingKeyA, okA := newResponsesWSSessionPreemptKey(missingA, body, resolveRequestSessionIdentity(missingA.Request.Header, body))
	missingKeyB, okB := newResponsesWSSessionPreemptKey(missingB, body, resolveRequestSessionIdentity(missingB.Request.Header, body))
	if !okA || !okB || missingKeyA.sessionHash != missingKeyB.sessionHash {
		t.Fatal("missing Thread-Id did not retain the legacy shared-session preemption key")
	}
	rootContext := newThreadContext("shared-session")
	rootKey, rootOK := newResponsesWSSessionPreemptKey(rootContext, body, resolveRequestSessionIdentity(rootContext.Request.Header, body))
	if !rootOK || rootKey.sessionHash != missingKeyA.sessionHash {
		t.Fatal("root Thread-Id equal to Session-Id changed the legacy preemption key")
	}
}

func TestResponsesWSSessionPreemptKeySeparatesMemoryRequestKind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	row := &database.APIKeyRow{ID: 11, AllowedGroupIDs: []int64{7}}
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	newKindContext := func(kind string) *gin.Context {
		c := newResponsesWSPreemptTestContext(11, row)
		c.Request.Header.Set("Session-Id", "shared-session")
		c.Request.Header.Set("Thread-Id", "shared-session")
		if kind != "" {
			c.Request.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"shared-session","request_kind":"`+kind+`"}`)
		}
		return c
	}
	keyFor := func(c *gin.Context, raw []byte) responsesWSSessionPreemptKey {
		t.Helper()
		key, ok := newResponsesWSSessionPreemptKey(c, raw, resolveRequestSessionIdentity(c.Request.Header, raw))
		if !ok {
			t.Fatal("session did not arm preemption")
		}
		return key
	}

	legacy := keyFor(newKindContext(""), body)
	turn := keyFor(newKindContext("turn"), body)
	compaction := keyFor(newKindContext("compaction"), body)
	if turn.sessionHash != legacy.sessionHash || compaction.sessionHash != legacy.sessionHash {
		t.Fatal("turn/compaction request kinds must keep the legacy shared-session preemption key")
	}
	memory := keyFor(newKindContext("memory"), body)
	if memory.sessionHash == legacy.sessionHash {
		t.Fatal("request_kind=memory shares the user turn preemption key; a background memory turn would cancel the active user turn")
	}
	if repeat := keyFor(newKindContext("memory"), body); repeat.sessionHash != memory.sessionHash {
		t.Fatal("memory request kind did not keep a stable preemption key")
	}

	// 头里没有 turn 元数据时按请求体 client_metadata 内嵌的 request_kind 分道。
	bodyMemory := []byte(`{"model":"gpt-5.5","input":"hello","client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"shared-session\",\"request_kind\":\"memory\"}"}}`)
	if keyFor(newKindContext(""), bodyMemory).sessionHash == legacy.sessionHash {
		t.Fatal("body-embedded request_kind=memory shares the user turn preemption key")
	}
}

func TestWatchResponsesWSSessionPreemptOwnerDetectsRemoteReplacement(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	ownerStore := tokenCache.(cache.RuntimeOwnerStore)
	key := responsesWSSessionPreemptKey{apiKeyID: 11, scopeHash: "scope", sessionHash: "session"}
	remoteKey := responsesWSSessionPreemptRemoteKey(key)
	ownerA := []byte("owner-a")
	ownerB := []byte("owner-b")
	if _, err := ownerStore.ClaimRuntimeOwner(context.Background(), responsesWSSessionPreemptNamespace, remoteKey, ownerA, time.Minute); err != nil {
		t.Fatalf("claim owner A: %v", err)
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lost := make(chan struct{}, 1)
	stop := watchResponsesWSSessionPreemptOwner(watchCtx, ownerStore, key, ownerA, func() { lost <- struct{}{} })
	defer stop()
	if previous, err := ownerStore.ClaimRuntimeOwner(context.Background(), responsesWSSessionPreemptNamespace, remoteKey, ownerB, time.Minute); err != nil || string(previous) != "owner-a" {
		t.Fatalf("replace owner = %q, %v", previous, err)
	}
	select {
	case <-lost:
	case <-time.After(responsesWSSessionPreemptWatchInterval + time.Second):
		t.Fatal("watcher did not detect remote owner replacement")
	}
}

func TestBeginResponsesWSSessionPreemptionCancelsPreviousAndSupportsHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"conversation","input":"hello"}`)
	row := &database.APIKeyRow{ID: 11}
	firstGin := newResponsesWSPreemptTestContext(11, row)
	firstIdentity := resolveRequestSessionIdentity(firstGin.Request.Header, body)
	firstCtx, firstCleanup, armed := h.beginResponsesWSSessionPreemption(firstGin.Request.Context(), firstGin, body, firstIdentity)
	if !armed {
		t.Fatal("first session did not arm preemption")
	}

	type beginResult struct {
		ctx     context.Context
		cleanup func()
		armed   bool
	}
	resultCh := make(chan beginResult, 1)
	secondGin := newResponsesWSPreemptTestContext(11, row)
	go func() {
		ctx, cleanup, armed := h.beginResponsesWSSessionPreemption(
			secondGin.Request.Context(),
			secondGin,
			body,
			resolveRequestSessionIdentity(secondGin.Request.Header, body),
		)
		resultCh <- beginResult{ctx: ctx, cleanup: cleanup, armed: armed}
	}()

	select {
	case <-firstCtx.Done():
		if !errors.Is(context.Cause(firstCtx), errResponsesWSSessionPreempted) {
			t.Fatalf("first cancellation cause = %v", context.Cause(firstCtx))
		}
	case <-time.After(time.Second):
		t.Fatal("new owner did not preempt the first owner")
	}
	firstCleanup()

	var second beginResult
	select {
	case second = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("new owner did not complete the handoff")
	}
	if !second.armed || second.ctx.Err() != nil {
		t.Fatalf("second owner = armed:%t err:%v", second.armed, second.ctx.Err())
	}
	second.cleanup()
}

func TestBeginResponsesWSSessionPreemptionNotifiesClientBeforeCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"conversation","input":"hello"}`)
	row := &database.APIKeyRow{ID: 11}

	var notifiedAt atomic.Int64
	notifyDone := make(chan struct{})
	firstGin := newResponsesWSPreemptTestContext(11, row)
	var firstCtx context.Context
	var firstCleanup func()
	var armed bool
	firstCtx, firstCleanup, armed = h.beginResponsesWSSessionPreemptionWithNotify(
		firstGin.Request.Context(),
		firstGin,
		body,
		resolveRequestSessionIdentity(firstGin.Request.Header, body),
		func() {
			if firstCtx.Err() != nil {
				t.Error("close-frame notification ran after the preempted context was cancelled")
			}
			notifiedAt.Store(time.Now().UnixNano())
			close(notifyDone)
		},
	)
	if !armed {
		t.Fatal("first session did not arm preemption")
	}
	defer firstCleanup()
	if isResponsesWSSessionPreempted(firstCtx) {
		t.Fatal("fresh owner must not report preempted")
	}

	secondGin := newResponsesWSPreemptTestContext(11, row)
	go func() {
		_, cleanup, _ := h.beginResponsesWSSessionPreemptionWithNotify(
			secondGin.Request.Context(),
			secondGin,
			body,
			resolveRequestSessionIdentity(secondGin.Request.Header, body),
			nil,
		)
		cleanup()
	}()

	select {
	case <-notifyDone:
	case <-time.After(time.Second):
		t.Fatal("preempted owner was never told to close its client connection")
	}
	if !isResponsesWSSessionPreempted(firstCtx) {
		t.Fatal("preempted flag must be visible as soon as the close frame goes out")
	}
	select {
	case <-firstCtx.Done():
		if !errors.Is(context.Cause(firstCtx), errResponsesWSSessionPreempted) {
			t.Fatalf("first cancellation cause = %v", context.Cause(firstCtx))
		}
	case <-time.After(2 * responsesWSSessionPreemptCloseGrace):
		t.Fatal("preempted owner was not cancelled after the close frame was delivered")
	}
	if notifiedAt.Load() == 0 {
		t.Fatal("notification timestamp missing")
	}
}

func TestBeginResponsesWSSessionPreemptionCancelsEvenWhenNotifyHangs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"conversation","input":"hello"}`)
	row := &database.APIKeyRow{ID: 11}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	firstGin := newResponsesWSPreemptTestContext(11, row)
	firstCtx, firstCleanup, armed := h.beginResponsesWSSessionPreemptionWithNotify(
		firstGin.Request.Context(),
		firstGin,
		body,
		resolveRequestSessionIdentity(firstGin.Request.Header, body),
		func() { <-release },
	)
	if !armed {
		t.Fatal("first session did not arm preemption")
	}
	defer firstCleanup()

	secondGin := newResponsesWSPreemptTestContext(11, row)
	go func() {
		_, cleanup, _ := h.beginResponsesWSSessionPreemption(
			secondGin.Request.Context(),
			secondGin,
			body,
			resolveRequestSessionIdentity(secondGin.Request.Header, body),
		)
		cleanup()
	}()

	start := time.Now()
	select {
	case <-firstCtx.Done():
	case <-time.After(responsesWSSessionPreemptCloseGrace + 2*time.Second):
		t.Fatal("hung close-frame write must not keep the preempted owner alive past the grace period")
	}
	if elapsed := time.Since(start); elapsed < responsesWSSessionPreemptCloseGrace/2 {
		t.Fatalf("cancelled after %s, want to wait for the close-frame grace period first", elapsed)
	}
}

func TestResponsesWebSocketNewerSameSessionPreemptsBeforeConcurrencyAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(nextSettings)

	firstStarted := make(chan struct{}, 1)
	firstCanceled := make(chan struct{}, 1)
	var calls atomic.Int64
	WebsocketExecuteFunc = func(ctx context.Context, _ *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			firstStarted <- struct{}{}
			<-ctx.Done()
			firstCanceled <- struct{}{}
			return nil, ctx.Err()
		}
		sse := `data: {"type":"response.created"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"new"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5"})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "at-1", AccountID: "acct-1", PlanType: "pro"}
	store.AddAccount(account)
	h := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 77, Limits: database.APIKeyLimits{MaxConcurrency: 1}}
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, row.ID)
		c.Set(contextAPIKeyRow, row)
		h.ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	first, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial first websocket: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	body := []byte(`{"type":"response.create","model":"gpt-5.5","prompt_cache_key":"conversation","input":"hello"}`)
	if err := first.WriteMessage(websocket.TextMessage, body); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first upstream request did not start")
	}

	second, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial second websocket: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.WriteMessage(websocket.TextMessage, body); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("preempted upstream was not canceled promptly")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = second.SetReadDeadline(deadline)
		_, frame, readErr := second.ReadMessage()
		if readErr != nil {
			t.Fatalf("read replacement response: %v", readErr)
		}
		if gjson.GetBytes(frame, "type").String() == "response.completed" {
			break
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	releaseDeadline := time.Now().Add(time.Second)
	for account.ActiveRequests.Load() != 0 && time.Now().Before(releaseDeadline) {
		time.Sleep(time.Millisecond)
	}
	if got := account.ActiveRequests.Load(); got != 0 {
		t.Fatalf("account active requests after replacement cleanup = %d, want 0", got)
	}
}
