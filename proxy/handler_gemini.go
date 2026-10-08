package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var geminiNativeGenerationMethods = []string{
	"generateContent",
	"streamGenerateContent",
	"countTokens",
}

// GeminiListModels handles GET /v1beta/models for native Gemini clients.
func (h *Handler) GeminiListModels(c *gin.Context) {
	if h.enforceAPIKeyLimitsAndReply(c, "") {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
		return
	}
	models := geminiNativeModelEntries(h.scopedModels(ctx, row))
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// GeminiGetModel handles GET /v1beta/models/{model} for native Gemini clients.
func (h *Handler) GeminiGetModel(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	if action == "" || strings.Contains(action, ":") {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, normalizeGeminiPublicModel(action)) {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	modelID := normalizeGeminiPublicModel(action)
	for _, entry := range geminiNativeModelEntries(h.scopedModels(ctx, row)) {
		name, _ := entry["name"].(string)
		if normalizeGeminiPublicModel(name) == modelID {
			c.JSON(http.StatusOK, entry)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": gin.H{
			"code":    http.StatusNotFound,
			"message": "Not Found",
			"status":  "NOT_FOUND",
		},
	})
}

func geminiNativeModelEntries(models []api.Model) []map[string]any {
	entries := make([]map[string]any, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		id := normalizeGeminiPublicModel(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, geminiNativeModelEntry(id))
	}
	return entries
}

func geminiNativeModelEntry(id string) map[string]any {
	id = normalizeGeminiPublicModel(id)
	return map[string]any{
		"name":                       "models/" + id,
		"displayName":                id,
		"description":                id,
		"supportedGenerationMethods": append([]string(nil), geminiNativeGenerationMethods...),
	}
}

// GeminiModelsAction handles native Gemini API POST /v1beta/models/*action requests
// and routes Antigravity OAuth accounts through the Cloud Code v1internal adapter.
func (h *Handler) GeminiModelsAction(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": parts[0] + " not found.",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	model := normalizeGeminiPublicModel(parts[0])
	method := parts[1]
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(security.MaxRequestBodySize+1)))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	if len(rawBody) > security.MaxRequestBodySize {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request body too large", api.ErrorTypeInvalidRequest))
		return
	}
	if !gjson.ValidBytes(rawBody) {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid JSON in request body", api.ErrorTypeInvalidRequest))
		return
	}

	switch method {
	case "generateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, false)
	case "streamGenerateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, true)
	case "countTokens":
		h.handleGeminiCountTokens(c, model, rawBody)
	default:
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": method + " not found.",
				"status":  "NOT_FOUND",
			},
		})
	}
}

func geminiCountTokensInboundEndpoint() string {
	return "/v1beta/models:countTokens"
}

func (h *Handler) handleGeminiCountTokens(c *gin.Context, model string, rawBody []byte) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withModelCooldownFilter(c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	inboundEndpoint := geminiCountTokensInboundEndpoint()
	upstreamEndpoint := "/v1internal:countTokens"

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy,
		)
		if account == nil {
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolOpenAI) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
			h.store.Release(account)
			return
		}

		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		resp, reqErr := ExecuteAntigravityGeminiCountTokensRequest(upstreamCtx, account, model, rawBody, proxyURL)
		upstreamCancel()
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestError(reqErr)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusNotFound {
				h.store.Release(account)
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				if refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account); refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			log.Printf("Gemini native countTokens upstream error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			if shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					continue
				}
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		out, readErr := io.ReadAll(io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		h.store.Release(account)
		if readErr != nil {
			ErrorToGinResponse(c, readErr)
			return
		}
		c.Data(http.StatusOK, "application/json", out)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       durationMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}

func geminiInboundEndpoint(stream bool) string {
	if stream {
		return "/v1beta/models:streamGenerateContent"
	}
	return "/v1beta/models:generateContent"
}

type geminiNativeStreamResult struct {
	usage        UsageInfo
	firstTokenMs int
	readErr      error
	writeErr     error
}

