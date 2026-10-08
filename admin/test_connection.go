package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var batchTestAccountTimeout = 30 * time.Second
var batchTestWhamTimeout = 5 * time.Second

// testEvent represents an SSE test event.
type testEvent struct {
	Type    string `json:"type"`              // test_start | content | diagnostics | test_complete | error
	Text    string `json:"text,omitempty"`    // Content text
	Model   string `json:"model,omitempty"`   // Test model
	Success bool   `json:"success,omitempty"` // Whether the test succeeded
	Error   string `json:"error,omitempty"`   // Error message
	// Diagnostics use channel-specific payloads: native Claude Messages tests use
	// diagnostics; Codex/Responses tests use codex_diagnostics. They are mutually exclusive.
	Diagnostics      *claudeTestDiagnostics `json:"diagnostics,omitempty"`
	CodexDiagnostics *codexTestDiagnostics  `json:"codex_diagnostics,omitempty"`
	// Interrupted 标记 error 事件是传输/流中断(连接失败、读流失败、无终态即断开),
	// 而非上游明确拒绝;降智检测据此决定是否自动重试及展示"已中断"。
	Interrupted bool `json:"interrupted,omitempty"`
}

type responsesTerminalOutcome uint8

const (
	responsesTerminalUnknown responsesTerminalOutcome = iota
	responsesTerminalSuccess
	responsesTerminalFailed
	responsesTerminalUsageLimited
)

func classifyResponsesTerminalEvent(data []byte) responsesTerminalOutcome {
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "response.completed":
		status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
		if status == "failed" || status == "incomplete" {
			if proxy.IsUsageLimitReachedError(data) {
				return responsesTerminalUsageLimited
			}
			return responsesTerminalFailed
		}
		return responsesTerminalSuccess
	case "response.failed", "error":
		if proxy.IsUsageLimitReachedError(data) {
			return responsesTerminalUsageLimited
		}
		return responsesTerminalFailed
	default:
		return responsesTerminalUnknown
	}
}

func (h *Handler) applyResponsesUsageLimitFailure(account *auth.Account, resp *http.Response, model string, payload []byte) bool {
	if h == nil || h.store == nil || account == nil || !proxy.IsUsageLimitReachedError(payload) {
		return false
	}
	proxy.Apply429Cooldown(h.store, account, payload, resp, model)
	return true
}

// TestConnection tests account connectivity and streams SSE results.
// GET /api/admin/accounts/:id/test
func (h *Handler) TestConnection(c *gin.Context) {
	h.testConnection(c, nil)
}

