package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func testCodexTelemetryProfile() codexTelemetryProfile {
	account := &auth.Account{DBID: 7, AccessToken: "test-token", AccountID: "acct-test"}
	return codexTelemetryProfile{
		client: codexTelemetryClient{
			account: account, accessToken: account.AccessToken, accountID: account.AccountID,
			userAgent:  "codex-tui/0.153.4 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.153.4)",
			originator: "codex_cli_rs", version: "0.153.4",
		},
		sessionID: "session-1", threadID: "thread-1", turnID: "turn-1", rootTurnID: "turn-1",
		model: "gpt-6-astra", effort: "high", serviceTier: "default", started: time.Now().Add(-time.Second),
		firstThread: true, dynamicTool: true, command: true, fileChange: true,
	}
}

func TestCodexTelemetryEventContract(t *testing.T) {
	profile := testCodexTelemetryProfile()
	terminal := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`)
	events := append(codexInitializationEvents(profile), codexTerminalEvents(profile, codexTelemetryTerminal{status: "completed", body: terminal})...)
	counts := make(map[string]int)
	var mainTurn, accepted map[string]any
	for _, event := range events {
		counts[event.EventType]++
		if event.EventType == "codex_turn_steer_event" {
			t.Fatal("Responses telemetry must not infer native turn/steer RPCs")
		}
		if event.EventType == "codex_turn_event" && event.EventParams["thread_source"] == "user" {
			mainTurn = event.EventParams
		}
		if event.EventType == "codex_turn_event" && event.EventParams["thread_source"] == "thread_title" {
			if event.EventParams["root_turn_id"] != event.EventParams["turn_id"] || event.EventParams["total_tool_call_count"] != 0 {
				t.Fatalf("title turn params = %#v", event.EventParams)
			}
		}
		if event.EventType == "codex_thread_initialized" && event.EventParams["thread_source"] == "guardian_review" {
			if event.EventParams["parent_thread_id"] != profile.threadID || event.EventParams["subagent_source"] != "guardian" || event.EventParams["ephemeral"] != false {
				t.Fatalf("guardian thread params = %#v", event.EventParams)
			}
		}
		if event.EventType == "codex_accepted_line_fingerprints" {
			accepted = event.EventParams
		}
	}
	if counts["codex_thread_initialized"] != 3 || counts["codex_turn_event"] != 2 || counts["codex_hook_run"] != 4 {
		t.Fatalf("event counts = %#v", counts)
	}
	if counts["codex_dynamic_tool_call_event"] != 1 || counts["codex_command_execution_event"] != 1 || counts["codex_file_change_event"] != 1 || counts["codex_accepted_line_fingerprints"] != 1 {
		t.Fatalf("random event counts = %#v", counts)
	}
	if mainTurn["initialization_mode"] != "new" || mainTurn["steer_count"] != 0 || mainTurn["total_tokens"] != int64(20) {
		t.Fatalf("main turn params = %#v", mainTurn)
	}
	if accepted["repo_hash"] != nil || len(accepted["line_fingerprints"].([]any)) != 0 {
		t.Fatalf("accepted fingerprint params = %#v", accepted)
	}
}

func TestCodexTelemetryDoesNotInferResume(t *testing.T) {
	profile := testCodexTelemetryProfile()
	body := []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_old","input":[{"role":"assistant"},{"type":"function_call_output"}]}`)
	profile = buildCodexTelemetryProfile(profile.client, codexTelemetryRequest{account: profile.client.account, body: body, sessionID: "session-1", headers: http.Header{}})
	params := codexMainTurnEvent(profile, codexTelemetryTerminal{status: "completed"}).EventParams
	if params["initialization_mode"] != "new" || params["steer_count"] != 0 {
		t.Fatalf("history changed turn classification: %#v", params)
	}
}

func testCodexMetricPoint(descriptor codexMetricDescriptor, value float64) *codexMetricPoint {
	return newCodexMetricPoint(descriptor, codexMetricAttributeMap(testCodexTelemetryProfile(), descriptor), value)
}

