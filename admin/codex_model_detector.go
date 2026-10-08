package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	codexDetectorTimeout            = 10 * time.Minute
	codexDetectorMaxResponseBytes   = 512 * 1024
	codexDetectorMaxAnswerBytes     = 256 * 1024
	defaultCodexDetectorConcurrency = 1
	maxCodexDetectorConcurrency     = 3
	// Each valid sample may be retried once, matching the original 3/6 budget.
	codexDetectorAttemptsPerSample = 2
)

type codexDetectorEvent struct {
	Type           string                  `json:"type"`
	Index          int                     `json:"index,omitempty"`
	Total          int                     `json:"total,omitempty"`
	Attempt        int                     `json:"attempt,omitempty"`
	MaxAttempts    int                     `json:"max_attempts,omitempty"`
	Concurrency    int                     `json:"concurrency,omitempty"`
	Model          string                  `json:"model,omitempty"`
	Source         string                  `json:"source,omitempty"`
	SourceRevision string                  `json:"source_revision,omitempty"`
	ProbeID        string                  `json:"probe_id,omitempty"`
	Status         string                  `json:"status,omitempty"`
	ParsedNumbers  int                     `json:"parsed_numbers,omitempty"`
	MinimumNumbers int                     `json:"minimum_numbers,omitempty"`
	ElapsedMS      int64                   `json:"elapsed_ms,omitempty"`
	Error          string                  `json:"error,omitempty"`
	Report         *proxy.ModelTraceReport `json:"report,omitempty"`
}

