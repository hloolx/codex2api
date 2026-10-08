package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type channelMonitorConfigRequest struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	Model           string `json:"model"`
}

type channelMonitorConfigResponse struct {
	AccountID       int64    `json:"account_id"`
	Enabled         bool     `json:"enabled"`
	IntervalMinutes int      `json:"interval_minutes"`
	Model           string   `json:"model"`
	AvailableModels []string `json:"available_models"`
	LastCheckedAt   *string  `json:"last_checked_at,omitempty"`
	NextCheckAt     *string  `json:"next_check_at,omitempty"`
}

type channelMonitorCheckResponse struct {
	Status       string `json:"status"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	FirstTokenMS int64  `json:"first_token_ms"`
	CheckedAt    string `json:"checked_at"`
}

type channelMonitorBillingResponse struct {
	Status       string         `json:"status"`
	Data         map[string]any `json:"data,omitempty"`
	Message      string         `json:"message,omitempty"`
	HTTPStatus   int            `json:"http_status,omitempty"`
	CheckedAt    *string        `json:"checked_at,omitempty"`
	SuccessAt    *string        `json:"success_at,omitempty"`
	NextCheckAt  *string        `json:"next_check_at,omitempty"`
	FailureCount int            `json:"failure_count,omitempty"`
}

type channelMonitorCardResponse struct {
	AccountID       int64                         `json:"account_id"`
	Name            string                        `json:"name"`
	BaseURL         string                        `json:"base_url"`
	Model           string                        `json:"model"`
	IntervalMinutes int                           `json:"interval_minutes"`
	Status          string                        `json:"status"`
	HTTPStatus      int                           `json:"http_status,omitempty"`
	LatencyMS       int64                         `json:"latency_ms"`
	FirstTokenMS    int64                         `json:"first_token_ms"`
	Message         string                        `json:"message,omitempty"`
	LastCheckedAt   *string                       `json:"last_checked_at,omitempty"`
	NextCheckAt     *string                       `json:"next_check_at,omitempty"`
	Availability7d  *float64                      `json:"availability_7d,omitempty"`
	Checks7d        int64                         `json:"checks_7d"`
	Billing         channelMonitorBillingResponse `json:"billing"`
	RecentChecks    []channelMonitorCheckResponse `json:"recent_checks"`
}

type channelMonitorListResponse struct {
	Items       []channelMonitorCardResponse `json:"items"`
	GeneratedAt string                       `json:"generated_at"`
}

type channelMonitorBillingRateItemResponse struct {
	AccountID int64                         `json:"account_id"`
	Billing   channelMonitorBillingResponse `json:"billing"`
}

type channelMonitorBillingRatesResponse struct {
	Items       []channelMonitorBillingRateItemResponse `json:"items"`
	GeneratedAt string                                  `json:"generated_at"`
}

func parseChannelMonitorAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return 0, false
	}
	return id, true
}

func (h *Handler) loadChannelMonitorAccount(ctx context.Context, accountID int64) (*auth.Account, *database.AccountRow, error) {
	if h == nil || h.db == nil || h.store == nil {
		return nil, nil, errors.New("渠道监控服务不可用")
	}
	row, err := h.db.GetAccountByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("账号不存在")
		}
		return nil, nil, err
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		account, err = h.store.BuildTransientAccountByID(ctx, accountID)
		if err != nil {
			return nil, nil, err
		}
	}
	if account == nil || !account.IsOpenAIResponsesAPI() || account.IsGrokAPI() ||
		!strings.EqualFold(strings.TrimSpace(row.Platform), "openai") ||
		!strings.EqualFold(strings.TrimSpace(row.Type), "responses_api") {
		return nil, nil, fmt.Errorf("仅 Responses API 类型的 Codex 渠道支持监控")
	}
	return account, row, nil
}

func channelMonitorTextModels(account *auth.Account) []string {
	if account == nil {
		return []string{}
	}
	models := account.OpenAIResponsesModels()
	result := make([]string, 0, len(models))
	for _, model := range models {
		if isTextConnectionModel(model) {
			result = append(result, strings.TrimSpace(model))
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result
}

func (h *Handler) defaultChannelMonitorModel(ctx context.Context, account *auth.Account) string {
	model, err := h.connectionTestModelForAccount(ctx, account, "")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(model)
}

func channelMonitorTime(value sql.NullTime) *string {
	if !value.Valid || value.Time.IsZero() {
		return nil
	}
	formatted := value.Time.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func buildChannelMonitorConfigResponse(config *database.ChannelMonitorConfig, models []string) channelMonitorConfigResponse {
	return channelMonitorConfigResponse{
		AccountID:       config.AccountID,
		Enabled:         config.Enabled,
		IntervalMinutes: config.IntervalMinutes,
		Model:           config.Model,
		AvailableModels: models,
		LastCheckedAt:   channelMonitorTime(config.LastCheckedAt),
		NextCheckAt:     channelMonitorTime(config.NextCheckAt),
	}
}

// GetChannelMonitorConfig returns defaults without creating a database row.
// This keeps unopened Responses accounts completely inert.
func (h *Handler) GetChannelMonitorConfig(c *gin.Context) {
	accountID, ok := parseChannelMonitorAccountID(c)
	if !ok {
		return
	}
	account, _, err := h.loadChannelMonitorAccount(c.Request.Context(), accountID)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	config, err := h.db.GetChannelMonitorConfig(c.Request.Context(), accountID)
	if errors.Is(err, sql.ErrNoRows) {
		config = &database.ChannelMonitorConfig{
			AccountID:       accountID,
			Enabled:         false,
			IntervalMinutes: database.DefaultChannelMonitorIntervalMinutes,
			Model:           h.defaultChannelMonitorModel(c.Request.Context(), account),
		}
	} else if err != nil {
		writeError(c, http.StatusInternalServerError, "读取渠道监控配置失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, buildChannelMonitorConfigResponse(config, channelMonitorTextModels(account)))
}

func (h *Handler) UpdateChannelMonitorConfig(c *gin.Context) {
	accountID, ok := parseChannelMonitorAccountID(c)
	if !ok {
		return
	}
	account, _, err := h.loadChannelMonitorAccount(c.Request.Context(), accountID)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	var request channelMonitorConfigRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式无效")
		return
	}
	if request.IntervalMinutes < database.MinChannelMonitorIntervalMinutes ||
		request.IntervalMinutes > database.MaxChannelMonitorIntervalMinutes {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("探测间隔必须在 %d 到 %d 分钟之间",
			database.MinChannelMonitorIntervalMinutes, database.MaxChannelMonitorIntervalMinutes))
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		request.Model = h.defaultChannelMonitorModel(c.Request.Context(), account)
	}
	if len(request.Model) > 255 || strings.ContainsAny(request.Model, "\r\n\x00") {
		writeError(c, http.StatusBadRequest, "探测模型无效")
		return
	}
	if request.Enabled && !isTextConnectionModel(request.Model) {
		writeError(c, http.StatusBadRequest, "启用监控前请选择文本模型")
		return
	}
	config, err := h.db.UpsertChannelMonitorConfig(c.Request.Context(), accountID, request.Enabled,
		request.IntervalMinutes, request.Model, time.Now())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "保存渠道监控配置失败: "+err.Error())
		return
	}
	if request.Enabled {
		h.signalChannelMonitor()
	}
	c.JSON(http.StatusOK, buildChannelMonitorConfigResponse(config, channelMonitorTextModels(account)))
}

func (h *Handler) ListChannelMonitors(c *gin.Context) {
	now := time.Now().UTC()
	configs, err := h.db.ListEnabledChannelMonitors(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取渠道监控失败: "+err.Error())
		return
	}
	availability, err := h.db.GetChannelMonitorAvailability(c.Request.Context(), now.Add(-7*24*time.Hour))
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取渠道监控可用率失败: "+err.Error())
		return
	}
	items := make([]channelMonitorCardResponse, 0, len(configs))
	for i := range configs {
		config := &configs[i]
		account, row, loadErr := h.loadChannelMonitorAccount(c.Request.Context(), config.AccountID)
		if loadErr != nil {
			continue
		}
		baseURL, _ := account.OpenAIResponsesCredentials()
		name := strings.TrimSpace(row.Name)
		if name == "" {
			name = baseURL
		}
		card := channelMonitorCardResponse{
			AccountID:       config.AccountID,
			Name:            name,
			BaseURL:         baseURL,
			Model:           config.Model,
			IntervalMinutes: config.IntervalMinutes,
			Status:          config.Status,
			HTTPStatus:      config.HTTPStatus,
			LatencyMS:       config.LatencyMS,
			FirstTokenMS:    config.FirstTokenMS,
			Message:         config.Message,
			LastCheckedAt:   channelMonitorTime(config.LastCheckedAt),
			NextCheckAt:     channelMonitorTime(config.NextCheckAt),
			Billing: channelMonitorBillingResponse{
				Status:       config.BillingStatus,
				Message:      config.BillingMessage,
				HTTPStatus:   config.BillingHTTPStatus,
				CheckedAt:    channelMonitorTime(config.BillingCheckedAt),
				SuccessAt:    channelMonitorTime(config.BillingSuccessAt),
				NextCheckAt:  channelMonitorTime(config.BillingNextCheckAt),
				FailureCount: config.BillingFailureCount,
			},
			RecentChecks: []channelMonitorCheckResponse{},
		}
		if raw := strings.TrimSpace(config.BillingData); raw != "" && raw != "{}" {
			_ = json.Unmarshal([]byte(raw), &card.Billing.Data)
		}
		if stats, exists := availability[config.AccountID]; exists {
			card.Checks7d = stats.Total
			if stats.Total > 0 {
				value := float64(stats.Available) * 100 / float64(stats.Total)
				card.Availability7d = &value
			}
		}
		checks, historyErr := h.db.ListChannelMonitorChecks(c.Request.Context(), config.AccountID,
			now.Add(-7*24*time.Hour), 60)
		if historyErr == nil {
			for index := len(checks) - 1; index >= 0; index-- {
				check := checks[index]
				card.RecentChecks = append(card.RecentChecks, channelMonitorCheckResponse{
					Status:       check.Status,
					HTTPStatus:   check.HTTPStatus,
					LatencyMS:    check.LatencyMS,
					FirstTokenMS: check.FirstTokenMS,
					CheckedAt:    check.CheckedAt.UTC().Format(time.RFC3339Nano),
				})
			}
		}
		items = append(items, card)
	}
	c.JSON(http.StatusOK, channelMonitorListResponse{
		Items:       items,
		GeneratedAt: now.Format(time.RFC3339Nano),
	})
}

// ListChannelMonitorBillingRates is the lightweight account-list projection.
// It deliberately omits health history so rendering a balance column does not
// turn into one history query per monitored account.
func (h *Handler) ListChannelMonitorBillingRates(c *gin.Context) {
	now := time.Now().UTC()
	configs, err := h.db.ListEnabledChannelMonitors(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取渠道倍率失败: "+err.Error())
		return
	}
	items := make([]channelMonitorBillingRateItemResponse, 0, len(configs))
	for i := range configs {
		config := &configs[i]
		billing := channelMonitorBillingResponse{
			Status:       config.BillingStatus,
			Message:      config.BillingMessage,
			HTTPStatus:   config.BillingHTTPStatus,
			CheckedAt:    channelMonitorTime(config.BillingCheckedAt),
			SuccessAt:    channelMonitorTime(config.BillingSuccessAt),
			NextCheckAt:  channelMonitorTime(config.BillingNextCheckAt),
			FailureCount: config.BillingFailureCount,
		}
		if raw := strings.TrimSpace(config.BillingData); raw != "" && raw != "{}" {
			_ = json.Unmarshal([]byte(raw), &billing.Data)
		}
		items = append(items, channelMonitorBillingRateItemResponse{
			AccountID: config.AccountID,
			Billing:   billing,
		})
	}
	c.JSON(http.StatusOK, channelMonitorBillingRatesResponse{
		Items:       items,
		GeneratedAt: now.Format(time.RFC3339Nano),
	})
}

func (h *Handler) ProbeChannelMonitorNow(c *gin.Context) {
	accountID, ok := parseChannelMonitorAccountID(c)
	if !ok {
		return
	}
	config, err := h.db.GetChannelMonitorConfig(c.Request.Context(), accountID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !config.Enabled) {
		writeError(c, http.StatusConflict, "该渠道尚未启用监控")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取渠道监控配置失败: "+err.Error())
		return
	}
	probeCtx, cancel := context.WithTimeout(c.Request.Context(), channelMonitorProbeTimeout)
	defer cancel()
	if err := h.runChannelMonitorAccount(probeCtx, accountID, true); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errChannelMonitorBusy) {
			status = http.StatusConflict
		}
		writeError(c, status, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "渠道健康与倍率探测完成"})
}
