package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	codexAnalyticsEndpointDefault = "https://chatgpt.com/backend-api/codex/analytics-events/events"
	codexMetricsEndpointDefault   = "https://ab.chatgpt.com/otlp/v1/metrics"
	codexStatsigAPIKeyDefault     = "client-MkRuleRQBd6qakfnDYqJVR9JuXcY57Ljly3vi5JVUIO"
	codexTelemetryQueueSize       = 1024
	codexTelemetryWorkers         = 4
	codexTelemetryTimeout         = 10 * time.Second
	codexTelemetryDropLogInterval = time.Minute
	codexTelemetryStateTTL        = 5 * time.Minute
	codexTelemetryMaxEventBytes   = 8 << 20
)

var (
	codexAnalyticsEndpoint = codexAnalyticsEndpointDefault
	codexMetricsEndpoint   = codexMetricsEndpointDefault
	codexTelemetryRandIntN = rand.IntN
	codexTelemetryGlobal   = newCodexTelemetryManager()
)

type codexTelemetryClient struct {
	account     *auth.Account
	accessToken string
	accountID   string
	proxyURL    string
	userAgent   string
	originator  string
	version     string
}

type codexTelemetryProfile struct {
	client       codexTelemetryClient
	sessionID    string
	threadID     string
	turnID       string
	rootTurnID   string
	model        string
	effort       string
	serviceTier  string
	started      time.Time
	firstThread  bool
	dynamicTool  bool
	command      bool
	fileChange   bool
	turnMetadata gjson.Result
}

type codexTelemetryRequest struct {
	account       *auth.Account
	body          []byte
	sessionID     string
	proxyOverride string
	apiKey        string
	deviceCfg     *DeviceProfileConfig
	headers       http.Header
}

type codexTelemetryTerminal struct {
	status     string
	body       []byte
	firstEvent time.Time
	firstToken time.Time
}

type codexTelemetryAttempt struct {
	profile codexTelemetryProfile
	// mu 保护 firstEvent/firstToken：读流 goroutine 在 processEvent 里写，
	// 上游超时或下游断开时 Close 可能从另一个 goroutine 触发 finish 读取。
	mu         sync.Mutex
	firstEvent time.Time
	firstToken time.Time
	done       sync.Once
	// 临时计时探针：仅在 CODEX_TELEMETRY_TIMING_DEBUG=1 时采集，其余时候零开销。
	timing     bool
	parseNanos atomic.Int64
	eventCount atomic.Int64
}

type codexTelemetryJob struct {
	client  codexTelemetryClient
	url     string
	body    []byte
	metrics bool
}

type codexTelemetryManager struct {
	once    sync.Once
	queue   chan codexTelemetryJob
	mu      sync.Mutex
	threads map[string]time.Time
	metrics map[int64]*codexMetricState
	// 队列满丢弃只按间隔汇总打日志，避免高流量下逐条刷屏。
	dropped     atomic.Int64
	dropLogUnix atomic.Int64
}

// newCodexTelemetryManager 创建进程内遥测队列与状态容器。
func newCodexTelemetryManager() *codexTelemetryManager {
	return &codexTelemetryManager{
		queue:   make(chan codexTelemetryJob, codexTelemetryQueueSize),
		threads: make(map[string]time.Time),
		metrics: make(map[int64]*codexMetricState),
	}
}

// start 按需启动发送 worker 和指标刷新循环。
//
// 每个 job 是一次同步 POST（可能经账号代理，几百毫秒到 10s 超时），单 worker
// 串行在几 QPS 就会把队列打满；用一小组并发 worker 撑住常规流量，队列满时仍丢弃。
func (m *codexTelemetryManager) start() {
	m.once.Do(func() {
		for range codexTelemetryWorkers {
			go m.worker()
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				m.flushMetrics(now)
			}
		}()
	})
}

// worker 串行发送队列中的遥测任务。
func (m *codexTelemetryManager) worker() {
	for job := range m.queue {
		if err := sendCodexTelemetryJob(job); err != nil {
			log.Printf("Codex 遥测发送失败: %v", err)
		}
	}
}