// DetectCodexModel runs ModelTrace against one concrete account. Official
// Codex and Claude accounts use their native upstream lifecycle, while API
// accounts use their configured base URL, credentials and proxy.
//
// GET /api/admin/accounts/:id/model-detector?model=gpt-5.6-sol&samples=1
func (h *Handler) DetectCodexModel(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在或不在运行时池中"})
		return
	}
	if (account.IsRelayStyle() && !account.IsOpenAIResponsesAPI() && !account.IsClaudeOAuth()) || account.IsAntigravityAPI() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模型指纹检测仅支持 Codex、Claude 和 OpenAI Responses API 账号"})
		return
	}
	if account.IsOpenAIResponsesAPI() {
		if baseURL, apiKey := account.OpenAIResponsesCredentials(); baseURL == "" || apiKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "API 渠道账号缺少 Base URL 或 API Key"})
			return
		}
	} else if !account.IsCodexAgentIdentity() && account.GetAccessToken() == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号没有可用的 Access Token，请先刷新"})
		return
	}

	detector, err := h.currentModelTraceDetector(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ModelTrace 指纹库加载失败"})
		return
	}
	model, err := h.connectionTestModelForAccount(c.Request.Context(), account, strings.TrimSpace(c.Query("model")))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	concurrency, err := parseCodexDetectorConcurrency(c.Query("concurrency"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	samples, err := parseCodexDetectorSamples(c.Query("samples"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	maxAttempts := samples * codexDetectorAttemptsPerSample
	challenges, err := detector.GenerateChallenges(maxAttempts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ModelTrace 挑战生成失败"})
		return
	}

	setupSSE(c)
	if !sendSSEJSON(c, codexDetectorEvent{
		Type: "start", Total: samples, MaxAttempts: maxAttempts,
		Concurrency: concurrency, Model: model,
		Source: "ModelTrace", SourceRevision: detector.Revision(),
	}) {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), codexDetectorTimeout)
	defer cancel()
	ctx = proxy.WithCodexDetectorRequest(ctx)
	outputs := make([]proxy.ModelTraceOutput, 0, maxAttempts)
	validOutputs := 0
	nextChallenge := 0
	for nextChallenge < len(challenges) && validOutputs < samples && ctx.Err() == nil {
		needed := samples - validOutputs
		batchSize := min(concurrency, needed, len(challenges)-nextChallenge)
		results := make(chan codexDetectorProbeResult, batchSize)
		for _, challenge := range challenges[nextChallenge : nextChallenge+batchSize] {
			challenge := challenge
			go func() {
				started := time.Now()
				answer, probeErr := h.executeModelDetectorProbe(ctx, account, model, challenge)
				results <- codexDetectorProbeResult{
					challenge: challenge, answer: answer, elapsedMS: time.Since(started).Milliseconds(), err: probeErr,
				}
			}()
		}
		nextChallenge += batchSize
		for range batchSize {
			result := <-results
			outputs = append(outputs, proxy.ModelTraceOutput{Text: result.answer, ExpectedCount: result.challenge.ExpectedCount})
			parsedNumbers := len(proxy.ParseModelTraceNumbers(result.answer))
			minimumNumbers := proxy.ModelTraceMinimumNumbers(result.challenge.ExpectedCount)
			status := "ok"
			if result.err != nil {
				status = "error"
			} else if parsedNumbers < minimumNumbers {
				status = "invalid"
			} else {
				validOutputs++
			}
			event := codexDetectorEvent{
				Type: "progress", Index: validOutputs, Total: samples,
				Attempt: len(outputs), MaxAttempts: maxAttempts, Concurrency: concurrency,
				ProbeID: result.challenge.ID, Status: status, ParsedNumbers: parsedNumbers,
				MinimumNumbers: minimumNumbers, ElapsedMS: result.elapsedMS,
			}
			if result.err != nil {
				event.Error = sanitizeDetectorError(result.err.Error())
			}
			if !sendSSEJSON(c, event) {
				return
			}
		}
	}
	if c.Request.Context().Err() != nil {
		return
	}
	report, err := detector.Analyze(outputs)
	if err != nil {
		sendSSEJSON(c, codexDetectorEvent{
			Type: "complete", Index: validOutputs, Total: samples,
			Attempt: len(outputs), MaxAttempts: maxAttempts, Concurrency: concurrency, Model: model,
			Source: "ModelTrace", SourceRevision: detector.Revision(),
			Status: "error", Error: sanitizeDetectorError(err.Error()),
		})
		return
	}
	sendSSEJSON(c, codexDetectorEvent{
		Type: "complete", Index: validOutputs, Total: samples,
		Attempt: len(outputs), MaxAttempts: maxAttempts, Concurrency: concurrency, Model: model,
		Source: "ModelTrace", SourceRevision: detector.Revision(), Status: "ok", Report: &report,
	})
}

type codexDetectorProbeResult struct {
	challenge proxy.ModelTraceChallenge
	answer    string
	elapsedMS int64
	err       error
}

// parseCodexDetectorSamples reads how many valid samples to collect. The bank
// ships calibration for 1..ModelTraceTargetOutputs pooled samples, so fewer
// samples trade accuracy for cost instead of using an uncalibrated score.
func parseCodexDetectorSamples(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return proxy.ModelTraceTargetOutputs, nil
	}
	samples, err := strconv.Atoi(raw)
	if err != nil || samples < 1 || samples > proxy.ModelTraceTargetOutputs {
		return 0, fmt.Errorf("检测次数必须是 1 到 %d 的整数", proxy.ModelTraceTargetOutputs)
	}
	return samples, nil
}

func parseCodexDetectorConcurrency(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCodexDetectorConcurrency, nil
	}
	concurrency, err := strconv.Atoi(raw)
	if err != nil || concurrency < 1 || concurrency > maxCodexDetectorConcurrency {
		return 0, fmt.Errorf("并发数必须是 1 到 %d 的整数", maxCodexDetectorConcurrency)
	}
	return concurrency, nil
}