func (h *Handler) testConnection(c *gin.Context, quality *qualityTestRequest) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}

	// Find the runtime account, or create a temporary account for a recycle-bin test.
	// Temporary accounts do not participate in scheduling or persist account status.
	account := h.store.FindByID(id)
	isTransient := false
	if account == nil {
		if quality != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不在运行时池中"})
			return
		}
		transient, buildErr := h.store.BuildTransientAccountByID(c.Request.Context(), id)
		if buildErr != nil || transient == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不在运行时池中"})
			return
		}
		account = transient
		isTransient = true
	}
	// Although this SSE endpoint uses GET, it can update authorization, errors, cooldowns,
	// and recovery. Invalidate snapshots after streaming so confirmed 401s are not shown as unsampled.
	if !isTransient {
		defer h.invalidateAccountSnapshotCaches()
	}

	isClaudeAccount := account.IsClaudeOAuth()
	// Antigravity uses relay-style identities but requires Cloud Code v1internal payloads.
	// Use its dedicated executor for endpoint fallback and 429/503 quota handling.
	isAntigravityAccount := account.IsAntigravityAPI()
	isOpenAIResponsesAccount := account.IsRelayStyle() && !isClaudeAccount && !isAntigravityAccount
	// Agent Identity signs requests with its private key and does not require an access token.
	if !isOpenAIResponsesAccount && !isAntigravityAccount && !account.IsCodexAgentIdentity() && account.GetAccessToken() == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号没有可用的 Access Token，请先刷新"})
		return
	}
	// Refresh missing Antigravity OAuth access tokens before testing to avoid inevitable 401s.
	// Temporary recycle-bin accounts must not persist credential changes.
	if isAntigravityAccount && !isTransient && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth {
		if _, bearer := account.AntigravityCredentials(); bearer == "" {
			if refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account); refreshErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Antigravity 账号没有可用的 Access Token，刷新失败: " + refreshErr.Error()})
				return
			}
		}
	}

	requestedModel := strings.TrimSpace(c.Query("model"))
	if quality != nil {
		requestedModel = quality.Model
		if err := h.validateQualityTestForAccount(c.Request.Context(), account, *quality); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	testModel, err := h.connectionTestModelForAccount(c.Request.Context(), account, requestedModel)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	prompt := c.Query("prompt")
	hasPrompt := quality == nil && c.Request.URL.Query().Has("prompt")
	if hasPrompt && (strings.TrimSpace(prompt) == "" || len([]rune(prompt)) > auth.MaxTestContentRunes) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("测试内容不能为空，且不能超过 %d 个字符", auth.MaxTestContentRunes)})
		return
	}
	claudeSecurityCfg := h.store.ClaudeSecurityConfig()
	payload := h.buildAccountConnectionTestPayload(c.Request.Context(), account, testModel, claudeSecurityCfg)
	if quality != nil {
		payload, err = buildQualityTestPayload(account, testModel, *quality, claudeSecurityCfg)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else if hasPrompt {
		if isClaudeAccount {
			payload = buildClaudeConnectionTestPayloadWithContent(testModel, prompt, claudeSecurityCfg)
		} else {
			payload = buildTestPayloadWithContent(testModel, prompt)
		}
	}

	// Log probes with separate internal reasons for connectivity and quality tests.
	usageReason := connectionTestReason(quality)
	usageEndpoint := connectionTestEndpoint(account)
	usageEffort := connectionTestReasoningEffort(payload)

	// Set SSE response headers.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Send test_start.
	sendTestEvent(c, testEvent{Type: "test_start", Model: testModel})

	// Record recycle-bin test results; restore_on_success restores successful accounts.
	restoreOnSuccess := isTransient && strings.EqualFold(strings.TrimSpace(c.Query("restore_on_success")), "true")
	transientOutcome := "failed"
	if isTransient {
		defer func() {
			h.persistRecycleBinTestResult(id, transientOutcome)
		}()
	}

	// Build a minimal request based on sub2api createOpenAITestPayload.
	claudeFingerprintMode := ""
	if isClaudeAccount {
		claudeFingerprintMode = account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault())
	}

	// Send the request.
	start := time.Now()
	var resp *http.Response
	var reqErr error
	if isClaudeAccount {
		resp, reqErr = proxy.ExecuteClaudeMessagesRequest(c.Request.Context(), account, payload, h.store.ResolveProxyForAccount(account), c.Request.Header.Clone(), claudeFingerprintMode, claudeSecurityCfg)
	} else if isAntigravityAccount {
		resp, reqErr = h.executeAntigravityConnectionTest(c.Request.Context(), account, testModel, payload, h.store.ResolveProxyForAccount(account), !isTransient)
	} else if isOpenAIResponsesAccount {
		resp, reqErr = proxy.ExecuteRelayStyleRequest(c.Request.Context(), account, payload, h.store.ResolveProxyForAccount(account), nil)
	} else if quality != nil {
		// Quality probes use independent HTTP SSE without occupying a pooled
		// WebSocket or inheriting a previous conversation.
		resp, reqErr = proxy.ExecuteRequest(c.Request.Context(), account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil, false)
	} else {
		resp, reqErr = proxy.ExecuteRequest(c.Request.Context(), account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	}
	if reqErr != nil {
		if message, blocked := statePolicyTestFailure(reqErr, testModel); blocked {
			sendTestEvent(c, testEvent{Type: "error", Error: message})
			return
		}
		h.logConnectionTestTransportFailure(c, account, usageReason, usageEndpoint, testModel, usageEffort, start, reqErr)
		event := testEvent{Type: "error", Error: fmt.Sprintf("请求失败: %s", reqErr.Error()), Interrupted: true}
		if isClaudeAccount {
			event.Diagnostics = newClaudeTestRecorder(nil, testModel, claudeFingerprintMode, account.GetAccessToken(), start).finish()
			event.Error = sanitizeClaudeTestText(event.Error, account.GetAccessToken())
		} else {
			failed := newCodexTestRecorder(nil, testModel, account, start)
			event.CodexDiagnostics = failed.finish()
			event.Error = sanitizeCodexTestText(event.Error, failed.secrets)
		}
		sendTestEvent(c, event)
		return
	}
	defer resp.Body.Close()
	if isClaudeAccount {
		h.handleClaudeConnectionTest(c, account, resp, testModel, start, claudeFingerprintMode, isTransient, restoreOnSuccess, &transientOutcome, id, quality != nil, usageReason, usageEffort)
		return
	}

	// Send initial Codex/Responses diagnostics as soon as headers arrive, then final timing,
	// terminal status, usage, and output previews after the stream ends. Clients refresh
	// account snapshots after SSE closes, because final diagnostics follow the terminal event.
	recorder := newCodexTestRecorder(resp, testModel, account, start)
	defer func() {
		diagnostics := recorder.finish()
		sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: diagnostics})
		usage := connectionTestUsageFromCodex(diagnostics, testModel)
		usage.Reason, usage.Endpoint, usage.ReasoningEffort = usageReason, usageEndpoint, usageEffort
		h.logConnectionTestUsage(c, account, usage)
	}()
	sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: recorder.details})

	if resp.StatusCode != http.StatusOK {
		if !isOpenAIResponsesAccount && !isAntigravityAccount && !isTransient {
			proxy.SyncCodexUsageState(h.store, account, resp)
		}
		errBody, _ := io.ReadAll(resp.Body)
		recorder.observe(errBody)
		errMsg := fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(errBody), 500))
		if !isTransient {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", errMsg)
			case http.StatusPaymentRequired:
				// A 402 indicates an account billing/workspace rejection, not an unsampled account.
				if proxy.IsDeactivatedWorkspaceError(errBody) {
					h.store.MarkDeactivatedWorkspace(account, errMsg)
				} else {
					h.store.MarkError(account, errMsg)
				}
			case http.StatusForbidden:
				if proxy.IsAgentRuntimeDeletedError(errBody) {
					h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errMsg)
				} else if proxy.IsDeactivatedWorkspaceError(errBody) {
					h.store.MarkDeactivatedWorkspace(account, errMsg)
				}
			case http.StatusTooManyRequests:
				// Grok has its own free-usage-exhausted semantics, including a 24-hour cooldown.
				// Do not reduce exhausted quota to the generic one-minute relay cooldown.
				// Antigravity 429s use structured Google quota metadata and account/model retry hints.
				if isAntigravityAccount {
					proxy.ApplyAntigravityCooldown(h.store, account, resp.StatusCode, errBody, resp, testModel)
				} else if isOpenAIResponsesAccount && !account.IsGrokAPI() {
					h.store.MarkCooldown(account, time.Minute, "rate_limited")
				} else {
					proxy.Apply429Cooldown(h.store, account, errBody, resp, testModel)
				}
			case http.StatusServiceUnavailable:
				// Cloud Code 503 signals shared model capacity exhaustion and only warrants a short model cooldown.
				if isAntigravityAccount {
					proxy.ApplyAntigravityCooldown(h.store, account, resp.StatusCode, errBody, resp, testModel)
				}
			}
		}
		// Count 429 as rate limiting rather than an account failure.
		if isTransient && resp.StatusCode == http.StatusTooManyRequests {
			transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: errMsg})
		return
	}

	var usageState proxy.CodexUsageSyncResult
	// Antigravity does not provide x-codex-* usage headers.
	if !isOpenAIResponsesAccount && !isAntigravityAccount {
		usageStore := h.store
		if isTransient {
			usageStore = nil // Display temporary-account usage without persisting it.
		}
		usageState = proxy.SyncCodexUsageState(usageStore, account, resp)
		if !isTransient {
			applyUsageLimitedTestState(h.store, account, usageState)
		}
		if msg, limited := formatUsageLimitedTestError(usageState); limited {
			// Count exhausted quota as rate limiting rather than a failure.
			if isTransient {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: msg})
			return
		}
	}

	// Parse the SSE stream.
	hasContent := false
	gotTerminal := false
	sentTerminal := false
	var lastUpstreamEvent []byte
	emitContent := func(text string) {
		hasContent = true
		recorder.contentReceived()
		sendTestEvent(c, testEvent{Type: "content", Text: text})
	}
	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		recorder.observe(data)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			if delta != "" {
				emitContent(delta)
			}
		case "response.output_text.done":
			if !hasContent {
				text := gjson.GetBytes(data, "text").String()
				if text != "" {
					emitContent(text)
				}
			}
		case "response.content_part.done":
			if !hasContent {
				text := gjson.GetBytes(data, "part.text").String()
				if text != "" {
					emitContent(text)
				}
			}
		case "response.output_item.done":
			if !hasContent {
				text := extractOutputItemText(gjson.GetBytes(data, "item"))
				if text != "" {
					emitContent(text)
				}
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				sentTerminal = true
				if !isTransient {
					h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
				}
				if isTransient && proxy.IsUsageLimitReachedError(data) {
					transientOutcome = "rate_limited"
				}
				sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 "+status)})
				return false
			}
			if !hasContent {
				text := extractCompletedOutputText(data)
				if text != "" {
					emitContent(text)
				}
			}
			if !hasContent {
				sentTerminal = true
				sendTestEvent(c, testEvent{Type: "error", Error: formatNoOutputUpstreamError(data)})
				return false
			}
			// Successful tests reset failure/cooldown state; the scheduler still enforces usage limits.
			// Temporary recycle-bin accounts must not update scheduling state.
			if !isTransient && (quality == nil || !quality.modelQualityProbe) {
				h.store.ClearUnsupportedModelSince(account, testModel, start)
			}
			if !isTransient && (quality == nil || !quality.modelQualityProbe) && (isOpenAIResponsesAccount || usageState.UsageWindowLimitsIgnored || (!usageState.Premium5hRateLimited && (!usageState.HasUsage7d || usageState.UsagePct7d < 100))) {
				h.store.RecordManualTestSuccess(account, time.Since(start))
			}
			if isTransient {
				transientOutcome = "success"
				if restoreOnSuccess {
					restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
					restoreErr := h.restoreAccountByID(restoreCtx, id)
					restoreCancel()
					if restoreErr != nil {
						sendTestEvent(c, testEvent{Type: "content", Text: "\n\n--- 自动恢复失败: " + restoreErr.Error() + " ---"})
					}
				}
			}
			if quality == nil {
				duration := time.Since(start).Milliseconds()
				sendTestEvent(c, testEvent{
					Type: "content",
					Text: fmt.Sprintf("\n\n--- 耗时 %dms ---", duration),
				})
			}
			sendTestEvent(c, testEvent{Type: "test_complete", Success: true})
			sentTerminal = true
			return false
		case "response.failed":
			gotTerminal = true
			sentTerminal = true
			if !isTransient {
				h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
			}
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 response.failed")})
			return false
		case "error":
			gotTerminal = true
			sentTerminal = true
			if !isTransient {
				h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
			}
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 error 事件")})
			return false
		}
		return true
	})

	if readErr != nil && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: "读取上游流失败: " + readErr.Error(), Interrupted: true})
		return
	}
	if !gotTerminal && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: formatMissingTerminalUpstreamError(lastUpstreamEvent), Interrupted: true})
	}
}

func buildConnectionTestPayload(store *auth.Store, model string) []byte {
	content := auth.DefaultTestContent
	if store != nil {
		content = store.GetTestContent()
	}
	// Randomly select a configured line and expand variables (issue #320) to avoid identical
	// probe content across accounts. Single-line configuration keeps its existing behavior.
	return buildTestPayloadWithContent(model, auth.RenderTestContent(content))
}

