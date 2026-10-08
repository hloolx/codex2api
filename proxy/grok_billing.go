package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// Grok billing 端点（cli-chat-proxy，与官方 Grok CLI 对齐）。
const (
	grokBillingWeeklyPath  = "/billing?format=credits"
	grokBillingMonthlyPath = "/billing"
)

// GrokProductUsage 是周额度内单个产品（如 GrokBuild / Api）的用量。
type GrokProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
}

// GrokBillingSummary 是 Grok 周/月额度的合并视图，供列表展示。
type GrokBillingSummary struct {
	Plan               string
	CurrentPeriodType  string
	WeeklyPercent      *float64
	WeeklyPeriodStart  string
	WeeklyPeriodEnd    string
	ProductUsage       []GrokProductUsage
	OnDemandCapCents   *float64
	OnDemandUsedCents  *float64
	MonthlyLimitCents  *float64
	MonthlyUsedCents   *float64
	MonthlyPercent     *float64
	MonthlyPeriodStart string
	MonthlyPeriodEnd   string
}

// GrokBillingDetail 是落库/透出给前端的完整额度视图（grok_billing_detail 凭据）。
type GrokBillingDetail struct {
	Plan               string             `json:"plan,omitempty"`
	WeeklyPercent      *float64           `json:"weekly_percent,omitempty"`
	WeeklyPeriodStart  string             `json:"weekly_period_start,omitempty"`
	WeeklyPeriodEnd    string             `json:"weekly_period_end,omitempty"`
	ProductUsage       []GrokProductUsage `json:"product_usage,omitempty"`
	OnDemandCapCents   *float64           `json:"on_demand_cap_cents,omitempty"`
	OnDemandUsedCents  *float64           `json:"on_demand_used_cents,omitempty"`
	MonthlyLimitCents  *float64           `json:"monthly_limit_cents,omitempty"`
	MonthlyUsedCents   *float64           `json:"monthly_used_cents,omitempty"`
	MonthlyPercent     *float64           `json:"monthly_percent,omitempty"`
	MonthlyPeriodStart string             `json:"monthly_period_start,omitempty"`
	MonthlyPeriodEnd   string             `json:"monthly_period_end,omitempty"`
	UpdatedAt          string             `json:"updated_at,omitempty"`
}

// GrokBillingSummaryFromFact builds the legacy display projection from the
// sanitized /billing?format=credits fact. The endpoint exposes one real
// currentPeriod; it must never be duplicated into both weekly and monthly
// buckets. Unknown period types keep amounts/PAYG details but do not invent a
// weekly/monthly percentage or rolling window.
func GrokBillingSummaryFromFact(payload map[string]any, presence map[string]string) *GrokBillingSummary {
	if payload == nil {
		return nil
	}
	config := payload
	if nested, ok := payload["config"].(map[string]any); ok {
		config = nested
	}
	if config == nil {
		return nil
	}
	summary := &GrokBillingSummary{}
	value := func(key string) *float64 {
		raw, exists := config[key]
		if !exists || raw == nil {
			return nil
		}
		if object, ok := raw.(map[string]any); ok {
			raw, exists = object["val"]
			if !exists || raw == nil {
				return nil
			}
		}
		return anyToFloat64(raw)
	}
	text := func(object map[string]any, key string) string {
		if raw, ok := object[key].(string); ok {
			return strings.TrimSpace(raw)
		}
		return ""
	}

	usagePercent := value("creditUsagePercent")
	periodType, periodStart, periodEnd := "", "", ""
	if current, ok := config["currentPeriod"].(map[string]any); ok {
		periodType = strings.ToLower(text(current, "type"))
		periodStart = text(current, "start")
		periodEnd = text(current, "end")
	}
	if periodStart == "" {
		periodStart = text(config, "billingPeriodStart")
	}
	if periodEnd == "" {
		periodEnd = text(config, "billingPeriodEnd")
	}
	summary.CurrentPeriodType = periodType
	switch {
	case strings.Contains(periodType, "weekly"):
		// proto3 JSON omits zero-valued scalars: a weekly period without the key
		// means 0% used, not unknown. Explicit null/invalid values stay unknown.
		if _, exists := config["creditUsagePercent"]; !exists && usagePercent == nil && presence["creditUsagePercent"] != "invalid" {
			zero := float64(0)
			usagePercent = &zero
		}
		summary.WeeklyPercent = usagePercent
		summary.WeeklyPeriodStart = periodStart
		summary.WeeklyPeriodEnd = periodEnd
	case strings.Contains(periodType, "monthly"):
		summary.MonthlyPercent = usagePercent
		summary.MonthlyPeriodStart = periodStart
		summary.MonthlyPeriodEnd = periodEnd
	}

	summary.MonthlyLimitCents = value("monthlyLimit")
	summary.MonthlyUsedCents = value("used")
	summary.OnDemandCapCents = value("onDemandCap")
	summary.OnDemandUsedCents = value("onDemandUsed")
	if summary.OnDemandUsedCents == nil && summary.MonthlyUsedCents != nil && summary.MonthlyLimitCents != nil &&
		*summary.MonthlyUsedCents > *summary.MonthlyLimitCents {
		over := *summary.MonthlyUsedCents - *summary.MonthlyLimitCents
		summary.OnDemandUsedCents = &over
	}
	// Legacy monthly payloads may omit creditUsagePercent; only then derive a
	// display percentage from explicit absolute amount fields.
	if strings.Contains(periodType, "monthly") && summary.MonthlyPercent == nil &&
		summary.MonthlyLimitCents != nil && *summary.MonthlyLimitCents > 0 && summary.MonthlyUsedCents != nil {
		pct := math.Min(*summary.MonthlyUsedCents / *summary.MonthlyLimitCents * 100, 100)
		summary.MonthlyPercent = &pct
	}
	_ = presence // presence remains authoritative in the fact snapshot itself.
	return summary
}

