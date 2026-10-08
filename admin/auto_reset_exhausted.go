package admin

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

// Exhaustion is capability-based: Business Pro Lite and other official Codex
// accounts may hold reset credits even though the expiry policy is Plus/Pro-only.
func exhaustedResetAccountEligible(account *auth.Account) bool {
	if !isCodexOAuthAccount(account) || strings.TrimSpace(account.GetAccessToken()) == "" {
		return false
	}
	switch account.RuntimeStatus() {
	case "error", "unauthorized":
		return false
	default:
		return true
	}
}

func autoResetCreditsAccountEligible(account *auth.Account, settings autoResetCreditsConfig) bool {
	if account == nil || strings.TrimSpace(account.GetAccessToken()) == "" {
		return false
	}
	// 管理员禁用（DispatchPaused）或 401 即时摘除（Disabled）的账号不接流量，
	// 自动消耗只会白白浪费重置券；手动重置不经过这里，仍可按需使用。
	if atomic.LoadInt32(&account.DispatchPaused) != 0 || atomic.LoadInt32(&account.Disabled) != 0 {
		return false
	}
	return (settings.Enabled && isAutoResetCreditsPlan(account.GetPlanType())) ||
		(settings.OnExhaustionEnabled && exhaustedResetAccountEligible(account))
}

func selectAutoResetCredit(account *auth.Account, settings autoResetCreditsConfig, credits []proxy.WhamResetCreditItem, usage *proxy.WhamUsage, observedAt, now time.Time) (proxy.WhamResetCreditItem, time.Time, string, bool) {
	if !autoResetCreditsAccountEligible(account, settings) {
		return proxy.WhamResetCreditItem{}, time.Time{}, "", false
	}
	// Expiry retains its existing plan gate and threshold. If both policies match,
	// redeem only once, with the same per-credit idempotency key.
	if settings.Enabled && isAutoResetCreditsPlan(account.GetPlanType()) {
		if credit, expiresAt, ok := earliestAutoResetCredit(credits, now, time.Duration(settings.BeforeExpiryMin)*time.Minute); ok {
			return credit, expiresAt, "expiring", true
		}
	}
	if settings.OnExhaustionEnabled && exhaustedResetAccountEligible(account) && resetUsageExhausted(usage, observedAt, now) {
		// Exhaustion has no imminence requirement; still reject expired/malformed coupons.
		credit, expiresAt, ok := earliestAutoResetCredit(credits, now, time.Duration(1<<63-1))
		return credit, expiresAt, "exhausted", ok
	}
	return proxy.WhamResetCreditItem{}, time.Time{}, "", false
}

func resetUsageExhausted(usage *proxy.WhamUsage, observedAt, now time.Time) bool {
	if usage == nil || observedAt.IsZero() || now.Sub(observedAt) > autoResetCreditsAccountTimeout {
		return false
	}
	if credits := usage.RateLimitResetCredits; credits != nil && (credits.AvailableCount <= 0 || credits.ApplicableAvailableCount <= 0) {
		return false
	}
	// Only the shared long (7d-slot) window counts; a full 5h window recovers on its
	// own within hours and is not worth a credit. Model-specific Spark is excluded.
	for _, window := range []*proxy.WhamUsageWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if window == nil || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 100 || !proxy.IsWhamLongWindowSeconds(window.LimitWindowSeconds) {
			continue
		}
		resetAt := time.Unix(window.ResetAt, 0)
		if window.ResetAt <= 0 {
			if window.ResetAfterSeconds <= 0 || window.ResetAfterSeconds > window.LimitWindowSeconds {
				continue
			}
			resetAt = observedAt.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
		}
		if resetAt.After(now) {
			return true
		}
	}
	return false
}

func (h *Handler) queryResetUsageWithRefresh(ctx context.Context, account *auth.Account) (*proxy.WhamUsage, error) {
	query := h.queryResetUsage
	if query == nil {
		query = proxy.QueryWhamUsage
	}
	usage, resp, err := query(ctx, account, h.store.ResolveProxyForAccount(account))
	if upstreamResetStatus(resp) == http.StatusUnauthorized {
		drainResetResponse(resp)
		if refreshErr := h.refreshAccountForReset(ctx, account.DBID); refreshErr != nil {
			return nil, fmt.Errorf("usage refresh after 401: %w", refreshErr)
		}
		usage, resp, err = query(ctx, account, h.store.ResolveProxyForAccount(account))
	}
	if err != nil {
		status := upstreamResetStatus(resp)
		drainResetResponse(resp)
		// Transport errors can contain proxy credentials; do not log their text.
		return nil, fmt.Errorf("reset usage query failed (status %d)", status)
	}
	if usage == nil {
		return nil, fmt.Errorf("reset usage query returned empty response")
	}
	return usage, nil
}