// buildClaudeConnectionTestPayload builds the native Anthropic Messages
// shape used by Claude OAuth accounts. Keeping this separate from the
// Responses test payload prevents an imported Claude token from ever being
// sent through an OpenAI-shaped probe.
func buildClaudeConnectionTestPayload(store *auth.Store, model string, securityCfg auth.ClaudeSecurityConfig) []byte {
	content := auth.DefaultTestContent
	if store != nil {
		content = store.GetTestContent()
	}
	return buildClaudeConnectionTestPayloadWithContent(model, auth.RenderTestContent(content), securityCfg)
}

// buildAccountConnectionTestPayload uses native Claude Messages or Responses payloads.
// Channel-specific test content takes precedence over the global default.
func (h *Handler) buildAccountConnectionTestPayload(ctx context.Context, account *auth.Account, model string, securityCfg auth.ClaudeSecurityConfig) []byte {
	content := h.connectionTestContentForAccount(ctx, account)
	if account != nil && account.IsClaudeOAuth() {
		return buildClaudeConnectionTestPayloadWithContent(model, content, securityCfg)
	}
	return buildTestPayloadWithContent(model, content)
}

func buildClaudeConnectionTestPayloadWithContent(model string, content string, securityCfg auth.ClaudeSecurityConfig) []byte {
	content = auth.NormalizeTestContent(content)
	maxTokens := claudeProbeTokenBudget(securityCfg)
	body, err := json.Marshal(map[string]interface{}{
		"model":      strings.TrimSpace(model),
		"max_tokens": maxTokens,
		"stream":     true,
		"messages": []map[string]interface{}{{
			"role":    "user",
			"content": content,
		}},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":"claude-haiku-4-5","max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"ping"}]}`, maxTokens))
	}
	return body
}

func (h *Handler) handleClaudeConnectionTest(
	c *gin.Context,
	account *auth.Account,
	resp *http.Response,
	testModel string,
	start time.Time,
	fingerprintMode string,
	isTransient bool,
	restoreOnSuccess bool,
	transientOutcome *string,
	id int64,
	preserveWhitespace bool,
	usageReason string,
	usageEffort string,
) {
	// For API Key accounts fingerprintMode already carries the account-level
	// client-identity emulation mode (empty = passthrough), so it is reported
	// as-is instead of being blanked.
	recorder := newClaudeTestRecorder(resp, testModel, fingerprintMode, account.GetAccessToken(), start)
	// The final diagnostics follow the terminal result; clients must drain the
	// SSE response before refreshing the invalidated account snapshot.
	defer func() {
		diagnostics := recorder.finish()
		sendTestEvent(c, testEvent{Type: "diagnostics", Diagnostics: diagnostics})
		usage := connectionTestUsageFromClaude(diagnostics, testModel)
		usage.Reason, usage.Endpoint, usage.ReasoningEffort = usageReason, connectionTestEndpoint(account), usageEffort
		h.logConnectionTestUsage(c, account, usage)
	}()
	if resp == nil {
		sendTestEvent(c, testEvent{Type: "error", Error: "Claude 上游未返回响应"})
		return
	}
	sendTestEvent(c, testEvent{Type: "diagnostics", Diagnostics: recorder.details})
	usageStore := h.store
	if isTransient {
		usageStore = nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, claudeTestBodyLimit+1))
		if len(body) > claudeTestBodyLimit {
			recorder.capture.truncated = true
		}
		recorder.observe(body)
		message := fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(sanitizeClaudeTestText(string(body), recorder.accessToken), 500))
		creditsRequired := false
		if account.IsClaudeOAuth() {
			creditsRequired = syncClaudeTestUsageState(usageStore, account, testModel, resp, body)
			if creditsRequired {
				message = fmt.Sprintf("上游模型 %s 需要 usage credits，当前账号套餐不可用", testModel)
			}
		}
		if !isTransient && !creditsRequired {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", message)
			case http.StatusPaymentRequired:
				if proxy.IsDeactivatedWorkspaceError(body) {
					h.store.MarkDeactivatedWorkspace(account, message)
				} else {
					h.store.MarkError(account, message)
				}
			case http.StatusForbidden:
				if proxy.IsDeactivatedWorkspaceError(body) {
					h.store.MarkDeactivatedWorkspace(account, message)
				}
			}
		}
		if isTransient && resp.StatusCode == http.StatusTooManyRequests && transientOutcome != nil {
			*transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: message})
		return
	}
	if account.IsClaudeOAuth() {
		proxy.SyncClaudeUsageState(usageStore, account, resp)
	}
	status, detail := readClaudeMessagesStreamObserved(c.Request.Context(), resp, func(text string) {
		if text != "" && (preserveWhitespace || strings.TrimSpace(text) != "") {
			recorder.contentReceived()
			sendTestEvent(c, testEvent{Type: "content", Text: text})
		}
	}, recorder.observe, preserveWhitespace)
	if preserveWhitespace && recorder.details.StopReason == "max_tokens" {
		sendTestEvent(c, testEvent{Type: "error", Error: "输出达到模型 token 上限，HTML 可能不完整"})
		return
	}
	if status != "success" {
		if !isTransient {
			applyClaudeConnectionStreamFailure(h, account, testModel, status, detail, resp)
		}
		if status == "rate_limited" && transientOutcome != nil && isTransient {
			*transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: sanitizeClaudeTestText(detail, recorder.accessToken)})
		return
	}
	if !isTransient && claudeConnectionTestShouldPreserveUsageCooldown(account, resp) {
		// A native Messages response can carry a valid body while explicitly
		// reporting a rejected/exhausted quota window. It is not evidence that
		// the account recovered; never let the manual-test success path erase the
		// authoritative cooldown just created by the same response.
		sendTestEvent(c, testEvent{Type: "error", Error: "Claude 上游返回了有效响应，但账号仍处于配额/限流状态"})
		return
	}
	if isTransient && claudeConnectionTestShouldPreserveUsageCooldown(account, resp) {
		if transientOutcome != nil {
			*transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: "Claude 上游返回了有效响应，但账号仍处于配额/限流状态"})
		return
	}
	if isTransient {
		if transientOutcome != nil {
			*transientOutcome = "success"
		}
		if restoreOnSuccess {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
			restoreErr := h.restoreAccountByID(restoreCtx, id)
			restoreCancel()
			if restoreErr != nil {
				sendTestEvent(c, testEvent{Type: "content", Text: "\n\n--- 自动恢复失败: " + restoreErr.Error() + " ---"})
			}
		}
	} else {
		h.store.RecordManualTestSuccess(account, time.Since(start))
	}
	// A successful explicit model probe invalidates the prior model cooldown immediately,
	// rather than waiting for the original 30-minute window to expire.
	if err := h.store.RestoreClaudeAccountModel(c.Request.Context(), account, testModel); err != nil {
		sendTestEvent(c, testEvent{Type: "error", Error: "模型复探成功，但恢复模型清单失败"})
		return
	}
	if account.ClaudeModelWasRejected(testModel) || account.IsModelRateLimited(testModel) {
		h.store.ClearModelCooldown(account, testModel)
	}
	proxy.NoteClaudeGatedModelSuccess(h.store, account, testModel)
	sendTestEvent(c, testEvent{Type: "test_complete", Success: true})
}

// applyClaudeConnectionStreamFailure makes a body-only native error visible to
// the account scheduler. Anthropic may return HTTP 200 with an SSE error event,
// so the ordinary HTTP status handlers cannot establish a short cooldown.
func applyClaudeConnectionStreamFailure(h *Handler, account *auth.Account, model, status, detail string, resp *http.Response) {
	if h == nil || h.store == nil || account == nil || !account.IsClaudeOAuth() {
		return
	}
	switch status {
	case "rate_limited":
		if claudeConnectionDetailRequiresCredits(h.store, account, model, detail) {
			return
		}
		// The caller already synchronized response headers before consuming the
		// stream. Never replace a precise 5h/7d cooldown with the generic one
		// minute fallback when those headers were authoritative.
		if claudeResponseHasUsageLimitSignal(resp) {
			return
		}
		headers := make(http.Header)
		if resp != nil {
			if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
				headers.Set("Retry-After", retryAfter)
			}
		}
		proxy.SyncClaudeUsageState(h.store, account, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	case "failed":
		lower := strings.ToLower(strings.TrimSpace(detail))
		if strings.Contains(lower, "authentication") || strings.Contains(lower, "unauthor") || strings.Contains(lower, "invalid token") || strings.Contains(lower, "invalid_token") {
			h.store.MarkCooldownWithError(account, 5*time.Minute, "unauthorized", "Claude 测试返回授权失败: "+truncate(detail, 300))
		}
	}
}

// syncClaudeTestUsageState keeps connection tests from turning a model-level
// credits_required response into an account-level cooldown. It returns true
// only when the response was handled as a model entitlement failure.
func syncClaudeTestUsageState(store *auth.Store, account *auth.Account, model string, resp *http.Response, body []byte) bool {
	if store == nil || account == nil || !account.IsClaudeOAuth() || resp == nil {
		return false
	}
	if proxy.HandleClaudeModelBillingRejection(store, account, model, resp.StatusCode, body) {
		return true
	}
	proxy.SyncClaudeUsageState(store, account, resp)
	return false
}

func claudeConnectionDetailRequiresCredits(store *auth.Store, account *auth.Account, model, detail string) bool {
	lower := strings.ToLower(strings.TrimSpace(detail))
	if !strings.Contains(lower, "credits_required") && !strings.Contains(lower, "usage credits") {
		return false
	}
	body := []byte(fmt.Sprintf(`{"error":{"details":{"error_code":"credits_required","model":%q}}}`, strings.TrimSpace(model)))
	return proxy.HandleClaudeModelBillingRejection(store, account, model, http.StatusTooManyRequests, body)
}

func claudeResponseHasUsageLimitSignal(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(resp.Header.Get("anthropic-ratelimit-unified-status")))
	if resp.StatusCode == http.StatusTooManyRequests || status == "rejected" {
		return true
	}
	claim := strings.ToLower(strings.TrimSpace(resp.Header.Get("anthropic-ratelimit-unified-representative-claim")))
	if claim != "five_hour" && claim != "five-hour" && claim != "5h" && claim != "seven_day" && claim != "seven-day" && claim != "7d" {
		return false
	}
	key := "anthropic-ratelimit-unified-5h-utilization"
	if claim == "seven_day" || claim == "seven-day" || claim == "7d" {
		key = "anthropic-ratelimit-unified-7d-utilization"
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(resp.Header.Get(key)), 64)
	if err == nil && ((value <= 1.5 && value >= 1) || value >= 100) {
		return true
	}
	return false
}

func claudeConnectionTestShouldPreserveUsageCooldown(account *auth.Account, resp *http.Response) bool {
	if !claudeResponseHasUsageLimitSignal(resp) {
		return false
	}
	// The response headers/event are authoritative even for a transient account
	// that intentionally does not persist state. Returning true prevents a
	// rejected 200 body from being treated as a successful recovery and restored
	// into the active pool.
	if account == nil {
		return true
	}
	return true
}

// buildTestPayload builds the default minimal test request.
func buildTestPayload(model string) []byte {
	return buildTestPayloadWithContent(model, auth.DefaultTestContent)
}

// buildTestPayloadWithContent builds a minimal request with custom user input.
func buildTestPayloadWithContent(model string, content string) []byte {
	content = auth.NormalizeTestContent(content)
	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "model", model)
	payload, _ = sjson.SetBytes(payload, "input", []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": content,
				},
			},
		},
	})
	payload, _ = sjson.SetBytes(payload, "stream", true)
	payload, _ = sjson.SetBytes(payload, "store", false)
	payload, _ = sjson.SetBytes(payload, "instructions", "You are a helpful assistant. Reply briefly.")
	return payload
}

func formatUsageLimitedTestError(state proxy.CodexUsageSyncResult) (string, bool) {
	if state.UsageWindowLimitsIgnored {
		return "", false
	}
	if state.Premium5hRateLimited {
		remaining := time.Until(state.Reset5hAt).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("上游探针返回 200，但 Codex 5h 用量头已达 %.0f%%，账号已保持限流状态，预计 %s 后恢复。", state.UsagePct5h, remaining), true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return fmt.Sprintf("上游探针返回 200，但 Codex 7d 用量头已达 %.0f%%，账号已标记为限流/用量耗尽状态。", state.UsagePct7d), true
	}
	return "", false
}

func applyUsageLimitedAccountState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) bool {
	if store == nil || account == nil {
		return false
	}
	if state.UsageWindowLimitsIgnored {
		return false
	}
	if state.Premium5hRateLimited || state.Usage7dRateLimited {
		return true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return store.MarkUsage7dRateLimited(account)
	}
	return false
}

func applyUsageLimitedTestState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) {
	applyUsageLimitedAccountState(store, account, state)
}

// sendTestEvent emits an SSE event.
func sendTestEvent(c *gin.Context, event testEvent) {
	rememberConnectionTestError(c, event)
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("序列化测试事件失败: %v", err)
		return
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		log.Printf("写入 SSE 事件失败: %v", err)
		return
	}
	c.Writer.Flush()
}

// truncate limits string length.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractCompletedOutputText(data []byte) string {
	if text := gjson.GetBytes(data, "response.output_text").String(); text != "" {
		return text
	}
	return extractOutputItemText(gjson.GetBytes(data, "response"))
}

func extractOutputItemText(item gjson.Result) string {
	var b strings.Builder
	writeTextFromOutputItem(&b, item)
	return b.String()
}

func writeTextFromOutputItem(b *strings.Builder, item gjson.Result) {
	if !item.Exists() {
		return
	}
	switch item.Get("type").String() {
	case "output_text", "text":
		b.WriteString(item.Get("text").String())
	case "message", "assistant":
		writeTextFromContentArray(b, item.Get("content"))
	default:
		if output := item.Get("output"); output.IsArray() {
			output.ForEach(func(_, child gjson.Result) bool {
				writeTextFromOutputItem(b, child)
				return true
			})
		}
		writeTextFromContentArray(b, item.Get("content"))
	}
}

func writeTextFromContentArray(b *strings.Builder, content gjson.Result) {
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "output_text" || partType == "text" {
			b.WriteString(part.Get("text").String())
		}
		return true
	})
}

func formatUpstreamTestError(data []byte, fallback string) string {
	msg := firstNonEmptyGJSONString(data,
		"response.status_details.error.message",
		"response.error.message",
		"error.message",
		"message",
		"response.incomplete_details.reason",
		"response.status_details.message",
	)
	if msg == "" {
		msg = fallback
	}

	code := firstNonEmptyGJSONString(data,
		"response.status_details.error.code",
		"response.error.code",
		"error.code",
	)
	if code != "" && !strings.Contains(msg, code) {
		msg += " (code: " + code + ")"
	}

	return formatUpstreamEventDetail(msg, data)
}

func formatNoOutputUpstreamError(data []byte) string {
	msg := "上游已完成但没有返回文本输出"
	if status := gjson.GetBytes(data, "response.status").String(); status != "" && status != "completed" {
		msg = "上游响应状态: " + status
	}
	if reason := gjson.GetBytes(data, "response.incomplete_details.reason").String(); reason != "" {
		msg += " (" + reason + ")"
	}
	return formatUpstreamEventDetail(msg, data)
}

func formatMissingTerminalUpstreamError(lastEvent []byte) string {
	if len(lastEvent) == 0 {
		return "上游流结束但未收到任何事件"
	}
	return formatUpstreamEventDetail("上游流提前结束，未收到 response.completed 或 response.failed", lastEvent)
}

func firstNonEmptyGJSONString(data []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func formatUpstreamEventDetail(message string, data []byte) string {
	if len(data) == 0 {
		return message
	}
	detail := string(data)
	var parsed any
	if err := json.Unmarshal(data, &parsed); err == nil {
		if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
			detail = string(pretty)
		}
	}
	return message + "\n\n上游事件:\n" + truncate(detail, 3000)
}

func isSupportedConnectionTestModel(model string) bool {
	if strings.Contains(strings.ToLower(model), "image") {
		return false
	}
	for _, supported := range proxy.SupportedModels {
		if model == supported {
			return true
		}
	}
	return false
}

func (h *Handler) connectionTestModel(ctx context.Context) string {
	model := strings.TrimSpace(h.store.GetTestModel())
	if proxy.IsTextTestModelID(ctx, h.db, model) {
		return model
	}
	models := proxy.TextTestModelIDs(ctx, h.db)
	if len(models) > 0 {
		return models[0]
	}
	return auth.DefaultTestModel
}

// defaultGrokConnectionTestModels supplies text models when an account has no catalog.
// It shares the default catalog used by /v1/models.
// OAuth uses the CLI channel and API keys use the public API, so account context is required.
func defaultGrokConnectionTestModels(account *auth.Account) []string {
	return proxy.DefaultGrokModelIDsForAccount(account)
}

func (h *Handler) connectionTestModelForAccount(ctx context.Context, account *auth.Account, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if account != nil && account.IsClaudeOAuth() {
		models := claudeProbeModelIDs(account)
		if requested != "" {
			if strings.HasPrefix(strings.ToLower(requested), "claude-") && account.ClaudeModelWasRejected(requested) {
				return requested, nil
			}
			if h != nil && h.db != nil && account.DBID > 0 {
				row, err := h.db.GetAccountByID(ctx, account.DBID)
				if err == nil && row != nil {
					persistedModels := row.GetCredentialStringSlice("models")
					if len(persistedModels) > 0 {
						persistedMatch := false
						for _, persisted := range persistedModels {
							if strings.EqualFold(strings.TrimSpace(persisted), requested) {
								persistedMatch = true
								break
							}
						}
						if !persistedMatch {
							return "", fmt.Errorf("该 Claude 账号的持久化模型清单不支持测试模型: %s", requested)
						}
					}
				}
			}
			// Explicit model probes may bypass model cooldowns, including credits_required, so
			// operators can verify newly purchased credits or plan changes. Rejected probes restore
			// the cooldown; successful callers clear it. Only automatic selection skips cooled models.
			for _, model := range models {
				if strings.EqualFold(strings.TrimSpace(model), requested) {
					return strings.TrimSpace(model), nil
				}
			}
			return "", fmt.Errorf("该 Claude 账号不支持测试模型: %s", requested)
		}
		if len(models) == 0 {
			return "", fmt.Errorf("该 Claude 账号没有可用于测试的文本模型")
		}
		// Prefer the configured Claude test model when it is listed and not cooling down.
		// Otherwise select automatically instead of failing due to an unsuitable configuration.
		if configured := h.channelTestSettingsForAccount(ctx, account).TestModel; configured != "" {
			for _, candidate := range models {
				if strings.EqualFold(strings.TrimSpace(candidate), configured) && !account.IsModelRateLimited(candidate) {
					return strings.TrimSpace(candidate), nil
				}
			}
		}
		for _, candidate := range models {
			if !account.IsModelRateLimited(candidate) && strings.Contains(strings.ToLower(candidate), "haiku") {
				return strings.TrimSpace(candidate), nil
			}
		}
		for _, candidate := range models {
			if !account.IsModelRateLimited(candidate) {
				return strings.TrimSpace(candidate), nil
			}
		}
		return "", fmt.Errorf("该 Claude 账号的文本模型均处于模型级冷却")
	}
	if account != nil && account.IsAntigravityAPI() {
		defaults := []string{h.channelTestSettingsForAccount(ctx, account).TestModel}
		if h != nil && h.store != nil {
			defaults = append(defaults, strings.TrimSpace(h.store.GetTestModel()))
		}
		return antigravityConnectionTestModel(account, requested, defaults...)
	}
	if account == nil || !account.IsRelayStyle() {
		if requested == "" {
			return h.connectionTestModel(ctx), nil
		}
		if !proxy.IsTextTestModelID(ctx, h.db, requested) {
			return "", fmt.Errorf("不支持的测试模型: %s", requested)
		}
		return requested, nil
	}

	models := account.OpenAIResponsesModels()
	textModels := make([]string, 0, len(models))
	for _, model := range models {
		if isTextConnectionModel(model) {
			textModels = append(textModels, strings.TrimSpace(model))
		}
	}
	// Grok may have no declared models until /v1/models is queried; use common text models.
	if len(textModels) == 0 && account.IsGrokAPI() {
		textModels = append(textModels, defaultGrokConnectionTestModels(account)...)
	}
	if len(textModels) == 0 {
		return "", fmt.Errorf("该 Responses API 账号没有可用于测试的文本模型")
	}
	if requested != "" {
		if mappedModel, ok := proxy.ResolveAccountModelMapping(account, requested); ok && mappedModel != "" {
			for _, model := range textModels {
				if strings.EqualFold(model, mappedModel) {
					return mappedModel, nil
				}
			}
		}
		for _, model := range textModels {
			if strings.EqualFold(model, requested) {
				return model, nil
			}
		}
		return "", fmt.Errorf("该账号不支持测试模型: %s", requested)
	}

	defaultModel := strings.TrimSpace(h.store.GetTestModel())
	for _, model := range textModels {
		if strings.EqualFold(model, defaultModel) {
			return model, nil
		}
	}
	return textModels[0], nil
}

func isTextConnectionModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	return model != "" && !strings.Contains(model, "image")
}

// antigravityConnectionTestModels returns text models eligible for connection testing.
// Project the synchronized wire catalog or safe defaults onto published fixed-tier IDs,
// matching the account selector and /v1/models.
func antigravityConnectionTestModels(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	published := proxy.AntigravityPublishedModelIDs(account.AntigravityModels())
	models := make([]string, 0, len(published))
	for _, model := range published {
		if isTextConnectionModel(model) {
			models = append(models, strings.TrimSpace(model))
		}
	}
	return models
}

// antigravityConnectionTestModel selects a model for a connection test. Explicit probes
// may bypass model cooldowns. Automatic selection tries channel and global defaults,
// then prefers the newest low-tier flash model before falling back to the first
// model without a cooldown.
func antigravityConnectionTestModel(account *auth.Account, requested string, defaultModels ...string) (string, error) {
	models := antigravityConnectionTestModels(account)
	if len(models) == 0 {
		return "", fmt.Errorf("该 Antigravity 账号没有可用于测试的文本模型")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, model := range models {
			if strings.EqualFold(model, requested) {
				return model, nil
			}
		}
		return "", fmt.Errorf("该 Antigravity 账号不支持测试模型: %s", requested)
	}
	for _, defaultModel := range defaultModels {
		defaultModel = strings.TrimSpace(defaultModel)
		if defaultModel == "" {
			continue
		}
		for _, model := range models {
			if strings.EqualFold(model, defaultModel) && !account.IsModelRateLimited(model) {
				return model, nil
			}
		}
	}
	if model := preferredAntigravityFlashLowModel(models, account.IsModelRateLimited); model != "" {
		return model, nil
	}
	for _, model := range models {
		if !account.IsModelRateLimited(model) {
			return model, nil
		}
	}
	return models[0], nil
}

// preferredAntigravityFlashLowModel selects the highest-version low-tier flash model.
// Synchronized catalogs can retain retired versions that only return retirement notices.
// Taking the first catalog entry could therefore select an unusable model.
func preferredAntigravityFlashLowModel(models []string, rateLimited func(string) bool) string {
	best := ""
	bestVersion := -1.0
	for _, model := range models {
		lower := strings.ToLower(strings.TrimSpace(model))
		if !strings.Contains(lower, "flash") || !strings.HasSuffix(lower, "-low") {
			continue
		}
		if rateLimited != nil && rateLimited(model) {
			continue
		}
		version := antigravityModelVersion(lower)
		if best == "" || version > bestVersion {
			best, bestVersion = strings.TrimSpace(model), version
		}
	}
	return best
}

// antigravityModelVersion extracts gemini-<major>.<minor> and returns zero on failure.
func antigravityModelVersion(model string) float64 {
	rest := strings.TrimPrefix(model, "gemini-")
	if rest == model {
		return 0
	}
	end := 0
	for end < len(rest) && (rest[end] == '.' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0
	}
	version, err := strconv.ParseFloat(strings.TrimSuffix(rest[:end], "."), 64)
	if err != nil {
		return 0
	}
	return version
}

// executeAntigravityConnectionTest uses the dedicated Antigravity executor and returns
// normalized Responses SSE for the shared Codex stream parser.
// As in regular scheduling, OAuth 401s trigger one token refresh and retry.
// allowRefresh=false keeps temporary recycle-bin credentials unchanged.
func (h *Handler) executeAntigravityConnectionTest(ctx context.Context, account *auth.Account, model string, payload []byte, proxyURL string, allowRefresh bool) (*http.Response, error) {
	execute := h.antigravityProbeExecutor()
	resp, err := execute(ctx, account, model, payload, true, proxyURL)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized || !allowRefresh || h == nil || h.store == nil ||
		account.AntigravityAuthKind() != auth.AntigravityAuthKindOAuth {
		return resp, nil
	}
	if refreshErr := h.store.RefreshAntigravityAccount(ctx, account); refreshErr != nil {
		// If refresh fails, return the original 401 for authorization handling.
		return resp, nil
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return execute(ctx, account, model, payload, true, proxyURL)
}

type batchTestRequest struct {
	IDs      *[]int64                  `json:"ids"`
	Selector *accountOperationSelector `json:"selector,omitempty"`
	// RestoreOnSuccess restores successful recycle-bin batch tests to the account pool.
	RestoreOnSuccess bool `json:"restore_on_success"`
}

// persistRecycleBinTestResult stores recycle-bin test results in account credentials.
func (h *Handler) persistRecycleBinTestResult(id int64, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{
		"recycle_last_test_status": status,
		"recycle_last_test_at":     time.Now().Format(time.RFC3339),
	}); err != nil {
		log.Printf("写入回收站测试结果失败 (account %d): %v", id, err)
	}
}

type batchOperationEvent struct {
	Type         string `json:"type"` // start | progress | complete
	Action       string `json:"action"`
	Status       string `json:"status,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Current      int    `json:"current"`
	Total        int    `json:"total"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	Banned       int64  `json:"banned,omitempty"`
	RateLimited  int64  `json:"rate_limited,omitempty"`
	Deleted      int64  `json:"deleted,omitempty"`
	AccountID    int64  `json:"account_id,omitempty"`
	AccountName  string `json:"account_name,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
}

