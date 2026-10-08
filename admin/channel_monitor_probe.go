package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	channelMonitorProbeTimeout       = 75 * time.Second
	channelMonitorHealthTimeout      = 60 * time.Second
	channelMonitorBillingTimeout     = 10 * time.Second
	channelMonitorBillingInterval    = 30 * time.Minute
	channelMonitorUnsupportedBilling = 4 * time.Hour
	channelMonitorLeaseTTL           = 90 * time.Second
	channelMonitorDegradedThreshold  = 6 * time.Second
	channelMonitorMaxResponseBytes   = 1024 * 1024
	channelMonitorMaxBillingBytes    = 64 * 1024
	channelMonitorDueBatchSize       = 20
)

var errChannelMonitorBusy = errors.New("该渠道正在探测，请稍后再试")

func (h *Handler) signalChannelMonitor() {
	if h == nil || h.channelMonitorWake == nil {
		return
	}
	select {
	case h.channelMonitorWake <- struct{}{}:
	default:
	}
}

// StartChannelMonitor starts a bounded, due-time driven runner. An account
// lease is acquired for every probe, so multiple application replicas sharing
// Redis do not duplicate billable health calls.
func (h *Handler) StartChannelMonitor(ctx context.Context) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		cleanupTicker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		defer cleanupTicker.Stop()
		h.runDueChannelMonitors(ctx)
		h.cleanupChannelMonitorHistory(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runDueChannelMonitors(ctx)
			case <-h.channelMonitorWake:
				h.runDueChannelMonitors(ctx)
			case <-cleanupTicker.C:
				h.cleanupChannelMonitorHistory(ctx)
			}
		}
	}()
}

func (h *Handler) cleanupChannelMonitorHistory(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	before := time.Now().UTC().AddDate(0, 0, -database.ChannelMonitorHistoryRetentionDays)
	deleted, err := h.db.DeleteChannelMonitorChecksBefore(cleanupCtx, before)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("渠道监控历史清理失败: %v", err)
	} else if deleted > 0 {
		log.Printf("渠道监控历史已清理: %d 条", deleted)
	}
}

func (h *Handler) runDueChannelMonitors(ctx context.Context) {
	cycleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	configs, err := h.db.ListDueChannelMonitors(cycleCtx, time.Now(), channelMonitorDueBatchSize)
	cancel()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("读取待探测渠道失败: %v", err)
		}
		return
	}
	for i := range configs {
		accountID := configs[i].AccountID
		go func() {
			probeCtx, probeCancel := context.WithTimeout(ctx, channelMonitorProbeTimeout)
			defer probeCancel()
			if err := h.runChannelMonitorAccount(probeCtx, accountID, false); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, errChannelMonitorBusy) {
				log.Printf("渠道监控探测失败 (account %d): %v", accountID, err)
			}
		}()
	}
}

func (h *Handler) runChannelMonitorAccount(ctx context.Context, accountID int64, force bool) error {
	if h == nil || h.channelMonitorSlots == nil {
		return errors.New("渠道监控服务不可用")
	}
	if _, running := h.channelMonitorRunning.LoadOrStore(accountID, struct{}{}); running {
		return errChannelMonitorBusy
	}
	defer h.channelMonitorRunning.Delete(accountID)

	select {
	case h.channelMonitorSlots <- struct{}{}:
		defer func() { <-h.channelMonitorSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}

	leaseOwner := fmt.Sprintf("%s-%d", h.channelMonitorID, time.Now().UnixNano())
	if h.cache != nil {
		leaseCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		acquired, err := h.cache.AcquireLease(leaseCtx, "channel-monitor", strconv.FormatInt(accountID, 10), leaseOwner, channelMonitorLeaseTTL)
		cancel()
		if err != nil {
			return fmt.Errorf("获取渠道监控防重锁失败: %w", err)
		}
		if !acquired {
			return errChannelMonitorBusy
		}
		defer func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = h.cache.ReleaseLease(releaseCtx, "channel-monitor", strconv.FormatInt(accountID, 10), leaseOwner)
		}()
	}

	config, err := h.db.GetChannelMonitorConfig(ctx, accountID)
	if err != nil {
		return err
	}
	if !config.Enabled {
		return errors.New("该渠道尚未启用监控")
	}
	now := time.Now().UTC()
	healthDue := force || !config.NextCheckAt.Valid || !now.Before(config.NextCheckAt.Time)
	billingDue := force || !config.BillingNextCheckAt.Valid || !now.Before(config.BillingNextCheckAt.Time)
	if !healthDue && !billingDue {
		return nil
	}
	account, _, err := h.loadChannelMonitorAccount(ctx, accountID)
	if err != nil {
		return err
	}

	if healthDue {
		result := h.probeResponsesChannel(ctx, account, config.Model)
		result.AccountID = accountID
		result.NextCheckAt = nextChannelMonitorHealthAt(result.CheckedAt, config.IntervalMinutes, accountID)
		if err := h.db.RecordChannelMonitorHealth(ctx, result); err != nil {
			return fmt.Errorf("保存渠道健康探测结果失败: %w", err)
		}
	}
	if billingDue {
		result := h.probeResponsesBilling(ctx, account, config.BillingFailureCount)
		result.AccountID = accountID
		if err := h.db.RecordChannelMonitorBilling(ctx, result); err != nil {
			return fmt.Errorf("保存渠道倍率探测结果失败: %w", err)
		}
	}
	return nil
}