// enqueue 非阻塞地加入遥测任务，队列满时直接丢弃并按间隔汇总记日志。
func (m *codexTelemetryManager) enqueue(job codexTelemetryJob) {
	m.start()
	select {
	case m.queue <- job:
	default:
		dropped := m.dropped.Add(1)
		now := time.Now().Unix()
		last := m.dropLogUnix.Load()
		if now-last >= int64(codexTelemetryDropLogInterval/time.Second) && m.dropLogUnix.CompareAndSwap(last, now) {
			log.Printf("Codex 遥测队列已满，累计丢弃 %d 批数据", dropped)
		}
	}
}

// markThread 记录账号观察到的 thread，并报告它是否首次出现。
func (m *codexTelemetryManager) markThread(accountID int64, threadID string, now time.Time) bool {
	key := strconv.FormatInt(accountID, 10) + ":" + threadID
	m.mu.Lock()
	defer m.mu.Unlock()
	_, found := m.threads[key]
	// ponytail: 固定上限比维护第二套 LRU 更小；超过时整体重建即可。
	if len(m.threads) >= 4096 {
		m.threads = make(map[string]time.Time)
		found = false
	}
	m.threads[key] = now
	return !found
}

// codexStatsigAPIKey 返回环境覆盖或官方公开 Statsig SDK key。
func codexStatsigAPIKey() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_STATSIG_API_KEY")); value != "" {
		return value
	}
	return codexStatsigAPIKeyDefault
}

// codexTelemetryTimingDebug 是临时计时探针开关：管理后台「客户端遥测计时探针」
// 或环境变量 CODEX_TELEMETRY_TIMING_DEBUG=1 任一开启即生效。关闭时不采集任何
// 耗时，行为与不插桩完全一致；开启时只额外打印 [TELEMETRY-TIMING] 日志，用于
// 对比开启/关闭遥测时的请求入口与流式解析开销。
func codexTelemetryTimingDebug() bool {
	if CurrentRuntimeSettings().CodexTelemetryTimingDebug {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TELEMETRY_TIMING_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// codexTelemetryEnabled 合并运行时开关与部署级关闭设置。
//
// 运行时开关默认关闭（实验性功能，事件是模拟生成的，是否外发由部署者决定），
// 因此测试里调用 ExecuteRequest 不会把真实令牌打到上游，不需要按二进制名特判。
func codexTelemetryEnabled() bool {
	if !CurrentRuntimeSettings().CodexTelemetryEnabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TELEMETRY_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// codexTelemetryEligible 判断请求是否应模拟官方 Codex 遥测。
func codexTelemetryEligible(body []byte, headers http.Header) bool {
	if !codexTelemetryEnabled() || headers == nil || !gjson.ValidBytes(body) {
		return false
	}
	if !strings.HasPrefix(CodexBaseURL, "https://chatgpt.com/backend-api/codex") {
		return false
	}
	if turnMetadataIndicatesCompaction(headers.Get(codexTurnMetadataHeader)) {
		return false
	}
	return !responsesBodyRequestsImageGeneration(body) && !requestBodyCompactionMeta(body).UsageTriggered
}

// beginCodexTelemetry 为符合条件的请求创建观测并发送初始化数据。
//
// HTTP 与 WebSocket 两条上游都覆盖：WS 执行器会把 WebSocket 帧包装成标准 SSE
// `http.Response`（见 wsrelay.websocketResponseToHTTP），因此本链路基于 SSE 的
// 解析对两种传输同样适用，不需要按传输分流。
func beginCodexTelemetry(input codexTelemetryRequest) *codexTelemetryAttempt {
	timing := codexTelemetryTimingDebug()
	var startedAt time.Time
	if timing {
		startedAt = time.Now()
	}
	if input.account == nil || !codexTelemetryEligible(input.body, input.headers) {
		return nil
	}
	client, ok := snapshotCodexTelemetryClient(input)
	if !ok {
		return nil
	}
	profile := buildCodexTelemetryProfile(client, input)
	profile.firstThread = codexTelemetryGlobal.markThread(input.account.ID(), profile.threadID, profile.started)
	profile.dynamicTool = codexTelemetryRandIntN(5) < 2
	profile.command = profile.dynamicTool && codexTelemetryRandIntN(2) == 0
	profile.fileChange = codexTelemetryRandIntN(5) == 0
	attempt := &codexTelemetryAttempt{profile: profile, timing: timing}
	codexTelemetryGlobal.enqueueAnalytics(codexInitializationEvents(profile))
	codexTelemetryGlobal.touchMetrics(profile)
	if timing {
		log.Printf("[TELEMETRY-TIMING] begin account=%d model=%s first_thread=%t setup_ms=%d",
			input.account.ID(), profile.model, profile.firstThread, time.Since(startedAt).Milliseconds())
	}
	return attempt
}

// snapshotCodexTelemetryClient 固化本次请求实际使用的账号和客户端身份。
func snapshotCodexTelemetryClient(input codexTelemetryRequest) (codexTelemetryClient, bool) {
	client := codexTelemetryClient{
		account: input.account, accessToken: input.account.GetAccessToken(),
		accountID: input.account.EffectiveAccountID(), proxyURL: input.account.GetProxyURL(),
	}
	if client.accessToken == "" || client.accountID == "" || input.account.IsCodexAgentIdentity() {
		return codexTelemetryClient{}, false
	}
	if input.proxyOverride != "" {
		client.proxyURL = input.proxyOverride
	}
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: input.account, APIKey: input.apiKey, DeviceConfig: input.deviceCfg, Headers: input.headers})
	if err != nil {
		return codexTelemetryClient{}, false
	}
	client.userAgent, client.version = identity.UserAgent, identity.Version
	client.originator = codexTelemetryOriginator(client.userAgent, input.headers)
	userAgentOverridden, originatorOverridden := false, false
	for name, value := range input.account.GetCustomHeaders() {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "user-agent":
			client.userAgent = value
			userAgentOverridden = true
		case "originator":
			client.originator = value
			originatorOverridden = true
		case "chatgpt-account-id":
			client.accountID = value
		case "version":
			client.version = value
		}
	}
	if userAgentOverridden && !originatorOverridden {
		client.originator = CodexOriginatorForGeneratedUserAgent(client.userAgent)
	}
	return client, client.userAgent != "" && client.originator != ""
}