func runtimeAccountOperationIdentity(account *auth.Account) (string, string) {
	if account == nil {
		return "", ""
	}
	account.Mu().RLock()
	email := strings.TrimSpace(account.Email)
	account.Mu().RUnlock()
	return "", email
}

func batchOperationHTTPStatus(status, message string) int {
	if status == "success" {
		return http.StatusOK
	}

	normalized := strings.ToLower(message)
	for _, marker := range []string{
		"上游返回 ",
		"http ",
		"status code ",
		"status ",
		"status=",
		"status:",
		"状态码 ",
	} {
		index := strings.Index(normalized, marker)
		if index < 0 {
			continue
		}
		remainder := strings.TrimLeft(normalized[index+len(marker):], " :=-")
		if len(remainder) < 3 {
			continue
		}
		code, err := strconv.Atoi(remainder[:3])
		if err == nil && code >= 100 && code <= 599 {
			return code
		}
	}
	return 0
}

type batchTestCounts struct {
	Total       int
	Success     int64
	Failed      int64
	Banned      int64
	RateLimited int64
}

func resolveBatchTestAccounts(store *auth.Store, ids *[]int64) ([]*auth.Account, int) {
	if store == nil {
		return nil, 0
	}
	if ids == nil {
		return store.Accounts(), 0
	}

	accounts := make([]*auth.Account, 0, len(*ids))
	missing := 0
	seen := make(map[int64]struct{}, len(*ids))
	for _, id := range *ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		acc := store.FindByID(id)
		if acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}
	return accounts, missing
}