func nextChannelMonitorHealthAt(checkedAt time.Time, intervalMinutes int, accountID int64) time.Time {
	interval := time.Duration(database.NormalizeChannelMonitorInterval(intervalMinutes)) * time.Minute
	// Add up to 10% deterministic jitter to avoid all cards firing together
	// after a restart or bulk enable.
	window := interval / 10
	if window <= 0 {
		return checkedAt.Add(interval)
	}
	seed := checkedAt.UnixNano() ^ accountID
	if seed < 0 {
		seed = -seed
	}
	return checkedAt.Add(interval + time.Duration(seed%int64(window)))
}

func buildChannelMonitorPayload(model string, _ int64) ([]byte, string) {
	const expected = "go"
	const prompt = "Complete: ready, set, ___"
	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "model", strings.TrimSpace(model))
	payload, _ = sjson.SetBytes(payload, "instructions", "Return only the missing word in lowercase, with no punctuation.")
	payload, _ = sjson.SetBytes(payload, "input", []map[string]any{{
		"role":    "user",
		"content": []map[string]any{{"type": "input_text", "text": prompt}},
	}})
	payload, _ = sjson.SetBytes(payload, "max_output_tokens", 64)
	payload, _ = sjson.SetBytes(payload, "stream", false)
	payload, _ = sjson.SetBytes(payload, "store", false)
	return payload, expected
}

type channelMonitorResponseRead struct {
	Text         string
	FirstTokenMS int64
	Terminal     bool
	Failure      string
}

func (h *Handler) probeResponsesChannel(ctx context.Context, account *auth.Account, model string) database.ChannelMonitorHealthResult {
	checkedAt := time.Now().UTC()
	result := database.ChannelMonitorHealthResult{
		Status:    database.ChannelMonitorStatusFailed,
		Model:     strings.TrimSpace(model),
		CheckedAt: checkedAt,
	}
	if !isTextConnectionModel(result.Model) {
		result.Message = "未配置可用的文本探测模型"
		return result
	}
	payload, expected := buildChannelMonitorPayload(result.Model, account.ID())
	requestCtx, cancel := context.WithTimeout(ctx, channelMonitorHealthTimeout)
	defer cancel()
	start := time.Now()
	resp, err := proxy.ExecuteOpenAIResponsesRequest(requestCtx, account, payload, h.store.ResolveProxyForAccount(account), nil)
	if err != nil {
		result.LatencyMS = time.Since(start).Milliseconds()
		result.Message = "请求失败: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		result.LatencyMS = time.Since(start).Milliseconds()
		result.Message = fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 500))
		return result
	}

	read := channelMonitorResponseRead{}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		read = readChannelMonitorSSE(resp.Body, start)
	} else {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, channelMonitorMaxResponseBytes+1))
		if readErr != nil {
			read.Failure = "读取上游响应失败: " + readErr.Error()
		} else if len(body) > channelMonitorMaxResponseBytes {
			read.Failure = "上游响应过大"
		} else {
			read = readChannelMonitorJSON(body)
		}
	}
	result.LatencyMS = time.Since(start).Milliseconds()
	result.FirstTokenMS = read.FirstTokenMS
	if read.Failure != "" {
		result.Message = read.Failure
		return result
	}
	if !read.Terminal {
		result.Message = "上游响应缺少完成事件"
		return result
	}
	if !channelMonitorAnswerMatches(read.Text, expected) {
		result.Message = "上游返回成功，但响应内容校验失败"
		return result
	}
	result.Status = database.ChannelMonitorStatusOperational
	result.Message = "探测成功"
	if time.Duration(result.LatencyMS)*time.Millisecond >= channelMonitorDegradedThreshold {
		result.Status = database.ChannelMonitorStatusDegraded
		result.Message = "探测成功，但响应较慢"
	}
	return result
}

