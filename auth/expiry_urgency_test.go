package auth

import (
	"testing"
	"time"
)

func TestExpiryUrgencyBonus(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		plan      string
		expiresAt time.Time
		wantBonus float64
	}{
		{"no_expiry_set", "team", time.Time{}, 0},
		{"free_plan_skipped", "free", now.Add(2 * 24 * time.Hour), 0},
		{"api_plan_skipped", "api", now.Add(2 * 24 * time.Hour), 0},
		{"already_expired", "team", now.Add(-1 * time.Hour), 0},
		{"within_3d_urgent", "team", now.Add(2 * 24 * time.Hour), expiryUrgencyUrgentBonus},
		{"3d_boundary_urgent", "plus", now.Add(3 * 24 * time.Hour), expiryUrgencyUrgentBonus},
		{"within_7d_warn", "plus", now.Add(5 * 24 * time.Hour), expiryUrgencyWarnBonus},
		{"7d_boundary_warn", "team", now.Add(7 * 24 * time.Hour), expiryUrgencyWarnBonus},
		{"beyond_7d_no_bonus", "team", now.Add(10 * 24 * time.Hour), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{PlanType: tc.plan, SubscriptionExpiresAt: tc.expiresAt}
			a.mu.Lock()
			got := a.expiryUrgencyBonusLocked(now)
			a.mu.Unlock()
			if got != tc.wantBonus {
				t.Fatalf("plan=%s expires=%v want bonus=%v got=%v",
					tc.plan, tc.expiresAt, tc.wantBonus, got)
			}
		})
	}
}