// BatchTest tests the specified account IDs, or all accounts when IDs are omitted.
// POST /api/admin/accounts/batch-test
func (h *Handler) BatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids 与 selector 不能同时提供")
		return
	}
	if req.Selector != nil {
		selectorCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		ids, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		cancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		req.IDs = &ids
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要测试的账号 ID 列表")
		return
	}

	accounts, missingCount := resolveBatchTestAccounts(h.store, req.IDs)
	h.serveBatchTest(c, accounts, missingCount, h.runSingleBatchTest)
}

// RecycleBinBatchTest tests specified recycle-bin IDs, or all recycle-bin accounts.
// Temporary accounts do not participate in scheduling or update account status.
// POST /api/admin/accounts/recycle-bin/batch-test
func (h *Handler) RecycleBinBatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要测试的账号 ID 列表")
		return
	}
	if req.Selector != nil {
		writeError(c, http.StatusBadRequest, "回收站批量测试不支持 selector")
		return
	}

	listCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	rows, err := h.db.ListDeleted(listCtx)
	cancel()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	binIDs := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		binIDs[row.ID] = struct{}{}
	}

	wanted := make([]int64, 0, len(rows))
	missing := 0
	if req.IDs == nil {
		for _, row := range rows {
			wanted = append(wanted, row.ID)
		}
	} else {
		seen := make(map[int64]struct{}, len(*req.IDs))
		for _, id := range *req.IDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if _, ok := binIDs[id]; ok {
				wanted = append(wanted, id)
			} else {
				missing++
			}
		}
	}

	accounts := make([]*auth.Account, 0, len(wanted))
	for _, id := range wanted {
		buildCtx, buildCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		acc, buildErr := h.store.BuildTransientAccountByID(buildCtx, id)
		buildCancel()
		if buildErr != nil || acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}

	restoreOnSuccess := req.RestoreOnSuccess
	testFn := func(ctx context.Context, acc *auth.Account) (string, string) {
		status, msg := h.runRecycleBinSingleTest(ctx, acc)
		h.persistRecycleBinTestResult(acc.DBID, status)
		if status == "success" && restoreOnSuccess {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
			restoreErr := h.restoreAccountByID(restoreCtx, acc.DBID)
			restoreCancel()
			if restoreErr != nil {
				msg = "测试通过，但自动恢复失败: " + restoreErr.Error()
			} else {
				msg = "测试通过，已自动恢复"
			}
		}
		return status, msg
	}
	h.serveBatchTest(c, accounts, missing, testFn)
}