// forwardGeminiNativeStream keeps reads and downstream writes distinguishable
// for usage logging. Emit complete SSE events and flush each one so a thought
// does not sit in the HTTP buffer while the upstream prepares its answer.
func forwardGeminiNativeStream(c *gin.Context, body io.Reader, startedAt time.Time) geminiNativeStreamResult {
	var result geminiNativeStreamResult
	var usage geminiNativeUsage
	activateContinuousRetryKeepalive(c.Request.Context())
	result.readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), body, func(_ string, data []byte) bool {
		if c.Request.Context().Err() != nil {
			return false
		}
		usage.observe(data)
		if result.firstTokenMs == 0 && !startedAt.IsZero() && geminiNativeHasContent(data) {
			result.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
		}
		frame := "data: " + string(data) + "\n\n"
		var written int
		written, result.writeErr = c.Writer.WriteString(frame)
		if result.writeErr == nil && written != len(frame) {
			result.writeErr = io.ErrShortWrite
		}
		if result.writeErr != nil {
			return false
		}
		c.Writer.Flush()
		return true
	})
	result.usage = usage.usageInfo()
	return result
}

// writeGeminiNativeError uses the same native error envelope before and after
// SSE headers are committed. Once streaming has started, the HTTP status cannot
// change. Google SDKs expect a bare JSON error at the end of the stream; an SSE
// data error is silently discarded by the SDK bundled with Gemini CLI 0.61.0.
func writeGeminiNativeError(c *gin.Context, status int, message string) {
	statusName := "INTERNAL"
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		statusName = "UNAVAILABLE"
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		statusName = "DEADLINE_EXCEEDED"
	case http.StatusTooManyRequests:
		statusName = "RESOURCE_EXHAUSTED"
	case http.StatusBadRequest:
		statusName = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		statusName = "UNAUTHENTICATED"
	case http.StatusForbidden:
		statusName = "PERMISSION_DENIED"
	case http.StatusNotFound:
		statusName = "NOT_FOUND"
	}
	payload := gin.H{"error": gin.H{"code": status, "message": message, "status": statusName}}
	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(status, payload)
		return
	}
	if !strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		return
	}
	data, _ := json.Marshal(payload)
	if _, err := c.Writer.Write(data); err == nil {
		c.Writer.Flush()
	}
}