func TestCodexTelemetryMetricsContract(t *testing.T) {
	if len(codexMetricDescriptors) != 66 {
		t.Fatalf("metric descriptor count = %d, want 66", len(codexMetricDescriptors))
	}
	names := make(map[string]bool, len(codexMetricDescriptors))
	points := make([]*codexMetricPoint, 0, len(codexMetricDescriptors))
	for _, descriptor := range codexMetricDescriptors {
		if names[descriptor.name] {
			t.Fatalf("duplicate metric %q", descriptor.name)
		}
		names[descriptor.name] = true
		points = append(points, testCodexMetricPoint(descriptor, 1))
	}
	for _, name := range []string{"codex.hooks.run", "codex.hooks.run.duration_ms", "codex.external_agent_config.detect", "codex.rollout.size_bytes"} {
		if !names[name] {
			t.Fatalf("missing dynamic metric %q", name)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(buildCodexMetricsPayload(testCodexTelemetryProfile(), time.Now(), points), &payload); err != nil {
		t.Fatalf("decode OTLP payload: %v", err)
	}
	resourceMetrics := payload["resourceMetrics"].([]any)
	scopeMetrics := resourceMetrics[0].(map[string]any)["scopeMetrics"].([]any)
	metrics := scopeMetrics[0].(map[string]any)["metrics"].([]any)
	if len(metrics) != 66 {
		t.Fatalf("OTLP metric count = %d, want 66", len(metrics))
	}
	for _, raw := range metrics {
		metric := raw.(map[string]any)
		for _, kind := range []string{"sum", "histogram"} {
			if aggregation, ok := metric[kind].(map[string]any); ok && aggregation["aggregationTemporality"] != float64(1) {
				t.Fatalf("metric %q is not delta temporality", metric["name"])
			}
		}
		histogram, ok := metric["histogram"].(map[string]any)
		if !ok {
			continue
		}
		dataPoint := histogram["dataPoints"].([]any)[0].(map[string]any)
		buckets := dataPoint["bucketCounts"].([]any)
		var bucketTotal float64
		for _, bucket := range buckets {
			bucketTotal += bucket.(float64)
		}
		if bucketTotal != dataPoint["count"].(float64) {
			t.Fatalf("metric %q bucket total %v != count %v", metric["name"], bucketTotal, dataPoint["count"])
		}
		if len(dataPoint["explicitBounds"].([]any)) != len(buckets)-1 {
			t.Fatalf("metric %q bounds/buckets mismatch", metric["name"])
		}
	}
}

// TestCodexTelemetryMetricAggregationKeepsObservations 回归 CodeRabbit 的
// "preserve per-profile histogram observations" 问题：不同 model 的同一轮指标必须
// 各自成点、保留独立 count/属性，同一 model 的多轮才累加。
func TestCodexTelemetryMetricAggregationKeepsObservations(t *testing.T) {
	m := newCodexTelemetryManager()
	m.once.Do(func() {}) // 不启动 worker，直接检查内部状态
	base := testCodexTelemetryProfile()
	accountID := base.client.account.ID()

	profileA := base
	profileA.model = "gpt-6-astra"
	profileA.started = time.Now().Add(-200 * time.Millisecond)
	profileA.dynamicTool, profileA.command, profileA.fileChange = false, false, false
	m.recordTurnMetrics(profileA, codexTelemetryTerminal{status: "completed"})

	profileB := base
	profileB.model = "gpt-5.6-sol"
	profileB.turnID = "turn-2"
	profileB.started = time.Now().Add(-4 * time.Second)
	profileB.dynamicTool, profileB.command, profileB.fileChange = false, false, false
	m.recordTurnMetrics(profileB, codexTelemetryTerminal{status: "completed"})

	state := m.metrics[accountID]
	if state == nil {
		t.Fatal("metric state missing")
	}
	byModel := make(map[string]*codexMetricPoint)
	for _, point := range state.points {
		if point.descriptor.name != "codex.turn.e2e_duration_ms" {
			continue
		}
		if point.count != 1 {
			t.Fatalf("per-model point count = %d, want 1", point.count)
		}
		byModel[point.attributes["model"]] = point
	}
	fast, slow := byModel["gpt-6-astra"], byModel["gpt-5.6-sol"]
	if fast == nil || slow == nil {
		t.Fatalf("per-model attribute points missing: %#v", byModel)
	}
	if slow.sum <= fast.sum {
		t.Fatalf("durations collapsed across profiles: fast=%v slow=%v", fast.sum, slow.sum)
	}

	// 同一 model 再来一轮，应并入同一个点，count 累加而不是新建。
	m.recordTurnMetrics(profileA, codexTelemetryTerminal{status: "completed"})
	if fast.count != 2 {
		t.Fatalf("same-profile observations must accumulate, count = %d", fast.count)
	}
	if len(byModel) != 2 {
		t.Fatalf("aggregation created extra points: %d", len(byModel))
	}
}

// TestCodexTelemetryHookHistogramCountsObservations 校验 hook 时长直方图按真实
// 观测数累加，而不是旧实现的量纲特判。
func TestCodexTelemetryHookHistogramCountsObservations(t *testing.T) {
	m := newCodexTelemetryManager()
	m.once.Do(func() {})
	profile := testCodexTelemetryProfile()
	m.recordTurnMetrics(profile, codexTelemetryTerminal{status: "completed"})
	state := m.metrics[profile.client.account.ID()]
	if state == nil {
		t.Fatal("metric state missing")
	}
	for _, point := range state.points {
		if point.descriptor.name != "codex.hooks.run.duration_ms" {
			continue
		}
		if point.count != 4 {
			t.Fatalf("hook duration histogram count = %d, want 4", point.count)
		}
		var bucketTotal uint64
		for _, bucket := range point.bucketCounts {
			bucketTotal += bucket
		}
		if bucketTotal != point.count {
			t.Fatalf("hook bucket total = %d, want %d", bucketTotal, point.count)
		}
		return
	}
	t.Fatal("hook duration histogram missing")
}

// TestCodexTelemetryRedirectRejected 校验遥测请求拒绝跟随重定向，避免携带
// Bearer 凭证被转发到重定向目标。
func TestCodexTelemetryRedirectRejected(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	profile := testCodexTelemetryProfile()
	err := sendCodexTelemetryJob(codexTelemetryJob{client: profile.client, url: redirector.URL, body: []byte(`{}`)})
	if err == nil {
		t.Fatal("telemetry client must not follow redirects")
	}
	if targetHits != 0 {
		t.Fatalf("redirect target received credentials %d times", targetHits)
	}
}

// TestCodexTelemetryAcceptedLinesIsolatedRequest 校验 accepted-line-fingerprints
// 独占一个请求，其余事件合并成批。
func TestCodexTelemetryAcceptedLinesIsolatedRequest(t *testing.T) {
	m := newCodexTelemetryManager()
	m.once.Do(func() {})
	profile := testCodexTelemetryProfile()
	events := codexTerminalEvents(profile, codexTelemetryTerminal{status: "completed"})
	m.enqueueAnalytics(events)

	var batches [][]map[string]any
	for len(m.queue) > 0 {
		job := <-m.queue
		var payload struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(job.body, &payload); err != nil {
			t.Fatalf("decode analytics batch: %v", err)
		}
		batches = append(batches, payload.Events)
	}
	if len(batches) < 2 {
		t.Fatalf("analytics batch count = %d, want accepted-lines split out", len(batches))
	}
	isolatedAccepted := 0
	for _, batch := range batches {
		hasAccepted := false
		for _, event := range batch {
			if event["event_type"] == "codex_accepted_line_fingerprints" {
				hasAccepted = true
			}
		}
		if !hasAccepted {
			continue
		}
		if len(batch) != 1 {
			t.Fatalf("accepted-line-fingerprints must be isolated, batch = %#v", batch)
		}
		isolatedAccepted++
	}
	if isolatedAccepted != 1 {
		t.Fatalf("isolated accepted-line-fingerprints batches = %d, want 1", isolatedAccepted)
	}
}

// TestCodexTelemetryStatsigGuard 校验客户端明确禁用的指标不会被发往 Statsig。
func TestCodexTelemetryStatsigGuard(t *testing.T) {
	for _, name := range []string{"codex.tool.call", "codex.tool.call.duration_ms", "codex.turn.token_usage", "codex.api_request"} {
		if codexTelemetryStatsigAllowed(name) {
			t.Fatalf("metric %q must be blocked from Statsig", name)
		}
	}
	if !codexTelemetryStatsigAllowed("codex.turn.e2e_duration_ms") {
		t.Fatal("normal metric must stay allowed")
	}
}

func TestCodexDesktopMetricIdentity(t *testing.T) {
	profile := testCodexTelemetryProfile()
	profile.client.userAgent = "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.903.61454)"
	profile.client.originator = "Codex Desktop"
	if got := codexMetricAttributeValue(profile, "codex.turn.e2e_duration_ms", "originator"); got != "Codex_Desktop" {
		t.Fatalf("metric originator = %q", got)
	}
	if got := codexMetricAttributeValue(profile, "codex.turn.e2e_duration_ms", "service_name"); got != "codex_desktop" {
		t.Fatalf("metric service name = %q", got)
	}
	if got := codexMetricAttributeValue(profile, "codex.process.start", "originator"); got != "codex-app-server" {
		t.Fatalf("process originator = %q", got)
	}
}

func TestCodexTelemetryTransportHeaders(t *testing.T) {
	t.Setenv("CODEX_STATSIG_API_KEY", "test-statsig-key")
	requests := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		requests <- request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	profile := testCodexTelemetryProfile()
	jobs := []codexTelemetryJob{
		{client: profile.client, url: server.URL, body: []byte(`{}`)},
		{client: profile.client, url: server.URL, body: []byte(`{}`), metrics: true},
	}
	for _, job := range jobs {
		if err := sendCodexTelemetryJob(job); err != nil {
			t.Fatalf("send telemetry: %v", err)
		}
	}
	analytics, metrics := <-requests, <-requests
	if analytics.Get("Authorization") != "Bearer test-token" || analytics.Get("Chatgpt-Account-Id") != "acct-test" || analytics.Get("Originator") != "codex_cli_rs" || analytics.Get("User-Agent") != profile.client.userAgent {
		t.Fatalf("analytics headers = %#v", analytics)
	}
	if metrics.Get("Authorization") != "" || metrics.Get("statsig-api-key") != "test-statsig-key" || metrics.Get("User-Agent") != "OTel-OTLP-Exporter-Rust/0.31.0" {
		t.Fatalf("metrics headers = %#v", metrics)
	}
}

// TestCodexTelemetryStartupMetricPartition 校验启动/动态指标的划分来自显式名单，
// 而不是 `codexMetricDescriptors[:62]` 的位置契约。
func TestCodexTelemetryStartupMetricPartition(t *testing.T) {
	catalog := make(map[string]bool, len(codexMetricDescriptors))
	startup := 0
	for _, descriptor := range codexMetricDescriptors {
		catalog[descriptor.name] = true
		if codexStartupMetric(descriptor.name) {
			startup++
		}
	}
	for name := range codexDynamicMetricNames {
		if !catalog[name] {
			t.Fatalf("dynamic metric %q missing from catalog", name)
		}
	}
	if startup != len(codexMetricDescriptors)-len(codexDynamicMetricNames) {
		t.Fatalf("startup=%d catalog=%d dynamic=%d", startup, len(codexMetricDescriptors), len(codexDynamicMetricNames))
	}
	if startup != 62 {
		t.Fatalf("startup metric count = %d, want 62", startup)
	}
}

// TestCodexTelemetryTimingProbeGate 校验临时计时探针默认关闭、按 env 开启。
func TestCodexTelemetryTimingProbeGate(t *testing.T) {
	t.Setenv("CODEX_TELEMETRY_TIMING_DEBUG", "")
	if codexTelemetryTimingDebug() {
		t.Fatal("timing probe must default off")
	}
	t.Setenv("CODEX_TELEMETRY_TIMING_DEBUG", "1")
	if !codexTelemetryTimingDebug() {
		t.Fatal("timing probe must honor CODEX_TELEMETRY_TIMING_DEBUG=1")
	}
}

// TestCodexTelemetryTimingProbeRecordsParse 校验探针开启时统计事件数与解析耗时。
func TestCodexTelemetryTimingProbeRecordsParse(t *testing.T) {
	t.Setenv("CODEX_TELEMETRY_TIMING_DEBUG", "1")
	attempt := &codexTelemetryAttempt{profile: testCodexTelemetryProfile(), timing: true}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws\"}}\n\n"
	body := &codexTelemetryBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), attempt: attempt}
	buf := make([]byte, 128)
	for {
		if _, err := body.Read(buf); err != nil {
			break
		}
	}
	if attempt.eventCount.Load() != 2 {
		t.Fatalf("probe event count = %d, want 2", attempt.eventCount.Load())
	}
	if attempt.firstToken.IsZero() {
		t.Fatal("probe run must still record first token")
	}
}