// codexTelemetryOriginator 复用合法下游值或按 User-Agent 推导 originator。
func codexTelemetryOriginator(userAgent string, headers http.Header) string {
	if value := strings.TrimSpace(headers.Get("Originator")); IsCodexOfficialClientByHeaders(userAgent, value) && value != "" {
		return value
	}
	return CodexOriginatorForGeneratedUserAgent(userAgent)
}

// codexAnalyticsIsolatedEvent 是需要单独成批发送的事件类型。真实客户端
// TrackEventRequest::should_send_in_isolated_request 只对 accepted-line-fingerprints
// 返回 true，即该事件必须独占一个 HTTP 请求。
func codexAnalyticsIsolatedEvent(eventType string) bool {
	return eventType == "codex_accepted_line_fingerprints"
}

// enqueueAnalytics 按真实客户端的批处理规则编码并排队发送分析事件：
// isolated 事件（accepted-line-fingerprints）单独成批，其余事件合并发送。
func (m *codexTelemetryManager) enqueueAnalytics(events []codexAnalyticsEvent) {
	if len(events) == 0 {
		return
	}
	batch := make([]codexAnalyticsEvent, 0, len(events))
	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.enqueueAnalyticsBatch(batch)
		batch = batch[:0]
	}
	for _, event := range events {
		if codexAnalyticsIsolatedEvent(event.EventType) {
			flush()
			m.enqueueAnalyticsBatch([]codexAnalyticsEvent{event})
			continue
		}
		batch = append(batch, event)
	}
	flush()
}

// enqueueAnalyticsBatch 编码一个非空事件批次并入队。
func (m *codexTelemetryManager) enqueueAnalyticsBatch(events []codexAnalyticsEvent) {
	if len(events) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err == nil {
		m.enqueue(codexTelemetryJob{client: events[0].client, url: codexAnalyticsEndpoint, body: body})
	}
}