func (h *Handler) handleGeminiGenerateContent(c *gin.Context, model string, rawBody []byte, stream bool) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withModelCooldownFilter(c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolGemini)
	defer stopRetryDeadline()
	// Gemini CLI's bundled Google SDK expects data events and cannot skip SSE
	// comments. An empty response keeps the connection alive without content.
	stopRetryKeepalive := installContinuousRetrySSEKeepaliveWithOptions(c, stream, continuousRetrySSEKeepaliveOptions{
		contentType: "text/event-stream; charset=utf-8",
		payload:     "data: {}\n\n",
	})
	defer stopRetryKeepalive()
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		activateContinuousRetryKeepalive(c.Request.Context())
	}

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	inboundEndpoint := geminiInboundEndpoint(stream)
	upstreamEndpoint := antigravityUpstreamEndpoint(stream)

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy,
		)
		if account == nil {
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolGemini) {
				return
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				setConcurrencySaturatedRetryAfter(c)
				c.JSON(http.StatusServiceUnavailable, concurrencySaturatedError())
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(model))
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return
			}
		}

		// Native Gemini forwards the stream directly and does not drain usage
		// after a disconnect. Keep this context alive through body consumption,
		// while letting a canceled client stop a blocked upstream read immediately.
		upstreamCtx, upstreamCancel := context.WithCancel(c.Request.Context())
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteAntigravityGeminiRequest(upstreamCtx, account, model, rawBody, stream, proxyURL)
		})
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			upstreamCancel()
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				if claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) && c.Request.Context().Err() == nil {
					writeGeminiNativeError(c, StatusCodeFromError(reqErr), "Upstream Gemini request failed")
				}
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			_ = resp.Body.Close()
			upstreamCancel()
			if continuousRetryCommitExpired(c, continuousRetryProtocolGemini) {
				h.store.Release(account)
				return
			}
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account)
				if refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					log.Printf("Antigravity OAuth token refreshed after upstream 401 (account=%d, endpoint=%s)", account.ID(), inboundEndpoint)
					continue
				}
				log.Printf("Antigravity OAuth refresh failed after upstream 401 (account=%d, endpoint=%s): %v", account.ID(), inboundEndpoint, refreshErr)
			}

			antigravityRefreshFailed := resp.StatusCode == http.StatusUnauthorized && antigravityRefreshRetried[account.ID()]
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" && !antigravityRefreshFailed && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("Gemini native upstream error (attempt %d, status %d, %s): %s", attempt+1, resp.StatusCode, inboundEndpoint, upstreamErrorConsoleBody(errBody))
			logUpstreamError(inboundEndpoint, resp.StatusCode, model, account.ID(), errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, model)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            stream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) {
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		if stream {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			c.Status(resp.StatusCode)
			result := forwardGeminiNativeStream(c, resp.Body, start)
			_ = resp.Body.Close()
			upstreamCancel()
			h.store.Release(account)
			durationMs = int(time.Since(start).Milliseconds())
			ctxErr := continuousRetryContextError(c.Request.Context())
			// The native reader only returns a clean EOF after a real Gemini
			// terminal. A successful upstream read still requires delivery to the
			// downstream client before this request can be recorded as successful.
			completed := result.readErr == nil && result.writeErr == nil && ctxErr == nil
			outcome := classifyStreamOutcome(ctxErr, result.readErr, result.writeErr, completed)
			var upstreamStreamErr *antigravityStreamError
			if ctxErr == nil && result.writeErr == nil && errors.As(result.readErr, &upstreamStreamErr) {
				outcome = h.antigravityStreamErrorOutcome(account, upstreamStreamErr, resp, model)
			}
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
			}
			if outcome.penalize {
				h.reportStreamOutcomeFailure(account, outcome, time.Since(start))
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if outcome.logStatusCode != http.StatusOK {
				if !writeContinuousRetryTimeoutResponse(c, continuousRetryProtocolGemini) && ctxErr == nil && result.writeErr == nil {
					status, message := http.StatusBadGateway, "Upstream Gemini stream failed before completion"
					if upstreamStreamErr != nil {
						// Gemini clients retry RESOURCE_EXHAUSTED/UNAVAILABLE themselves,
						// so keep the upstream status instead of a generic 502.
						status = upstreamStreamErr.StatusCode
						if text := strings.TrimSpace(gjson.GetBytes(upstreamStreamErr.Body, "error.message").String()); text != "" {
							message = text
						}
					}
					writeGeminiNativeError(c, status, message)
				}
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        outcome.logStatusCode,
				DurationMs:        durationMs,
				FirstTokenMs:      result.firstTokenMs,
				PromptTokens:      result.usage.PromptTokens,
				CompletionTokens:  result.usage.CompletionTokens,
				InputTokens:       result.usage.InputTokens,
				OutputTokens:      result.usage.OutputTokens,
				ReasoningTokens:   result.usage.ReasoningTokens,
				CachedTokens:      result.usage.CachedTokens,
				TotalTokens:       result.usage.TotalTokens,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            true,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: outcome.failureKind,
				ErrorMessage:      usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			})
			return
		}

		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		upstreamCancel()
		if readErr != nil {
			h.store.ReportRequestFailure(account, "upstream_read_error", time.Duration(durationMs)*time.Millisecond)
			h.store.Release(account)
			if claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) && c.Request.Context().Err() == nil {
				writeGeminiNativeError(c, http.StatusBadGateway, "Failed to read upstream Gemini response")
			}
			return
		}
		usage := geminiNativeUsageFromBody(out)
		if !claimContinuousRetrySuccess(c, continuousRetryProtocolGemini) {
			h.store.Release(account)
			return
		}
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Status(resp.StatusCode)
		_, _ = c.Writer.Write(out)
		h.store.Release(account)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       int(time.Since(start).Milliseconds()),
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			InputTokens:      usage.InputTokens,
			OutputTokens:     usage.OutputTokens,
			ReasoningTokens:  usage.ReasoningTokens,
			CachedTokens:     usage.CachedTokens,
			TotalTokens:      usage.TotalTokens,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			Stream:           false,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}