// serveBatchTest handles streamed and non-streamed batch results using testFn.
func (h *Handler) serveBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchTest(c, accounts, missingCount, testFn)
		return
	}

	if len(accounts) == 0 && missingCount == 0 {
		c.JSON(http.StatusOK, gin.H{"total": 0, "success": 0, "failed": 0, "banned": 0, "rate_limited": 0})
		return
	}

	counts := h.runBatchTest(c.Request.Context(), accounts, missingCount, testFn, nil)
	c.JSON(http.StatusOK, gin.H{
		"total":        counts.Total,
		"success":      counts.Success,
		"failed":       counts.Failed,
		"banned":       counts.Banned,
		"rate_limited": counts.RateLimited,
	})
}

func (h *Handler) streamBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	total := len(accounts) + missingCount
	setupSSE(c)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_test", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_test"})
		return
	}

	events := make(chan batchOperationEvent, len(accounts)+2)
	ctx := c.Request.Context()
	go func() {
		counts := h.runBatchTest(ctx, accounts, missingCount, testFn, func(event batchOperationEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		select {
		case events <- batchOperationEvent{
			Type:        "complete",
			Action:      "batch_test",
			Current:     counts.Total,
			Total:       counts.Total,
			Success:     counts.Success,
			Failed:      counts.Failed,
			Banned:      counts.Banned,
			RateLimited: counts.RateLimited,
		}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for event := range events {
		sendSSEJSON(c, event)
	}
}

func (h *Handler) runBatchTest(ctx context.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string), onProgress func(batchOperationEvent)) batchTestCounts {
	total := len(accounts) + missingCount
	concurrency := h.batchTestConcurrency(ctx, accounts)

	var (
		successCount   int64
		failedCount    = int64(missingCount)
		bannedCount    int64
		rateLimitCount int64
		completedCount = int64(missingCount)
		wg             sync.WaitGroup
		sem            = make(chan struct{}, concurrency)
	)

	if missingCount > 0 && onProgress != nil {
		onProgress(batchOperationEvent{
			Type:    "progress",
			Action:  "batch_test",
			Current: missingCount,
			Total:   total,
			Failed:  failedCount,
			Error:   fmt.Sprintf("%d 个账号不在运行时池中", missingCount),
		})
	}

	for _, account := range accounts {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&failedCount, 1)
				h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, "failed", "测试已取消")
				return
			}
			defer func() { <-sem }()

			status, message := testFn(ctx, acc)
			switch status {
			case "success":
				atomic.AddInt64(&successCount, 1)
			case "banned":
				atomic.AddInt64(&bannedCount, 1)
			case "rate_limited":
				atomic.AddInt64(&rateLimitCount, 1)
			default:
				atomic.AddInt64(&failedCount, 1)
			}
			h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, status, message)
		}(account)
	}

	wg.Wait()
	return batchTestCounts{
		Total:       total,
		Success:     atomic.LoadInt64(&successCount),
		Failed:      atomic.LoadInt64(&failedCount),
		Banned:      atomic.LoadInt64(&bannedCount),
		RateLimited: atomic.LoadInt64(&rateLimitCount),
	}
}