// LegacyGrokBillingCredentialsFromFact creates the additive-migration
// credentials projection from one sanitized billing fact. Explicit nil values
// clear a legacy bucket that does not match the single real currentPeriod, so
// an older monthly observation cannot make a new weekly period look like 30D
// (and vice versa).
func LegacyGrokBillingCredentialsFromFact(account *auth.Account, payload map[string]any, presence map[string]string) map[string]any {
	summary := GrokBillingSummaryFromFact(payload, presence)
	if summary == nil {
		return nil
	}
	credentials := ApplyGrokBilling(nil, account, summary)
	switch {
	case strings.Contains(summary.CurrentPeriodType, "weekly"):
		credentials["grok_monthly_usage_percent"] = nil
		credentials["grok_monthly_period_end"] = nil
	case strings.Contains(summary.CurrentPeriodType, "monthly"):
		credentials["grok_weekly_usage_percent"] = nil
		credentials["grok_weekly_period_end"] = nil
	default:
		credentials["grok_weekly_usage_percent"] = nil
		credentials["grok_weekly_period_end"] = nil
		credentials["grok_monthly_usage_percent"] = nil
		credentials["grok_monthly_period_end"] = nil
	}
	return credentials
}

type grokBillingPayload struct {
	Config *grokBillingConfig `json:"config"`
}

