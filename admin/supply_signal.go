package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/ipv6state"
	"github.com/gin-gonic/gin"
)

type supplyPolicy struct {
	AccountBudget     float64  `json:"account_budget_usd"`
	Throughput        float64  `json:"sustainable_usd_per_hour"`
	ProductiveMinutes float64  `json:"productive_minutes"`
	LeadMinutes       float64  `json:"lead_minutes"`
	BufferMinutes     float64  `json:"buffer_minutes"`
	Pending           int      `json:"pending_accounts"`
	MinReady          int      `json:"min_ready_accounts"`
	DemandOverride    *float64 `json:"demand_usd_per_hour_override"`
}

type supplyModel struct {
	Model             string     `json:"model"`
	Valid             bool       `json:"valid"`
	Available         bool       `json:"available"`
	IssuedAt          *time.Time `json:"issued_at"`
	CapturedAt        *time.Time `json:"captured_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	Restriction       string     `json:"restriction,omitempty"`
	CaptureHTTPStatus int        `json:"last_capture_http_status"`
}

type supplyAccount struct {
	ID                 int64                   `json:"id"`
	Email              string                  `json:"email"`
	Status             string                  `json:"status"`
	Enabled            bool                    `json:"enabled"`
	Ready              bool                    `json:"ready"`
	NeedsRelogin       bool                    `json:"needs_relogin"`
	ReloginReason      string                  `json:"relogin_reason,omitempty"`
	RegisteredAt       *time.Time              `json:"registered_at"`
	OnlineAt           *time.Time              `json:"online_at"`
	ExpiresAt          *time.Time              `json:"expires_at"`
	RemainingSeconds   int64                   `json:"remaining_seconds"`
	LastUsedAt         *time.Time              `json:"last_dispatched_at"`
	LastUnauthorizedAt *time.Time              `json:"last_unauthorized_at"`
	CooldownUntil      *time.Time              `json:"cooldown_until"`
	ActiveRequests     int64                   `json:"active_requests"`
	Models             []supplyModel           `json:"models"`
	Activity           database.SupplyActivity `json:"activity"`
}

type supplySummary struct {
	Total          int `json:"total_accounts"`
	Ready          int `json:"ready_accounts"`
	Active         int `json:"active_ready_accounts"`
	Valid          int `json:"valid_state_accounts"`
	Unauthorized   int `json:"unauthorized_accounts"`
	Relogin        int `json:"relogin_accounts"`
	Expiring       int `json:"expiring_within_horizon"`
	ReadyAtHorizon int `json:"ready_at_horizon"`
}

type supplyRecommendation struct {
	Action            string     `json:"action"`
	Reason            string     `json:"reason"`
	AddNow            int        `json:"add_accounts_now"`
	TargetReady       int        `json:"target_ready_accounts"`
	EffectiveCapacity float64    `json:"effective_capacity_per_account_usd"`
	ReadyPerHour      float64    `json:"ready_accounts_per_hour"`
	IntervalMinutes   *float64   `json:"interval_minutes_per_account"`
	NextActionAt      *time.Time `json:"next_action_at"`
	ReloginIDs        []int64    `json:"relogin_account_ids"`
	SignalKey         string     `json:"signal_key"`
}

type supplyResponse struct {
	Version          int                  `json:"version"`
	GeneratedAt      time.Time            `json:"generated_at"`
	PollAfterSeconds int                  `json:"poll_after_seconds"`
	Models           []string             `json:"required_models"`
	OnlineAtSource   string               `json:"online_at_source"`
	Policy           supplyPolicy         `json:"policy"`
	Summary          supplySummary        `json:"summary"`
	Demand15         float64              `json:"successful_demand_usd_per_hour_15m"`
	Demand60         float64              `json:"successful_demand_usd_per_hour_60m"`
	Demand           float64              `json:"planning_demand_usd_per_hour"`
	ActivityAsOf     *time.Time           `json:"activity_as_of"`
	DataQuality      string               `json:"data_quality"`
	Warnings         []string             `json:"warnings"`
	Recommendation   supplyRecommendation `json:"recommendation"`
	Accounts         []supplyAccount      `json:"accounts"`
}

func supplyTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func supplyUnix(t int64) *time.Time {
	if t <= 0 {
		return nil
	}
	return supplyTime(time.Unix(t, 0))
}

func parseSupplyPolicy(c *gin.Context) (supplyPolicy, error) {
	p := supplyPolicy{AccountBudget: 400, Throughput: 410, ProductiveMinutes: 50, LeadMinutes: 10, BufferMinutes: 5}
	for _, param := range []struct {
		name     string
		out      *float64
		min, max float64
	}{
		{"account_budget_usd", &p.AccountBudget, 0.01, 1e6}, {"sustainable_usd_per_hour", &p.Throughput, 0.01, 1e6},
		{"productive_minutes", &p.ProductiveMinutes, 1, 60}, {"lead_minutes", &p.LeadMinutes, 0, 45}, {"buffer_minutes", &p.BufferMinutes, 0, 15},
	} {
		if value, ok := c.GetQuery(param.name); ok {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < param.min || n > param.max {
				return p, fmt.Errorf("invalid %s", param.name)
			}
			*param.out = n
		}
	}
	for _, param := range []struct {
		name string
		out  *int
	}{{"pending_accounts", &p.Pending}, {"min_ready_accounts", &p.MinReady}} {
		if value, ok := c.GetQuery(param.name); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 100000 {
				return p, fmt.Errorf("invalid %s", param.name)
			}
			*param.out = n
		}
	}
	if value, ok := c.GetQuery("demand_usd_per_hour"); ok {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e8 {
			return p, fmt.Errorf("invalid demand_usd_per_hour")
		}
		p.DemandOverride = &n
	}
	return p, nil
}

// GetSupplySignal reads runtime state and persisted usage only. It never probes,
// refreshes, clears cooldowns, or starts logins. All models are required together.
func (h *Handler) GetSupplySignal(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h.store == nil || h.ipv6State == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "State supply is unavailable")
		return
	}
	p, err := parseSupplyPolicy(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	state := h.ipv6State.Status()
	models := slices.Clone(state.Config.Models)
	if raw, ok := c.GetQuery("models"); ok {
		models = strings.Split(raw, ",")
		seen := map[string]bool{}
		for i, model := range models {
			model = strings.TrimSpace(model)
			if !slices.Contains(state.Config.Models, model) || seen[model] {
				writeError(c, 400, "models must be distinct exact models in State capture scope")
				return
			}
			models[i], seen[model] = model, true
		}
	}
	sort.Strings(models)
	now := time.Unix(state.ServerTime, 0).UTC()
	activity, errActivity := h.db.GetSupplyActivity(c.Request.Context(), now, models)
	byID := map[int64]database.SupplyActivity{}
	for _, row := range activity {
		byID[row.AccountID] = row
	}
	entries := map[int64]map[string]ipv6state.Entry{}
	for _, entry := range state.Entries {
		if entries[entry.AccountID] == nil {
			entries[entry.AccountID] = map[string]ipv6state.Entry{}
		}
		entries[entry.AccountID][entry.Model] = entry
	}
	accounts := []supplyAccount{}
	for _, account := range h.store.Accounts() {
		if !ipv6state.NativeAccount(account) || (len(state.Config.AccountIDs) > 0 && !slices.Contains(state.Config.AccountIDs, account.ID())) {
			continue
		}
		runtime := account.GetAccountListRuntimeSnapshot()
		account.Mu().RLock()
		email, errorMessage := account.Email, strings.ToLower(account.ErrorMsg)
		account.Mu().RUnlock()
		a := supplyAccount{ID: account.ID(), Email: email, Status: runtime.Status,
			Enabled:      atomic.LoadInt32(&account.DispatchPaused) == 0,
			RegisteredAt: supplyTime(time.Unix(0, account.AddedAt.Load())),
			LastUsedAt:   supplyTime(account.GetLastUsedAt()), LastUnauthorizedAt: supplyTime(runtime.LastUnauthorizedAt),
			CooldownUntil: supplyTime(runtime.CooldownUntil), ActiveRequests: runtime.ActiveRequests,
			Ready: state.Config.Enabled && len(models) > 0, Models: []supplyModel{}, Activity: byID[account.ID()]}
		a.Activity.AccountID = a.ID
		_, restriction, _ := account.StateAvailability("", now)
		if restriction == "unauthorized" {
			a.NeedsRelogin = a.Enabled
			a.ReloginReason = "unauthorized"
			if strings.Contains(errorMessage, "token_revoked") {
				a.ReloginReason = "token_revoked"
			}
		}
		for _, model := range models {
			e := entries[a.ID][model]
			valid := e.Valid && e.ExpiresAt > now.Unix()
			available := valid && e.Available && state.Config.Enabled
			a.Models = append(a.Models, supplyModel{Model: model, Valid: valid, Available: available,
				IssuedAt: supplyUnix(e.IssuedAt), CapturedAt: supplyUnix(e.CapturedAt), ExpiresAt: supplyUnix(e.ExpiresAt),
				Restriction: e.CooldownReason, CaptureHTTPStatus: e.HTTPStatus})
			a.Ready = a.Ready && available
			if e.CapturedAt > 0 && (a.OnlineAt == nil || e.CapturedAt > a.OnlineAt.Unix()) {
				a.OnlineAt = supplyUnix(e.CapturedAt)
			}
			if e.ExpiresAt > 0 && (a.ExpiresAt == nil || e.ExpiresAt < a.ExpiresAt.Unix()) {
				a.ExpiresAt = supplyUnix(e.ExpiresAt)
			}
		}
		if !a.Ready {
			a.OnlineAt = nil
		}
		if a.ExpiresAt != nil {
			a.RemainingSeconds = max(0, a.ExpiresAt.Unix()-now.Unix())
		}
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	response := buildSupplySignal(now, models, p, accounts, activity)
	if !state.Config.Enabled {
		response.DataQuality = "unavailable"
		response.Warnings = append(response.Warnings, "state_automation_disabled")
	}
	if errActivity != nil || h.db.GetUsageLogMode() != "full" {
		response.DataQuality = "unavailable"
		response.Warnings = append(response.Warnings, "usage_metrics_unavailable_or_incomplete")
	}
	if response.DataQuality == "unavailable" {
		response.Recommendation.Action, response.Recommendation.Reason = "observe", "metrics_or_state_unavailable"
		response.Recommendation.AddNow, response.Recommendation.NextActionAt = 0, nil
	}
	c.JSON(http.StatusOK, response)
}

func buildSupplySignal(now time.Time, models []string, p supplyPolicy, accounts []supplyAccount, activity []database.SupplyActivity) supplyResponse {
	r := supplyResponse{Version: 1, GeneratedAt: now, PollAfterSeconds: 30, Models: models,
		OnlineAtSource: "latest_required_state_captured_at_not_login_time", Policy: p,
		DataQuality: "estimated", Accounts: accounts, Warnings: []string{
			"local_billing_is_not_upstream_credit_balance", "throughput_and_budget_are_planning_assumptions",
			"successful_traffic_excludes_unserved_demand", "capture_attempts_are_not_in_business_usage_logs",
			"expiry_forecast_does_not_predict_new_upstream_throttles_or_quota_exhaustion",
			"pending_accounts_must_be_owned_by_one_external_controller_and_ready_within_horizon",
		}, Recommendation: supplyRecommendation{Action: "hold", Reason: "capacity_sufficient", ReloginIDs: []int64{}}}
	for _, row := range activity {
		r.Demand15 += max(0, row.Billed15m) * 4
		r.Demand60 += max(0, row.Billed60m)
		if row.LastAttemptAt != nil && (r.ActivityAsOf == nil || row.LastAttemptAt.After(*r.ActivityAsOf)) {
			r.ActivityAsOf = row.LastAttemptAt
		}
	}
	r.Demand = max(r.Demand15, r.Demand60)
	if p.DemandOverride != nil {
		r.Demand = *p.DemandOverride
	}
	capacity := min(p.AccountBudget, p.Throughput*p.ProductiveMinutes/60)
	rec := &r.Recommendation
	rec.EffectiveCapacity = capacity
	rec.ReadyPerHour = r.Demand / capacity
	rate := capacity * 60 / p.ProductiveMinutes
	rec.TargetReady = max(p.MinReady, int(math.Ceil(r.Demand/rate)))
	if rec.ReadyPerHour > 0 {
		interval := 60 / rec.ReadyPerHour
		rec.IntervalMinutes = &interval
	}
	horizon := now.Add(time.Duration((p.LeadMinutes + p.BufferMinutes) * float64(time.Minute)))
	expiries := []time.Time{}
	for _, a := range accounts {
		r.Summary.Total++
		if a.ReloginReason != "" {
			r.Summary.Unauthorized++
		}
		if a.NeedsRelogin {
			rec.ReloginIDs = append(rec.ReloginIDs, a.ID)
			r.Summary.Relogin++
		}
		valid := len(a.Models) > 0
		for _, model := range a.Models {
			valid = valid && model.Valid
		}
		if valid {
			r.Summary.Valid++
		}
		if !a.Ready || a.ExpiresAt == nil || !a.ExpiresAt.After(now) {
			continue
		}
		r.Summary.Ready++
		if a.ActiveRequests > 0 {
			r.Summary.Active++
		}
		expiries = append(expiries, *a.ExpiresAt)
		if a.ExpiresAt.After(horizon) {
			r.Summary.ReadyAtHorizon++
		} else {
			r.Summary.Expiring++
		}
	}
	rec.AddNow = max(0, rec.TargetReady-r.Summary.ReadyAtHorizon-p.Pending)
	if rec.AddNow > 0 {
		rec.Action, rec.Reason, rec.NextActionAt = "replenish", "capacity_shortfall_within_lead_time", supplyTime(now)
	} else {
		sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })
		for i, expires := range expiries {
			// Pending logins only cover the current horizon; do not assume they last forever.
			if len(expiries)-i-1 < rec.TargetReady {
				next := expires.Add(-time.Duration((p.LeadMinutes + p.BufferMinutes) * float64(time.Minute)))
				if next.After(now) {
					rec.NextActionAt = supplyTime(next)
				}
				break
			}
		}
	}
	if len(rec.ReloginIDs) > 0 && rec.AddNow == 0 {
		rec.Action, rec.Reason = "relogin", "current_unauthorized_accounts"
	}
	if r.ActivityAsOf == nil && p.DemandOverride == nil && p.MinReady == 0 && len(rec.ReloginIDs) == 0 {
		rec.Action, rec.Reason = "observe", "no_recent_business_demand"
	}
	// Stable across polling timestamps; this is a snapshot key, not a reservation.
	type readyIdentity struct {
		ID        int64
		ExpiresAt *time.Time
	}
	readyIDs := []readyIdentity{}
	for _, account := range accounts {
		if account.Ready {
			readyIDs = append(readyIDs, readyIdentity{account.ID, account.ExpiresAt})
		}
	}
	sort.Slice(readyIDs, func(i, j int) bool { return readyIDs[i].ID < readyIDs[j].ID })
	sort.Slice(rec.ReloginIDs, func(i, j int) bool { return rec.ReloginIDs[i] < rec.ReloginIDs[j] })
	keyData, _ := json.Marshal(struct {
		Models      []string
		Ready       []readyIdentity
		IDs         []int64
		Policy      supplyPolicy
		Target, Add int
	}{models, readyIDs, rec.ReloginIDs, p, rec.TargetReady, rec.AddNow})
	hash := sha256.Sum256(keyData)
	rec.SignalKey = hex.EncodeToString(hash[:16])
	return r
}