func (h *Handler) emitBatchTestProgress(
	onProgress func(batchOperationEvent),
	accountID int64,
	total int,
	completedCount *int64,
	successCount *int64,
	failedCount *int64,
	bannedCount *int64,
	rateLimitCount *int64,
	status string,
	message string,
) {
	if onProgress == nil {
		return
	}
	accountName, accountEmail := h.accountOperationIdentity(accountID)
	current := int(atomic.AddInt64(completedCount, 1))
	event := batchOperationEvent{
		Type:         "progress",
		Action:       "batch_test",
		Status:       status,
		HTTPStatus:   batchOperationHTTPStatus(status, message),
		Current:      current,
		Total:        total,
		Success:      atomic.LoadInt64(successCount),
		Failed:       atomic.LoadInt64(failedCount),
		Banned:       atomic.LoadInt64(bannedCount),
		RateLimited:  atomic.LoadInt64(rateLimitCount),
		AccountID:    accountID,
		AccountName:  accountName,
		AccountEmail: accountEmail,
		Message:      message,
	}
	if status == "failed" {
		event.Error = message
	}
	onProgress(event)
}

func (h *Handler) runSingleBatchTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()
	if acc == nil {
		return "failed", "账号不存在"
	}

	if !acc.IsRelayStyle() && !acc.IsCodexAgentIdentity() && acc.GetAccessToken() == "" {
		acc.Mu().RLock()
		hasRefreshToken := acc.RefreshToken != ""
		acc.Mu().RUnlock()
		if !hasRefreshToken {
			h.store.MarkError(acc, "批量测试失败: 账号缺少 access_token 和 refresh_token")
		}
		return "failed", "账号缺少 access_token 和 refresh_token"
	}

	if status, msg, done := h.batchTestSkipDeactivatedWorkspace(acc); done {
		return status, msg
	}

	if status, msg, done := h.batchTestWhamPreflight(testCtx, acc); done {
		return status, msg
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		h.store.MarkError(acc, "批量测试失败: "+modelErr.Error())
		return "failed", modelErr.Error()
	}
	securityCfg := h.store.ClaudeSecurityConfig()
	payload := h.buildAccountConnectionTestPayload(testCtx, acc, testModel, securityCfg)
	start := time.Now()

	var resp *http.Response
	var err error
	if acc.IsClaudeOAuth() {
		resp, err = proxy.ExecuteClaudeMessagesRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil, acc.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault()), securityCfg)
	} else if acc.IsAntigravityAPI() {
		resp, err = h.executeAntigravityConnectionTest(testCtx, acc, testModel, payload, h.store.ResolveProxyForAccount(acc), true)
	} else if acc.IsRelayStyle() {
		resp, err = proxy.ExecuteRelayStyleRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil)
	} else {
		resp, err = proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	}
	if err != nil {
		if message, blocked := statePolicyTestFailure(err, testModel); blocked {
			return "failed", message
		}
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			if errors.Is(testCtx.Err(), context.DeadlineExceeded) {
				h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
			}
			return "failed", msg
		}
		h.store.MarkError(acc, "批量测试请求失败: "+err.Error())
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if acc.IsClaudeOAuth() {
			proxy.SyncClaudeUsageState(h.store, acc, resp)
			status, msg := readClaudeMessagesStream(testCtx, resp, nil)
			if status != "success" {
				applyClaudeConnectionStreamFailure(h, acc, testModel, status, msg, resp)
			}
			if status == "rate_limited" {
				return "rate_limited", msg
			}
			if status != "success" {
				return "failed", msg
			}
			proxy.NoteClaudeGatedModelSuccess(h.store, acc, testModel)
		} else if !acc.IsRelayStyle() {
			usageState := proxy.SyncCodexUsageState(h.store, acc, resp)
			applyUsageLimitedTestState(h.store, acc, usageState)
			if msg, limited := formatUsageLimitedTestError(usageState); limited {
				return "rate_limited", msg
			}
		}
		status, msg := "success", "测试通过"
		if !acc.IsClaudeOAuth() {
			status, msg = h.readBatchTestStreamResult(testCtx, acc, resp, testModel)
		}
		if status != "success" {
			return status, msg
		}
		if acc.IsClaudeOAuth() && claudeConnectionTestShouldPreserveUsageCooldown(acc, resp) {
			return "rate_limited", "Claude 上游返回了有效响应，但账号仍处于配额/限流状态"
		}
		// Successful tests reset failure/cooldown state; usage limits remain scheduler-controlled.
		h.store.RecordManualTestSuccess(acc, time.Since(start))
		return "success", msg
	case http.StatusUnauthorized:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		if acc.IsClaudeOAuth() {
			proxy.SyncClaudeUsageState(h.store, acc, resp)
		} else if !acc.IsRelayStyle() {
			proxy.SyncCodexUsageState(h.store, acc, resp)
		}
		h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300)))
		return "banned", "上游返回 401: 账号授权失败"
	case http.StatusTooManyRequests:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		// Grok free-usage-exhausted must go through Apply429Cooldown for its quota snapshot
		// and 24-hour usage_limited state, rather than the generic one-minute relay cooldown.
		if acc.IsClaudeOAuth() {
			if proxy.HandleClaudeModelBillingRejection(h.store, acc, testModel, resp.StatusCode, body) {
				return "rate_limited", fmt.Sprintf("上游模型 %s 需要 usage credits，当前账号套餐不可用", testModel)
			}
			proxy.SyncClaudeUsageState(h.store, acc, resp)
		} else if acc.IsAntigravityAPI() {
			proxy.ApplyAntigravityCooldown(h.store, acc, resp.StatusCode, body, resp, testModel)
		} else if acc.IsRelayStyle() && !acc.IsGrokAPI() {
			h.store.MarkCooldown(acc, time.Minute, "rate_limited")
		} else {
			if !acc.IsRelayStyle() {
				proxy.SyncCodexUsageState(h.store, acc, resp)
			}
			proxy.Apply429Cooldown(h.store, acc, body, resp, testModel)
		}
		return "rate_limited", "上游返回 429: 账号触发限流"
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		msg := fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
		if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
			h.store.MarkCooldownWithErrorExactDuration(acc, 24*time.Hour, "unauthorized", msg)
			return "banned", msg
		}
		// Cloud Code 503 indicates shared capacity exhaustion; apply a model cooldown.
		if acc.IsAntigravityAPI() && resp.StatusCode == http.StatusServiceUnavailable &&
			proxy.ApplyAntigravityCooldown(h.store, acc, resp.StatusCode, body, resp, testModel) {
			return "rate_limited", msg
		}
		if shouldMarkBatchTestAccountError(resp.StatusCode, body) {
			if proxy.IsDeactivatedWorkspaceError(body) {
				h.store.MarkDeactivatedWorkspace(acc, "批量测试"+msg)
			} else {
				h.store.MarkError(acc, "批量测试"+msg)
			}
		}
		return "failed", msg
	}
}