func readChannelMonitorSSE(body io.Reader, start time.Time) channelMonitorResponseRead {
	var result channelMonitorResponseRead
	var text strings.Builder
	readErr := proxy.ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		appendText := func(value string) {
			if value == "" {
				return
			}
			if result.FirstTokenMS == 0 {
				result.FirstTokenMS = time.Since(start).Milliseconds()
				if result.FirstTokenMS == 0 {
					result.FirstTokenMS = 1
				}
			}
			if text.Len() < 4096 {
				remaining := 4096 - text.Len()
				if len(value) > remaining {
					value = value[:remaining]
				}
				text.WriteString(value)
			}
		}
		switch eventType {
		case "response.output_text.delta":
			appendText(gjson.GetBytes(data, "delta").String())
		case "response.output_text.done":
			if text.Len() == 0 {
				appendText(gjson.GetBytes(data, "text").String())
			}
		case "response.content_part.done":
			if text.Len() == 0 {
				appendText(gjson.GetBytes(data, "part.text").String())
			}
		case "response.output_item.done":
			if text.Len() == 0 {
				appendText(extractOutputItemText(gjson.GetBytes(data, "item")))
			}
		case "response.completed":
			status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
			if status == "failed" || status == "incomplete" || status == "cancelled" {
				result.Failure = formatUpstreamTestError(data, "上游返回 "+status)
				return false
			}
			if text.Len() == 0 {
				appendText(extractCompletedOutputText(data))
			}
			result.Terminal = true
			return false
		case "response.failed", "error":
			result.Failure = formatUpstreamTestError(data, "上游返回 "+eventType)
			return false
		}
		return true
	})
	if readErr != nil && result.Failure == "" {
		result.Failure = "读取上游流失败: " + readErr.Error()
	}
	result.Text = text.String()
	return result
}

func readChannelMonitorJSON(body []byte) channelMonitorResponseRead {
	result := channelMonitorResponseRead{}
	if !gjson.ValidBytes(body) {
		result.Failure = "上游返回了无效 JSON"
		return result
	}
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
	if status == "failed" || status == "incomplete" || status == "cancelled" {
		result.Failure = formatUpstreamTestError(body, "上游返回 "+status)
		return result
	}
	result.Text = gjson.GetBytes(body, "output_text").String()
	if result.Text == "" {
		result.Text = extractOutputItemText(gjson.ParseBytes(body))
	}
	result.Terminal = status == "" || status == "completed"
	return result
}

func channelMonitorAnswerMatches(text, expected string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(text)), "`'\".,!?;: ")
	return normalized == strings.ToLower(strings.TrimSpace(expected))
}

type channelMonitorBillingWire struct {
	Object                  string   `json:"object"`
	SchemaVersion           int      `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64 `json:"resolved_rate_multiplier"`
	PeakRateEnabled         *bool    `json:"peak_rate_enabled"`
	PeakStart               *string  `json:"peak_start"`
	PeakEnd                 *string  `json:"peak_end"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	Timezone                *string  `json:"timezone"`
	ObservedAt              string   `json:"observed_at"`
}

func (h *Handler) probeResponsesBilling(ctx context.Context, account *auth.Account, previousFailureCount int) database.ChannelMonitorBillingResult {
	checkedAt := time.Now().UTC()
	result := database.ChannelMonitorBillingResult{
		Status:       database.ChannelMonitorBillingStatusFailed,
		CheckedAt:    checkedAt,
		PreserveData: true,
		FailureCount: previousFailureCount + 1,
	}
	requestCtx, cancel := context.WithTimeout(ctx, channelMonitorBillingTimeout)
	defer cancel()
	resp, err := proxy.ExecuteOpenAIResponsesBillingRequest(requestCtx, account, h.store.ResolveProxyForAccount(account))
	if err != nil {
		result.Message = "倍率请求失败: " + err.Error()
		result.NextCheckAt = nextChannelMonitorBillingAt(result.Status, checkedAt, result.FailureCount, 0, account.ID())
		return result
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, channelMonitorMaxBillingBytes+1))
	if readErr != nil {
		result.Message = "读取倍率响应失败: " + readErr.Error()
	} else if len(body) > channelMonitorMaxBillingBytes {
		result.Message = "倍率响应过大"
	} else if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		result.Status = database.ChannelMonitorBillingStatusUnsupported
		result.Message = "上游不支持倍率探测"
		result.FailureCount = 0
	} else if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.Message = fmt.Sprintf("倍率接口返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
	} else {
		data, parseErr := parseChannelMonitorBilling(body, checkedAt)
		if parseErr != nil {
			result.Message = "倍率响应无效: " + parseErr.Error()
		} else {
			encoded, marshalErr := json.Marshal(data)
			if marshalErr != nil {
				result.Message = "倍率响应保存失败"
			} else {
				result.Status = database.ChannelMonitorBillingStatusOK
				result.Data = string(encoded)
				result.Message = "倍率探测成功"
				result.PreserveData = false
				result.FailureCount = 0
			}
		}
	}
	retryAfter := channelMonitorRetryAfter(resp.Header, checkedAt)
	result.NextCheckAt = nextChannelMonitorBillingAt(result.Status, checkedAt, result.FailureCount, retryAfter, account.ID())
	return result
}