// observeResult 根据上游结果立即结束观测或包装响应流。
func (a *codexTelemetryAttempt) observeResult(resp *http.Response, err error) {
	if a == nil {
		return
	}
	if err != nil || resp == nil {
		a.finish("failed", nil)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Body == nil {
		a.finish("failed", nil)
		return
	}
	resp.Body = &codexTelemetryBody{ReadCloser: resp.Body, attempt: a}
}

// finish 仅一次地提交终止事件和本轮指标。
func (a *codexTelemetryAttempt) finish(status string, terminal []byte) {
	if a == nil {
		return
	}
	a.done.Do(func() {
		a.mu.Lock()
		firstEvent, firstToken := a.firstEvent, a.firstToken
		a.mu.Unlock()
		result := codexTelemetryTerminal{status: status, body: terminal, firstEvent: firstEvent, firstToken: firstToken}
		codexTelemetryGlobal.enqueueAnalytics(codexTerminalEvents(a.profile, result))
		codexTelemetryGlobal.recordTurnMetrics(a.profile, result)
		if a.timing {
			log.Printf("[TELEMETRY-TIMING] finish account=%d status=%s parse_ms=%d events=%d",
				a.profile.client.account.ID(), status,
				a.parseNanos.Load()/int64(time.Millisecond), a.eventCount.Load())
		}
	})
}

type codexTelemetryBody struct {
	io.ReadCloser
	attempt  *codexTelemetryAttempt
	pending  []byte
	event    []byte
	dropping bool
}

func (b *codexTelemetryBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.attempt.timing {
			startedAt := time.Now()
			b.observe(p[:n])
			b.attempt.parseNanos.Add(time.Since(startedAt).Nanoseconds())
		} else {
			b.observe(p[:n])
		}
	}
	if err == io.EOF {
		b.flushJSON()
		b.attempt.finish("interrupted", nil)
	}
	return n, err
}

func (b *codexTelemetryBody) Close() error {
	b.attempt.finish("interrupted", nil)
	return b.ReadCloser.Close()
}

func (b *codexTelemetryBody) observe(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		part := data
		if i >= 0 {
			part = data[:i]
		}
		if !b.dropping && len(b.pending)+len(part) <= codexTelemetryMaxEventBytes {
			b.pending = append(b.pending, part...)
		} else {
			b.pending, b.event, b.dropping = nil, nil, true
		}
		if i < 0 {
			return
		}
		b.line(bytes.TrimSuffix(b.pending, []byte{'\r'}))
		if cap(b.pending) > 256<<10 {
			b.pending = nil
		} else {
			b.pending = b.pending[:0]
		}
		data = data[i+1:]
	}
}

func (b *codexTelemetryBody) line(line []byte) {
	if len(line) == 0 {
		b.processEvent(b.event)
		if cap(b.event) > 256<<10 {
			b.event = nil
		} else {
			b.event = b.event[:0]
		}
		b.dropping = false
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) && !b.dropping {
		part := bytes.TrimSpace(line[5:])
		if len(b.event)+len(part) <= codexTelemetryMaxEventBytes {
			b.event = append(b.event, part...)
		}
	}
}

// processEvent 解析 SSE 事件并记录首包、首 token 与终态。
func (b *codexTelemetryBody) processEvent(data []byte) {
	if !json.Valid(data) {
		return
	}
	now := time.Now()
	typ := gjson.GetBytes(data, "type").String()
	b.attempt.mu.Lock()
	if b.attempt.firstEvent.IsZero() {
		b.attempt.firstEvent = now
	}
	if b.attempt.firstToken.IsZero() && strings.HasSuffix(typ, ".delta") {
		b.attempt.firstToken = now
	}
	b.attempt.mu.Unlock()
	if b.attempt.timing {
		b.attempt.eventCount.Add(1)
	}
	switch typ {
	case "response.completed":
		b.attempt.finish("completed", data)
	case "response.failed", "error":
		b.attempt.finish("failed", data)
	case "response.incomplete":
		b.attempt.finish("interrupted", data)
	}
}

// flushJSON 在非流式响应结束时解析最终状态。
func (b *codexTelemetryBody) flushJSON() {
	if len(b.event) > 0 {
		b.processEvent(b.event)
		return
	}
	if json.Valid(b.pending) {
		status := gjson.GetBytes(b.pending, "status").String()
		if status == "completed" {
			b.attempt.finish("completed", b.pending)
		} else if status == "failed" {
			b.attempt.finish("failed", b.pending)
		}
	}
}