// runRecycleBinSingleTest tests a temporary account without modifying account state.
// It never calls MarkError, MarkCooldown, or RecordManualTestSuccess;
// results are for display only.
func (h *Handler) runRecycleBinSingleTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()
	if acc == nil {
		return "failed", "账号不存在"
	}

	if !acc.IsRelayStyle() && !acc.IsCodexAgentIdentity() && acc.GetAccessToken() == "" {
		return "failed", "账号缺少可用的 Access Token"
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		return "failed", modelErr.Error()
	}
	claudeSecurityCfg := h.store.ClaudeSecurityConfig()
	payload := h.buildAccountConnectionTestPayload(testCtx, acc, testModel, claudeSecurityCfg)

	var resp *http.Response
	var err error
	if acc.IsClaudeOAuth() {
		resp, err = proxy.ExecuteClaudeMessagesRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil, acc.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault()), claudeSecurityCfg)
	} else if acc.IsAntigravityAPI() {
		// Temporary recycle-bin accounts do not refresh credentials on 401.
		resp, err = h.executeAntigravityConnectionTest(testCtx, acc, testModel, payload, h.store.ResolveProxyForAccount(acc), false)
	} else if acc.IsRelayStyle() {
		resp, err = proxy.ExecuteRelayStyleRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil)
	} else {
		resp, err = proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	}
	if err != nil {
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			return "failed", msg
		}
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if acc.IsClaudeOAuth() {
			// Recycle-bin tests are intentionally read-only. Inspect the native
			// response headers without mutating the transient account snapshot.
			status, msg := readClaudeMessagesStream(testCtx, resp, nil)
			if status == "success" && claudeConnectionTestShouldPreserveUsageCooldown(acc, resp) {
				return "rate_limited", "Claude 上游返回了有效响应，但账号仍处于配额/限流状态"
			}
			return status, msg
		} else if !acc.IsRelayStyle() {
			// A nil store parses usage for display without persistence or cooldown updates.
			usageState := proxy.SyncCodexUsageState(nil, acc, resp)
			if msg, limited := formatUsageLimitedTestError(usageState); limited {
				return "rate_limited", msg
			}
		}
		return readRecycleBinTestStream(testCtx, resp)
	case http.StatusUnauthorized:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "banned", fmt.Sprintf("上游返回 401: %s", truncate(string(body), 300))
	case http.StatusTooManyRequests:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "rate_limited", fmt.Sprintf("上游返回 429: %s", truncate(string(body), 300))
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			if msg, ok := batchTestContextFailure(testCtx, readErr); ok {
				return "failed", msg
			}
			return "failed", readErr.Error()
		}
		return "failed", fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
}

// readRecycleBinTestStream evaluates SSE like readBatchTestStreamResult
// but does not update account state.
func readRecycleBinTestStream(ctx context.Context, resp *http.Response) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus = "failed"
				if proxy.IsUsageLimitReachedError(data) {
					resultStatus = "rate_limited"
				}
				resultMessage = formatUpstreamTestError(data, "上游返回 "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				return false
			}
			resultStatus = "success"
			resultMessage = "测试通过"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus = "failed"
			if proxy.IsUsageLimitReachedError(data) {
				resultStatus = "rate_limited"
			}
			resultMessage = formatUpstreamTestError(data, "上游返回 response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus = "failed"
			resultMessage = formatUpstreamTestError(data, "上游返回 error 事件")
			return false
		}
		return true
	})

	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return "failed", msg
		}
		return "failed", readErr.Error()
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		return "failed", formatMissingTerminalUpstreamError(lastUpstreamEvent)
	}
	return "failed", "上游测试未返回明确结果"
}

func (h *Handler) batchTestSkipDeactivatedWorkspace(acc *auth.Account) (string, string, bool) {
	if h == nil || h.store == nil || acc == nil {
		return "", "", false
	}
	msg, ok := h.store.LinkedDeactivatedWorkspaceResult(acc)
	if !ok {
		return "", "", false
	}
	if acc.RuntimeStatus() != "error" {
		h.store.MarkError(acc, msg)
	}
	return "failed", msg, true
}

func (h *Handler) batchTestWhamPreflight(ctx context.Context, acc *auth.Account) (string, string, bool) {
	if h == nil || h.store == nil || acc == nil || acc.IsRelayStyle() || acc.GetAccessToken() == "" {
		return "", "", false
	}

	whamCtx, cancel := context.WithTimeout(ctx, batchTestWhamTimeout)
	defer cancel()

	usage, resp, err := proxy.QueryWhamUsage(whamCtx, acc, h.store.ResolveProxyForAccount(acc))
	if resp != nil && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg := fmt.Sprintf("WHAM 用量探针返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
			h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", msg)
			return "banned", msg, true
		default:
			if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
				msg := fmt.Sprintf("WHAM 用量探针返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
				if proxy.IsDeactivatedWorkspaceError(body) {
					h.store.MarkDeactivatedWorkspace(acc, msg)
				} else {
					h.store.MarkError(acc, msg)
				}
				return "failed", msg, true
			}
		}
	}
	if err != nil || usage == nil {
		return "", "", false
	}

	usageState := proxy.ApplyWhamUsage(h.store, acc, usage)
	// WHAM lacks subscription expiry; supplement it from /subscriptions when needed (issue #360).
	proxy.MaybeSyncSubscriptionExpiry(ctx, h.store, acc, h.store.ResolveProxyForAccount(acc))
	applyUsageLimitedTestState(h.store, acc, usageState)
	if msg, limited := formatUsageLimitedTestError(usageState); limited {
		return "rate_limited", msg, true
	}
	return "", "", false
}

func (h *Handler) readBatchTestStreamResult(ctx context.Context, acc *auth.Account, resp *http.Response, model string) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				h.markBatchTestStreamFailure(acc, resultMessage)
				return false
			}
			resultStatus = "success"
			resultMessage = "测试通过"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 error 事件")
			return false
		}
		return true
	})

	if readErr != nil {
		return h.handleBatchTestReadError(ctx, acc, readErr)
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		msg := formatMissingTerminalUpstreamError(lastUpstreamEvent)
		h.markBatchTestStreamFailure(acc, msg)
		return "failed", msg
	}
	msg := "上游测试未返回明确结果"
	h.markBatchTestStreamFailure(acc, msg)
	return "failed", msg
}

func (h *Handler) batchTestTerminalFailure(acc *auth.Account, resp *http.Response, model string, payload []byte, fallback string) (string, string) {
	message := formatUpstreamTestError(payload, fallback)
	if h.applyResponsesUsageLimitFailure(acc, resp, model, payload) {
		return "rate_limited", message
	}
	h.markBatchTestStreamFailure(acc, message)
	return "failed", message
}

func (h *Handler) markBatchTestStreamFailure(acc *auth.Account, message string) {
	if h == nil || h.store == nil || acc == nil {
		return
	}
	switch acc.RuntimeStatus() {
	case "active", "ready", "refreshing", "error":
		h.store.MarkError(acc, "批量测试失败: "+message)
	}
}

func readBatchTestErrorBody(ctx context.Context, body io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(body, 64<<10))
}

func (h *Handler) handleBatchTestReadError(ctx context.Context, acc *auth.Account, err error) (string, string) {
	if msg, ok := batchTestContextFailure(ctx, err); ok {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
		}
		return "failed", msg
	}
	h.store.MarkError(acc, "批量测试读取响应失败: "+err.Error())
	return "failed", err.Error()
}

func batchTestContextFailure(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("测试超时: %s 内未完成", batchTestAccountTimeout), true
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "测试已取消", true
	}
	return "", false
}

func shouldMarkBatchTestAccountError(statusCode int, body []byte) bool {
	msg := strings.ToLower(string(body))
	if statusCode == http.StatusPaymentRequired {
		return true
	}
	if statusCode == http.StatusForbidden {
		return true
	}
	if statusCode == http.StatusBadRequest {
		for _, needle := range []string{
			"invalid_grant",
			"invalid_client",
			"unauthorized_client",
			"access_denied",
			"account_deactivated",
			"unsupported_country_region_territory",
		} {
			if strings.Contains(msg, needle) {
				return true
			}
		}
	}
	return false
}