func (h *Handler) executeModelDetectorProbe(ctx context.Context, account *auth.Account, model string, challenge proxy.ModelTraceChallenge) (string, error) {
	if account != nil && account.IsClaudeOAuth() {
		return h.executeClaudeDetectorProbe(ctx, account, model, challenge)
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"input": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": challenge.Prompt}},
			},
		},
		"stream": true,
		"store":  false,
	})
	if err != nil {
		return "", err
	}
	var response *http.Response
	var requestErr error
	proxyURL := h.store.ResolveProxyForAccount(account)
	if account.IsOpenAIResponsesAPI() {
		response, requestErr = proxy.ExecuteOpenAIResponsesRequest(ctx, account, payload, proxyURL, nil)
	} else {
		// Repeated probes stay on HTTP because pooled WebSockets may be closed by
		// an intermediary before response.completed arrives. Each challenge still
		// gets a native Codex session identity so the HTTP request carries the same
		// session/thread/request-id and prompt-cache shape as a real client without
		// linking independent fingerprint challenges into one conversation.
		response, requestErr = proxy.ExecuteRequest(ctx, account, payload, proxy.NewUpstreamSessionUUID(), proxyURL, "", nil, nil, false)
	}
	if requestErr != nil {
		return "", requestErr
	}
	if response == nil {
		return "", fmt.Errorf("上游未返回响应")
	}
	if response.Body == nil {
		return "", fmt.Errorf("上游响应没有正文")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		detail := sanitizeDetectorError(string(body))
		if detail == "" {
			return "", fmt.Errorf("上游返回 HTTP %d", response.StatusCode)
		}
		return "", fmt.Errorf("上游返回 HTTP %d: %s", response.StatusCode, detail)
	}

	var answer strings.Builder
	terminal := false
	failed := false
	overflow := false
	readErr := proxy.ReadSSEStream(io.LimitReader(response.Body, codexDetectorMaxResponseBytes+1), func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		appendText := func(text string) bool {
			if len(text) == 0 {
				return true
			}
			if answer.Len()+len(text) > codexDetectorMaxAnswerBytes {
				overflow = true
				return false
			}
			answer.WriteString(text)
			return true
		}
		switch eventType {
		case "response.output_text.delta":
			return appendText(gjson.GetBytes(data, "delta").String())
		case "response.output_text.done":
			if answer.Len() == 0 {
				return appendText(gjson.GetBytes(data, "text").String())
			}
		case "response.content_part.done":
			if answer.Len() == 0 {
				return appendText(gjson.GetBytes(data, "part.text").String())
			}
		case "response.output_item.done":
			if answer.Len() == 0 {
				return appendText(extractOutputItemText(gjson.GetBytes(data, "item")))
			}
		case "response.completed":
			terminal = true
			if answer.Len() == 0 {
				return appendText(extractCompletedOutputText(data))
			}
			return false
		case "response.failed", "response.incomplete", "error":
			terminal = true
			failed = true
			return false
		}
		return true
	})
	if readErr != nil {
		return "", readErr
	}
	if overflow {
		return "", fmt.Errorf("上游响应文本超过限制")
	}
	if failed {
		return "", fmt.Errorf("上游响应未完成")
	}
	if !terminal || strings.TrimSpace(answer.String()) == "" {
		return "", fmt.Errorf("上游未返回有效文本")
	}
	return answer.String(), nil
}

func (h *Handler) executeClaudeDetectorProbe(ctx context.Context, account *auth.Account, model string, challenge proxy.ModelTraceChallenge) (string, error) {
	securityConfig := auth.DefaultClaudeSecurityConfig()
	fingerprintMode := ""
	proxyURL := ""
	if h != nil && h.store != nil {
		securityConfig = h.store.ClaudeSecurityConfig()
		fingerprintMode = account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault())
		proxyURL = h.store.ResolveProxyForAccount(account)
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": claudeProbeTokenBudget(securityConfig),
		"stream":     true,
		"messages": []any{map[string]any{
			"role": "user", "content": challenge.Prompt,
		}},
	})
	if err != nil {
		return "", err
	}
	response, err := proxy.ExecuteClaudeMessagesRequest(ctx, account, payload, proxyURL, nil, fingerprintMode, securityConfig)
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", fmt.Errorf("上游未返回响应")
	}
	if response.Body == nil {
		return "", fmt.Errorf("上游响应没有正文")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		detail := sanitizeDetectorError(string(body))
		if detail == "" {
			return "", fmt.Errorf("上游返回 HTTP %d", response.StatusCode)
		}
		return "", fmt.Errorf("上游返回 HTTP %d: %s", response.StatusCode, detail)
	}
	return readClaudeDetectorAnswer(ctx, response)
}

func readClaudeDetectorAnswer(ctx context.Context, response *http.Response) (string, error) {
	limited := &io.LimitedReader{R: response.Body, N: codexDetectorMaxResponseBytes + 1}
	view := *response
	view.Body = io.NopCloser(limited)
	var answer strings.Builder
	overflow := false
	status, detail := readClaudeMessagesStreamObserved(ctx, &view, func(text string) {
		if overflow || text == "" {
			return
		}
		if answer.Len()+len(text) > codexDetectorMaxAnswerBytes {
			overflow = true
			return
		}
		answer.WriteString(text)
	}, nil, true)
	if limited.N <= 0 || overflow {
		return "", fmt.Errorf("上游响应文本超过限制")
	}
	if status != "success" {
		return "", fmt.Errorf("上游响应未完成: %s", sanitizeDetectorError(detail))
	}
	if strings.TrimSpace(answer.String()) == "" {
		return "", fmt.Errorf("上游未返回有效文本")
	}
	return answer.String(), nil
}

func sanitizeDetectorError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 300 {
		message = message[:300] + "..."
	}
	return message
}
