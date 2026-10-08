package auth

import (
	"testing"
	"time"
)

func TestTransientBackoffCapsOnlyLocalLadder(t *testing.T) {
	if got := nextTransientRateLimitCooldown(100, 0); got != 5*time.Minute {
		t.Fatalf("local cap=%v", got)
	}
	if got := nextTransientRateLimitCooldown(0, 15*time.Minute); got != 15*time.Minute {
		t.Fatalf("server minimum truncated: %v", got)
	}
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	store := &Store{accounts: []*Account{acc}}
	store.maxConcurrency.Store(4)
	store.MarkTransientRateLimited(acc, 0)
	if got := store.MarkTransientRateLimited(acc, 15*time.Minute); got < 14*time.Minute {
		t.Fatalf("same-window extension truncated: %v", got)
	}
}