func parseChannelMonitorBilling(body []byte, now time.Time) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var wire channelMonitorBillingWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, err
	}
	if wire.Object != "" && wire.Object != "sub2api.key_billing" {
		return nil, fmt.Errorf("未知的倍率对象")
	}
	if wire.SchemaVersion != 0 && wire.SchemaVersion != 1 {
		return nil, fmt.Errorf("不支持的倍率协议版本")
	}
	if wire.BillingScope != "" && wire.BillingScope != "token" {
		return nil, fmt.Errorf("仅支持 token 倍率")
	}
	if wire.ResolvedRateMultiplier == nil {
		return nil, fmt.Errorf("缺少 resolved_rate_multiplier")
	}
	resolved := *wire.ResolvedRateMultiplier
	if !validChannelMonitorMultiplier(resolved) {
		return nil, fmt.Errorf("resolved_rate_multiplier 无效")
	}
	effective := resolved
	if wire.EffectiveRateMultiplier != nil {
		effective = *wire.EffectiveRateMultiplier
	}
	if !validChannelMonitorMultiplier(effective) {
		return nil, fmt.Errorf("effective_rate_multiplier 无效")
	}
	group := resolved
	if wire.GroupRateMultiplier != nil && validChannelMonitorMultiplier(*wire.GroupRateMultiplier) {
		group = *wire.GroupRateMultiplier
	}
	data := map[string]any{
		"object":                    "sub2api.key_billing",
		"schema_version":            1,
		"billing_scope":             "token",
		"group_rate_multiplier":     group,
		"resolved_rate_multiplier":  resolved,
		"effective_rate_multiplier": effective,
		"peak_rate_enabled":         false,
		"observed_at":               now.Format(time.RFC3339Nano),
	}
	if wire.UserRateMultiplier != nil && validChannelMonitorMultiplier(*wire.UserRateMultiplier) {
		data["user_rate_multiplier"] = *wire.UserRateMultiplier
	}
	if observed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(wire.ObservedAt)); err == nil && !observed.IsZero() {
		data["observed_at"] = observed.UTC().Format(time.RFC3339Nano)
	}
	if wire.PeakRateEnabled != nil {
		data["peak_rate_enabled"] = *wire.PeakRateEnabled
	}
	if wire.PeakStart != nil && strings.TrimSpace(*wire.PeakStart) != "" {
		data["peak_start"] = strings.TrimSpace(*wire.PeakStart)
	}
	if wire.PeakEnd != nil && strings.TrimSpace(*wire.PeakEnd) != "" {
		data["peak_end"] = strings.TrimSpace(*wire.PeakEnd)
	}
	if wire.Timezone != nil && strings.TrimSpace(*wire.Timezone) != "" {
		data["timezone"] = strings.TrimSpace(*wire.Timezone)
	}
	if wire.PeakRateMultiplier != nil && validChannelMonitorMultiplier(*wire.PeakRateMultiplier) {
		data["peak_rate_multiplier"] = *wire.PeakRateMultiplier
	}
	if wire.AppliedPeakMultiplier != nil && validChannelMonitorMultiplier(*wire.AppliedPeakMultiplier) {
		data["applied_peak_multiplier"] = *wire.AppliedPeakMultiplier
	}
	return data, nil
}

func validChannelMonitorMultiplier(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func channelMonitorRetryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func nextChannelMonitorBillingAt(status string, checkedAt time.Time, failureCount int, retryAfter time.Duration, accountID int64) time.Time {
	delay := channelMonitorBillingInterval
	switch status {
	case database.ChannelMonitorBillingStatusUnsupported:
		delay = channelMonitorUnsupportedBilling
	case database.ChannelMonitorBillingStatusFailed:
		if failureCount < 1 {
			failureCount = 1
		}
		exponent := failureCount - 1
		if exponent > 3 {
			exponent = 3
		}
		delay = channelMonitorBillingInterval * time.Duration(1<<exponent)
		if delay > 6*time.Hour {
			delay = 6 * time.Hour
		}
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	window := delay / 20
	seed := checkedAt.UnixNano() ^ (accountID << 1)
	if seed < 0 {
		seed = -seed
	}
	if window > 0 {
		delay += time.Duration(seed % int64(window))
	}
	return checkedAt.Add(delay)
}