// TestCodexTelemetryParsesWebsocketSSE 校验 WS 上游的 SSE 包装流同样被解析：
// wsrelay.websocketResponseToHTTP 把每个 WebSocket 帧写成 `data: <json>\n\n`，
// 与 HTTP 路径同形，所以两种传输共用同一套观测逻辑。
func TestCodexTelemetryParsesWebsocketSSE(t *testing.T) {
	attempt := &codexTelemetryAttempt{profile: testCodexTelemetryProfile()}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws\"}}\n\n"
	body := &codexTelemetryBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), attempt: attempt}
	buf := make([]byte, 128)
	for {
		if _, err := body.Read(buf); err != nil {
			break
		}
	}
	if attempt.firstEvent.IsZero() {
		t.Fatal("WebSocket-shaped SSE stream did not record first event")
	}
	if attempt.firstToken.IsZero() {
		t.Fatal("WebSocket-shaped SSE stream did not record first token")
	}
}

func TestCodexTelemetryDefaultOff(t *testing.T) {
	if DefaultRuntimeSettings().CodexTelemetryEnabled {
		t.Fatal("simulated telemetry must be opt-in")
	}
}

func TestCodexTelemetryJobRoutesThroughResin(t *testing.T) {
	// The telemetry worker pool is process-global. Jobs queued by earlier
	// tests may still be draining while this test points the global Resin
	// config at its own server, so the handler must only capture this
	// test's request (matched by body) and must never block on the channel:
	// a blocked handler would stall the client until timeout and then hang
	// the deferred server.Close forever.
	const marker = `{"codex2api_test":"resin-route"}`
	requests := make(chan *http.Request, 1)
	const probeBody = `{"test_marker":"resin_route_probe"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if string(body) == marker {
			select {
			case requests <- request.Clone(request.Context()):
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	SetResinConfig(&ResinConfig{BaseURL: server.URL + "/token", PlatformName: "test"})
	defer SetResinConfig(nil)
	profile := testCodexTelemetryProfile()
	job := codexTelemetryJob{client: profile.client, url: "https://chatgpt.com/backend-api/codex/analytics-events/events", body: []byte(marker)}
	if err := sendCodexTelemetryJob(job); err != nil {
		t.Fatalf("send telemetry via resin: %v", err)
	}
	var got *http.Request
	select {
	case got = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("resin server did not receive the telemetry request")
	}
	if got.URL.Path != "/token/test/https/chatgpt.com/backend-api/codex/analytics-events/events" {
		t.Fatalf("resin path = %q", got.URL.Path)
	}
	if got.Header.Get("X-Resin-Account") != ResinAccountID(profile.client.account) || got.Header.Get("Authorization") != "Bearer test-token" {
		t.Fatalf("resin headers = %#v", got.Header)
	}
}

func TestCodexTelemetryRecordTurnWithoutStateDoesNotPanic(t *testing.T) {
	m := newCodexTelemetryManager()
	m.once.Do(func() {}) // 不启动 worker，直接观察队列内容
	profile := testCodexTelemetryProfile()
	profile.started = time.Now().Add(-2 * codexTelemetryStateTTL)
	m.recordTurnMetrics(profile, codexTelemetryTerminal{status: "completed"})
	if len(m.queue) != 1 {
		t.Fatalf("startup batch count = %d, want 1", len(m.queue))
	}
	state := m.metrics[profile.client.account.ID()]
	if state == nil || time.Since(state.lastSeen) > time.Minute {
		t.Fatalf("state after long turn = %#v", state)
	}
	// 分钟级 flush 不应把刚结束的长回合状态清掉，也不应再发第二批启动指标。
	m.flushMetrics(time.Now())
	if m.metrics[profile.client.account.ID()] == nil {
		t.Fatal("state evicted right after a long turn")
	}
	m.recordTurnMetrics(profile, codexTelemetryTerminal{status: "completed"})
	if len(m.queue) != 2 { // 1 startup + 1 flush batch；第二轮不再发启动指标
		t.Fatalf("queue after second turn = %d, want 2", len(m.queue))
	}
}

func TestCodexTelemetryProfileIgnoresLocalAffinityKey(t *testing.T) {
	profile := testCodexTelemetryProfile()
	headers := http.Header{}
	headers.Set(downstreamAffinityHeader, "tenant-secret-42")
	profile = buildCodexTelemetryProfile(profile.client, codexTelemetryRequest{account: profile.client.account, body: []byte(`{"model":"gpt-6-astra"}`), sessionID: "upstream-session", headers: headers})
	if profile.sessionID != "upstream-session" || profile.threadID != "upstream-session" {
		t.Fatalf("session identity = %q/%q, must not derive from local affinity key", profile.sessionID, profile.threadID)
	}
}

func TestCodexTelemetryBodyCloseRacesRead(t *testing.T) {
	pr, pw := io.Pipe()
	attempt := &codexTelemetryAttempt{profile: testCodexTelemetryProfile()}
	body := &codexTelemetryBody{ReadCloser: pr, attempt: attempt}
	go func() {
		for i := 0; i < 50; i++ {
			_, _ = pw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"))
		}
		_ = pw.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			if _, err := body.Read(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Millisecond)
	_ = body.Close()
	<-done
}