type grokBillingConfig struct {
	CurrentPeriod *struct {
		Type  string `json:"type"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"currentPeriod"`
	CreditUsagePercent *float64 `json:"creditUsagePercent"`
	ProductUsage       []struct {
		Product      string          `json:"product"`
		UsagePercent json.RawMessage `json:"usagePercent"`
	} `json:"productUsage"`
	MonthlyLimit       json.RawMessage `json:"monthlyLimit"`
	Used               json.RawMessage `json:"used"`
	OnDemandCap        json.RawMessage `json:"onDemandCap"`
	OnDemandUsed       json.RawMessage `json:"onDemandUsed"`
	BillingPeriodStart string          `json:"billingPeriodStart"`
	BillingPeriodEnd   string          `json:"billingPeriodEnd"`
}

// FetchGrokBilling 拉取 Grok 账号的周额度 + 月额度（chat-proxy /v1/billing）。
func FetchGrokBilling(ctx context.Context, account *auth.Account, proxyURL string) (*GrokBillingSummary, error) {
	if account == nil || !account.IsGrokAPI() {
		return nil, fmt.Errorf("not a grok account")
	}
	baseURL, bearer := account.GrokCredentials()
	if bearer == "" {
		return nil, fmt.Errorf("grok 账号缺少 access token")
	}
	// billing 只在 chat-proxy 上有完整套餐视图；API Key 账号若走 api.x.ai 也尝试同一 path。
	if baseURL == "" || strings.Contains(baseURL, "api.x.ai") {
		// OAuth 默认 chat-proxy；纯 API Key 也尝试 chat-proxy 拿不到就只试 baseURL。
		if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
			baseURL = auth.GrokDefaultChatProxyBaseURL
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	client := getPooledClient(account, proxyURL)
	weekly, weeklyErr := fetchGrokBillingOnce(ctx, client, account, bearer, baseURL+grokBillingWeeklyPath)
	monthly, monthlyErr := fetchGrokBillingOnce(ctx, client, account, bearer, baseURL+grokBillingMonthlyPath)
	if weeklyErr != nil && monthlyErr != nil {
		return nil, fmt.Errorf("billing 探针失败: weekly=%v monthly=%v", weeklyErr, monthlyErr)
	}

	// Billing is an independent balance/period fact. It must never infer or
	// overwrite a subscription tier: live /user, settings display, JWT and the
	// import archive label are persisted and presented as separate sources.
	summary := &GrokBillingSummary{}
	if weekly != nil && weekly.Config != nil {
		cfg := weekly.Config
		if cfg.CreditUsagePercent != nil {
			v := *cfg.CreditUsagePercent
			summary.WeeklyPercent = &v
		} else if cfg.CurrentPeriod != nil && strings.Contains(strings.ToLower(cfg.CurrentPeriod.Type), "weekly") {
			// proto3 JSON omits a zero creditUsagePercent.
			zero := float64(0)
			summary.WeeklyPercent = &zero
		}
		if cfg.CurrentPeriod != nil {
			summary.WeeklyPeriodStart = strings.TrimSpace(cfg.CurrentPeriod.Start)
			summary.WeeklyPeriodEnd = strings.TrimSpace(cfg.CurrentPeriod.End)
		}
		if summary.WeeklyPeriodStart == "" {
			summary.WeeklyPeriodStart = strings.TrimSpace(cfg.BillingPeriodStart)
		}
		if summary.WeeklyPeriodEnd == "" {
			summary.WeeklyPeriodEnd = strings.TrimSpace(cfg.BillingPeriodEnd)
		}
		summary.ProductUsage = parseGrokProductUsage(cfg)
	}
	if monthly != nil && monthly.Config != nil {
		cfg := monthly.Config
		summary.MonthlyLimitCents = parseGrokCentValue(cfg.MonthlyLimit)
		summary.MonthlyUsedCents = parseGrokCentValue(cfg.Used)
		summary.OnDemandCapCents = parseGrokCentValue(cfg.OnDemandCap)
		summary.OnDemandUsedCents = parseGrokCentValue(cfg.OnDemandUsed)
		summary.MonthlyPeriodStart = strings.TrimSpace(cfg.BillingPeriodStart)
		summary.MonthlyPeriodEnd = strings.TrimSpace(cfg.BillingPeriodEnd)
		if summary.MonthlyLimitCents != nil && *summary.MonthlyLimitCents > 0 && summary.MonthlyUsedCents != nil {
			used := math.Min(*summary.MonthlyUsedCents, *summary.MonthlyLimitCents)
			pct := (used / *summary.MonthlyLimitCents) * 100
			summary.MonthlyPercent = &pct
		}
		// 超出月度包含额度的部分记为按量付费用量（上游未显式给出时推导）。
		if summary.OnDemandUsedCents == nil &&
			summary.MonthlyUsedCents != nil && summary.MonthlyLimitCents != nil &&
			*summary.MonthlyUsedCents > *summary.MonthlyLimitCents {
			over := *summary.MonthlyUsedCents - *summary.MonthlyLimitCents
			summary.OnDemandUsedCents = &over
		}
		if len(summary.ProductUsage) == 0 {
			summary.ProductUsage = parseGrokProductUsage(cfg)
		}
	}
	// weekly 载荷也可能带 onDemand 字段，作为兜底。
	if summary.OnDemandCapCents == nil && weekly != nil && weekly.Config != nil {
		summary.OnDemandCapCents = parseGrokCentValue(weekly.Config.OnDemandCap)
		if summary.OnDemandUsedCents == nil {
			summary.OnDemandUsedCents = parseGrokCentValue(weekly.Config.OnDemandUsed)
		}
	}
	return summary, nil
}

func parseGrokProductUsage(cfg *grokBillingConfig) []GrokProductUsage {
	if cfg == nil || len(cfg.ProductUsage) == 0 {
		return nil
	}
	out := make([]GrokProductUsage, 0, len(cfg.ProductUsage))
	for _, item := range cfg.ProductUsage {
		product := strings.TrimSpace(item.Product)
		if product == "" {
			continue
		}
		out = append(out, GrokProductUsage{
			Product:      product,
			UsagePercent: parseGrokCentValue(item.UsagePercent),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fetchGrokBillingOnce(ctx context.Context, client *http.Client, account *auth.Account, bearer, url string) (*grokBillingPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 复用 Grok CLI 头契约；billing GET 也需要 x-xai-token-auth。
	applyGrokRequestHeaders(req, account, bearer, nil, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Accept") // re-set clean
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized: %s", truncateRunes(string(body), 200))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var payload grokBillingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse billing: %w", err)
	}
	return &payload, nil
}

// ApplyGrokBilling keeps the legacy billing detail fields in sync during the
// additive migration. Billing periods are deliberately not copied into the
// generic 5h/7d quota slots and do not mutate plan_type: rolling 7d/30d usage
// is aggregated from terminal gateway usage events, while subscription facts
// remain independent of balance facts.
func ApplyGrokBilling(store *auth.Store, account *auth.Account, summary *GrokBillingSummary) map[string]interface{} {
	if account == nil || summary == nil {
		return nil
	}
	now := time.Now()
	credentials := map[string]interface{}{}
	_ = store // retained for source compatibility while callers migrate.

	if summary.WeeklyPercent != nil {
		credentials["grok_weekly_usage_percent"] = *summary.WeeklyPercent
	}
	if summary.WeeklyPeriodEnd != "" {
		credentials["grok_weekly_period_end"] = summary.WeeklyPeriodEnd
	}
	if summary.MonthlyPercent != nil {
		credentials["grok_monthly_usage_percent"] = *summary.MonthlyPercent
	}
	if summary.MonthlyLimitCents != nil {
		credentials["grok_monthly_limit_cents"] = *summary.MonthlyLimitCents
	}
	if summary.MonthlyUsedCents != nil {
		credentials["grok_monthly_used_cents"] = *summary.MonthlyUsedCents
	}
	if summary.MonthlyPeriodEnd != "" {
		credentials["grok_monthly_period_end"] = summary.MonthlyPeriodEnd
	}
	if summary.WeeklyPercent != nil || summary.MonthlyPercent != nil {
		credentials["grok_usage_updated_at"] = now.UTC().Format(time.RFC3339)
	}
	// 完整额度视图（产品用量、按量付费、月度金额）单独落一个 JSON 凭据，
	// 供账号列表透出给前端渲染。
	detail := &GrokBillingDetail{
		Plan:               "",
		WeeklyPercent:      summary.WeeklyPercent,
		WeeklyPeriodStart:  summary.WeeklyPeriodStart,
		WeeklyPeriodEnd:    summary.WeeklyPeriodEnd,
		ProductUsage:       summary.ProductUsage,
		OnDemandCapCents:   summary.OnDemandCapCents,
		OnDemandUsedCents:  summary.OnDemandUsedCents,
		MonthlyLimitCents:  summary.MonthlyLimitCents,
		MonthlyUsedCents:   summary.MonthlyUsedCents,
		MonthlyPercent:     summary.MonthlyPercent,
		MonthlyPeriodStart: summary.MonthlyPeriodStart,
		MonthlyPeriodEnd:   summary.MonthlyPeriodEnd,
		UpdatedAt:          now.UTC().Format(time.RFC3339),
	}
	if detailJSON, err := json.Marshal(detail); err == nil {
		credentials["grok_billing_detail"] = string(detailJSON)
	}
	return credentials
}

func parseGrokCentValue(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj struct {
		Val any `json:"val"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Val != nil {
		return anyToFloat64(obj.Val)
	}
	var n any
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return anyToFloat64(n)
}

func anyToFloat64(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	default:
		return nil
	}
}

func parseGrokTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
