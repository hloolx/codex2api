package auth

import (
	"time"

	"github.com/codex2api/cache"
)

const (
	// TransientRateLimitBackoffBase is the first account-wide freeze after a
	// Codex throttle that is not usage_limit / 5h / 7d exhaustion.
	TransientRateLimitBackoffBase = 15 * time.Second
	// TransientRateLimitBackoffMax caps the local exponential ladder only.
	TransientRateLimitBackoffMax = 5 * time.Minute
	// transientRateLimitStableReset is how long an account must stay free of
	// new upstream 429s after LastRateLimitedAt before the backoff ladder
	// resets. Clearing on the first success would recreate a 15s retry loop
	// while concurrent in-flight requests are still being rejected.
	transientRateLimitStableReset = 5 * time.Minute
)

// nextTransientRateLimitCooldown returns the freeze duration for the given
// backoff level, never shorter than a valid upstream Retry-After.
func nextTransientRateLimitCooldown(level int, retryAfter time.Duration) time.Duration {
	if level < 0 {
		level = 0
	}
	cooldown := TransientRateLimitBackoffBase
	for step := 0; step < level && cooldown < TransientRateLimitBackoffMax; step++ {
		cooldown *= 2
	}
	if cooldown > TransientRateLimitBackoffMax {
		cooldown = TransientRateLimitBackoffMax
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
	}
	if cooldown < TransientRateLimitBackoffBase {
		cooldown = TransientRateLimitBackoffBase
	}
	return cooldown
}

// MarkTransientRateLimited applies an account-wide short freeze for a Codex
// throttle. Concurrent 429s that land while the current window is still open
// reuse that deadline instead of climbing the backoff ladder. An already
// longer quota cooldown is left untouched.
//
// Unlike a quota cooldown the freeze is seconds long, so it neither triggers
// a WHAM usage probe nor is written to the database: under a burst that would
// turn every throttled account into one probe plus one write per window.
// The scheduler and the cross-instance cooldown cache are still updated.
func (s *Store) MarkTransientRateLimited(acc *Account, retryAfter time.Duration) time.Duration {
	if s == nil || acc == nil {
		return nextTransientRateLimitCooldown(0, retryAfter)
	}
	now := time.Now()
	acc.mu.Lock()
	until := acc.CooldownUtil
	sameWindow := acc.Status == StatusCooldown && until.After(now)
	if sameWindow {
		remaining := until.Sub(now)
		// A quota/auth window cannot be replaced or extended by a throttle.
		if !acc.isTransientRateLimitCooldownLocked() {
			acc.mu.Unlock()
			return remaining
		}
		acc.LastRateLimitedAt = now
		extension := now.Add(retryAfter)
		if retryAfter <= 0 || !extension.After(until) {
			acc.mu.Unlock()
			return remaining
		}
		until = extension
	} else {
		if accountDispatchBlocked(acc) || acc.Status == StatusError || acc.healthTierLocked() == HealthTierBanned {
			acc.mu.Unlock()
			return nextTransientRateLimitCooldown(0, retryAfter)
		}
		cooldown := nextTransientRateLimitCooldown(acc.transientRateLimitBackoff, retryAfter)
		until = now.Add(cooldown)
		// An upstream hint at the cap must not prevent this new window from
		// advancing the local ladder; hints and backoff are separate inputs.
		if nextTransientRateLimitCooldown(acc.transientRateLimitBackoff, 0) < TransientRateLimitBackoffMax {
			acc.transientRateLimitBackoff++
		}
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
	}
	// Publish all local fields in one critical section; concurrent failures
	// cannot observe the new backoff without the window that caused it.
	acc.LastRateLimitedAt = now
	acc.setCooldownUntilLocked(until, ResponsesRateLimitedCooldownReason)
	acc.transientRateLimitUntil = until
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.armTransientRateLimitRecoveryLocked(s)
	record := runtimeCooldownRecord{
		Kind: cache.CooldownKindTransient, Reason: ResponsesRateLimitedCooldownReason,
		ResetAt: until, UpdatedAt: now, BackoffLevel: acc.transientRateLimitBackoff,
	}
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	merged := s.cacheAccountCooldownRecord(acc.DBID, record)
	if merged.Kind != record.Kind || !merged.ResetAt.Equal(record.ResetAt) || merged.BackoffLevel != record.BackoffLevel {
		s.applyCachedAccountCooldown(acc, merged)
	}
	_, deadline := acc.GetCooldownSnapshot()
	return max(time.Until(deadline), 0)
}

func (a *Account) isTransientRateLimitCooldownLocked() bool {
	return a.Status == StatusCooldown && a.CooldownReason == ResponsesRateLimitedCooldownReason &&
		!a.transientRateLimitUntil.IsZero() && a.CooldownUtil.Equal(a.transientRateLimitUntil)
}

// Short freezes restore the local index directly, without a WHAM probe or a
// database write. Only active throttles allocate timers, at most one per
// account; changing the deadline cancels the previous one. The deadline and
// account identity checks fence callbacks that already started before a reset,
// removal, re-import or a stronger cooldown.
func (a *Account) armTransientRateLimitRecoveryLocked(s *Store) {
	if s == nil || s.backgroundCtx == nil || s.backgroundCtx.Err() != nil {
		return
	}
	if a.transientRateLimitTimer != nil {
		a.transientRateLimitTimer.Stop()
	}
	until := a.CooldownUtil
	a.transientRateLimitTimer = time.AfterFunc(max(time.Until(until), 0), func() {
		if s.backgroundCtx.Err() != nil {
			return
		}
		s.mu.RLock()
		current := s.lookupByIDLocked(a.DBID) == a
		s.mu.RUnlock()
		if !current {
			return
		}
		a.mu.Lock()
		if !a.isTransientRateLimitCooldownLocked() || !a.CooldownUtil.Equal(until) || time.Now().Before(until) {
			a.mu.Unlock()
			return
		}
		a.transientRateLimitTimer = nil
		a.recomputeSchedulerLocked(s.maxConcurrency.Load())
		a.mu.Unlock()
		s.fastSchedulerUpdate(a)
	})
}

// transientRateLimitRemainingLocked reports how long the current freeze still
// lasts when, and only when, the active cooldown is the one created by
// MarkTransientRateLimited and no quota window blocks the account underneath.
func (a *Account) transientRateLimitRemainingLocked(now time.Time) (time.Duration, bool) {
	return a.transientRateLimitRemainingForPolicyLocked(now, DispatchPolicyStandard)
}

func (a *Account) transientRateLimitRemainingForPolicyLocked(now time.Time, policy DispatchPolicy) (time.Duration, bool) {
	if !a.isTransientRateLimitCooldownLocked() || !a.CooldownUtil.After(now) {
		return 0, false
	}
	if policy == DispatchPolicySpark {
		if a.sparkUsageExhaustedLocked(now) {
			return 0, false
		}
	} else if a.usageWindowBlocksFreshDispatchLocked(now) {
		return 0, false
	}
	return a.CooldownUtil.Sub(now), true
}

// TransientRateLimitRemaining is the exported, locking form of
// transientRateLimitRemainingLocked.
func (a *Account) TransientRateLimitRemaining(now time.Time) (time.Duration, bool) {
	return a.transientRateLimitRemainingForPolicy(now, DispatchPolicyStandard)
}

func (a *Account) transientRateLimitRemainingForPolicy(now time.Time, policy DispatchPolicy) (time.Duration, bool) {
	if a == nil {
		return 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.transientRateLimitRemainingForPolicyLocked(now, policy)
}

func (a *Account) observeTransientRateLimitSuccessLocked(now time.Time) {
	if a == nil || a.transientRateLimitBackoff == 0 {
		return
	}
	if a.Status == StatusCooldown && a.CooldownUtil.After(now) {
		return
	}
	if a.LastRateLimitedAt.IsZero() || now.Sub(a.LastRateLimitedAt) < transientRateLimitStableReset {
		return
	}
	a.transientRateLimitBackoff = 0
}

// TransientRateLimitBackoff reports the in-memory throttle backoff exponent.
func (a *Account) TransientRateLimitBackoff() int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.transientRateLimitBackoff
}
