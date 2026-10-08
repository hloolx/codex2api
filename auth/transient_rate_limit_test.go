package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

type delayedTransientCooldownCache struct {
	cache.TokenCache
	entered, resume chan struct{}
}

func (c *delayedTransientCooldownCache) MergeRuntimeCooldown(ctx context.Context, namespace, key string, record cache.RuntimeCooldown) (cache.RuntimeCooldown, error) {
	if record.Kind == cache.CooldownKindTransient {
		close(c.entered)
		<-c.resume
	}
	return c.TokenCache.(cache.RuntimeCooldownMerger).MergeRuntimeCooldown(ctx, namespace, key, record)
}

func TestTransientRateLimitDelayedPublicationCannotReplaceQuota(t *testing.T) {
	c := &delayedTransientCooldownCache{TokenCache: cache.NewMemory(1), entered: make(chan struct{}), resume: make(chan struct{})}
	defer c.Close()
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s := (&Store{accounts: []*Account{acc}, tokenCache: c}).withMaxConcurrency(4)
	done := make(chan struct{})
	go func() { s.MarkTransientRateLimited(acc, 0); close(done) }()
	<-c.entered
	s.MarkCooldown(acc, time.Hour, "usage_limit")
	close(c.resume)
	<-done
	if acc.GetCooldownReason() != "usage_limit" {
		t.Fatal("late local throttle replaced quota")
	}
	record, ok := s.getCachedAccountCooldown(acc.DBID)
	if !ok || record.Reason != "usage_limit" || record.Kind == cache.CooldownKindTransient {
		t.Fatalf("late shared throttle replaced quota: %+v", record)
	}
}

func TestTransientRateLimitExpiryRestoresIndexWithoutProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	other := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 4)
	s := (&Store{accounts: []*Account{acc, other}, backgroundCtx: ctx}).withMaxConcurrency(4)
	s.rebuildAccountIndex()
	s.SetSchedulerEngine("indexed")
	s.MarkTransientRateLimited(acc, 0)
	if acc.SparkDispatchEligible() {
		t.Fatal("account-wide throttle allowed Spark dispatch")
	}
	scheduler := s.getFastScheduler()
	scheduler.mu.RLock()
	_, present := scheduler.positions[acc.DBID]
	scheduler.mu.RUnlock()
	if present {
		t.Fatal("throttled account was not removed before recovery")
	}
	// Use a short deadline to exercise the same recovery callback without a
	// 15-second unit test. The other ready account prevents miss repair.
	acc.mu.Lock()
	acc.CooldownUtil = time.Now().Add(30 * time.Millisecond)
	acc.transientRateLimitUntil = acc.CooldownUtil
	acc.armTransientRateLimitRecoveryLocked(s)
	acc.mu.Unlock()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		scheduler := s.getFastScheduler()
		scheduler.mu.RLock()
		_, exists := scheduler.positions[acc.DBID]
		scheduler.mu.RUnlock()
		if exists && acc.IsAvailable() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("expired transient account was not restored to the index")
		}
	}
}

func TestTransientRateLimitConcurrentWindowOnlyEscalatesOnce(t *testing.T) {
	for trial := 0; trial < 100; trial++ {
		s := (&Store{}).withMaxConcurrency(4)
		acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; s.MarkTransientRateLimited(acc, 0) }()
		}
		close(start)
		wg.Wait()
		if level := acc.TransientRateLimitBackoff(); level != 1 {
			t.Fatalf("trial %d: first window escalated to %d, want 1", trial, level)
		}
	}
}

func TestTransientRateLimitCappedHintStillAdvancesBackoff(t *testing.T) {
	s := (&Store{}).withMaxConcurrency(4)
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s.MarkTransientRateLimited(acc, TransientRateLimitBackoffMax)
	if got := acc.TransientRateLimitBackoff(); got != 1 {
		t.Fatalf("capped hint left backoff at %d, want 1", got)
	}
	acc.mu.Lock()
	acc.CooldownUtil = time.Now().Add(-time.Second)
	acc.mu.Unlock()
	if got := s.MarkTransientRateLimited(acc, 0); got < 29*time.Second || got > 30*time.Second {
		t.Fatalf("next window = %v, want 30s", got)
	}
}

func TestTransientRateLimitSummaryRespectsSparkWindow(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	acc.PlanType = "pro"
	acc.UsagePercent5hValid, acc.UsagePercent5h = true, 100
	acc.Reset5hAt = time.Now().Add(time.Hour)
	s := (&Store{accounts: []*Account{acc}}).withMaxConcurrency(4)
	s.MarkTransientRateLimited(acc, 0)
	if got := s.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicySpark); !got.TransientOnly {
		t.Fatalf("main quota incorrectly classified Spark throttle: %+v", got)
	}
	if got := s.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicyStandard); got.TransientOnly {
		t.Fatal("main-model exhaustion was classified as transient")
	}
	if got := s.MarkTransientRateLimited(acc, time.Minute); got < 59*time.Second {
		t.Fatal("main usage snapshot prevented extending the transient window")
	}
	acc.mu.Lock()
	acc.UsagePercentSparkValid, acc.UsagePercentSpark = true, 100
	acc.ResetSparkAt = time.Now().Add(time.Hour)
	acc.mu.Unlock()
	if got := s.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicySpark); !got.Found || got.TransientOnly || got.RetryAfter != 0 {
		t.Fatalf("Spark quota exhaustion was classified as transient: %+v", got)
	}
}

func TestTransientRateLimitExtendsWindowForLaterRetryAfter(t *testing.T) {
	s := (&Store{}).withMaxConcurrency(4)
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s.MarkTransientRateLimited(acc, 0)
	if got := s.MarkTransientRateLimited(acc, 2*time.Minute); got < 119*time.Second {
		t.Fatalf("later Retry-After was lost: %v", got)
	}
	if got := acc.TransientRateLimitBackoff(); got != 1 {
		t.Fatalf("extending the same window escalated to %d", got)
	}
}

func TestTransientRateLimitCachePreservesClassification(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	a := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	b := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s1 := (&Store{accounts: []*Account{a}, tokenCache: tokenCache}).withMaxConcurrency(4)
	s2 := (&Store{accounts: []*Account{b}, tokenCache: tokenCache}).withMaxConcurrency(4)
	s1.MarkTransientRateLimited(a, 0)
	if !s2.accountHasCachedCooldown(b) {
		t.Fatal("shared cooldown missing")
	}
	for _, s := range []*Store{s1, s2} {
		got := s.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicyStandard)
		if !got.TransientOnly || got.RetryAfter <= 0 {
			t.Fatalf("transient classification lost: %+v", got)
		}
	}
	if b.TransientRateLimitBackoff() != a.TransientRateLimitBackoff() {
		t.Fatal("cache did not preserve the backoff level")
	}
}

func TestTransientRateLimitDoesNotArmUsageProbe(t *testing.T) {
	s := (&Store{boundaryProbeWakeCh: make(chan struct{}, 1)}).withMaxConcurrency(4)
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	s.MarkTransientRateLimited(acc, 0)
	if _, ok := acc.nextProbeBoundary(time.Now()); ok || len(s.boundaryProbeWakeCh) != 0 {
		t.Fatal("transient-only cooldown armed a WHAM probe")
	}
	acc.mu.Lock()
	acc.UsagePercent7dValid = true
	acc.Reset7dAt = time.Now().Add(time.Hour)
	acc.mu.Unlock()
	if got, ok := acc.nextProbeBoundary(time.Now()); !ok || !got.Equal(acc.Reset7dAt) {
		t.Fatal("a real quota boundary was suppressed by transient cooldown")
	}
}

func newTransientRateLimitTestStore() *Store {
	return NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:  4,
		TestConcurrency: 1,
		TestModel:       "gpt-5.4",
	})
}

func TestMarkTransientRateLimitedProgressiveBackoff(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 1, AccessToken: "token", Status: StatusReady}

	first := store.MarkTransientRateLimited(acc, 0)
	if first < 14*time.Second || first > 16*time.Second {
		t.Fatalf("first cooldown = %v, want about 15s", first)
	}
	if got := acc.TransientRateLimitBackoff(); got != 1 {
		t.Fatalf("backoff after first = %d, want 1", got)
	}

	second := store.MarkTransientRateLimited(acc, 0)
	if second > first {
		t.Fatalf("same-window second cooldown = %v, want reuse of first window %v", second, first)
	}
	if got := acc.TransientRateLimitBackoff(); got != 1 {
		t.Fatalf("backoff after same-window repeat = %d, want 1", got)
	}

	acc.mu.Lock()
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.mu.Unlock()

	third := store.MarkTransientRateLimited(acc, 0)
	if third < 28*time.Second || third > 32*time.Second {
		t.Fatalf("escalated cooldown = %v, want about 30s", third)
	}
	if got := acc.TransientRateLimitBackoff(); got != 2 {
		t.Fatalf("backoff after escalate = %d, want 2", got)
	}
}

func TestMarkTransientRateLimitedRespectsRetryAfter(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 2, AccessToken: "token", Status: StatusReady}

	got := store.MarkTransientRateLimited(acc, 45*time.Second)
	if got < 44*time.Second || got > 46*time.Second {
		t.Fatalf("cooldown = %v, want about 45s Retry-After", got)
	}
}

func TestMarkTransientRateLimitedDoesNotShortenQuotaCooldown(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 3, AccessToken: "token", Status: StatusReady}
	store.MarkCooldown(acc, 2*time.Hour, "usage_limit")

	got := store.MarkTransientRateLimited(acc, 0)
	if got < time.Hour {
		t.Fatalf("cooldown = %v, want existing quota window preserved", got)
	}
	if acc.GetCooldownReason() != "usage_limit" {
		t.Fatalf("reason = %q, want usage_limit", acc.GetCooldownReason())
	}
}

func TestReportRequestSuccessResetsTransientBackoffAfterStableWindow(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 4, AccessToken: "token", Status: StatusReady}
	store.MarkTransientRateLimited(acc, 0)

	acc.mu.Lock()
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastRateLimitedAt = time.Now().Add(-transientRateLimitStableReset - time.Second)
	acc.mu.Unlock()

	store.ReportRequestSuccess(acc, 20*time.Millisecond)
	if got := acc.TransientRateLimitBackoff(); got != 0 {
		t.Fatalf("backoff after stable success = %d, want 0", got)
	}
}

func TestReportRequestSuccessKeepsBackoffDuringStableWindow(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 5, AccessToken: "token", Status: StatusReady}
	store.MarkTransientRateLimited(acc, 0)

	acc.mu.Lock()
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastRateLimitedAt = time.Now().Add(-time.Minute)
	acc.mu.Unlock()

	store.ReportRequestSuccess(acc, 20*time.Millisecond)
	if got := acc.TransientRateLimitBackoff(); got != 1 {
		t.Fatalf("backoff after early success = %d, want 1", got)
	}
}

func TestClearCooldownResetsTransientBackoff(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 6, AccessToken: "token", Status: StatusReady}
	store.MarkTransientRateLimited(acc, 0)
	store.ClearCooldown(acc)
	if got := acc.TransientRateLimitBackoff(); got != 0 {
		t.Fatalf("backoff after ClearCooldown = %d, want 0", got)
	}
}

func TestUsageLimitedCandidateSummaryTransientOnly(t *testing.T) {
	store := newTransientRateLimitTestStore()
	fast := &Account{DBID: 11, AccessToken: "token", Status: StatusReady}
	slow := &Account{DBID: 12, AccessToken: "token", Status: StatusReady}
	store.AddAccount(fast)
	store.AddAccount(slow)

	store.MarkTransientRateLimited(fast, 0)
	store.MarkTransientRateLimited(slow, 90*time.Second)

	got := store.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicyStandard)
	if !got.Found || !got.TransientOnly {
		t.Fatalf("summary = %#v, want Found && TransientOnly", got)
	}
	if got.RetryAfter < 10*time.Second || got.RetryAfter > 16*time.Second {
		t.Fatalf("RetryAfter = %v, want the shortest freeze (about 15s)", got.RetryAfter)
	}
	if _, ok := fast.TransientRateLimitRemaining(time.Now()); !ok {
		t.Fatal("fast account should report an active transient freeze")
	}
}

func TestUsageLimitedCandidateSummaryQuotaWins(t *testing.T) {
	store := newTransientRateLimitTestStore()
	throttled := &Account{DBID: 21, AccessToken: "token", Status: StatusReady}
	exhausted := &Account{DBID: 22, AccessToken: "token", Status: StatusReady}
	store.AddAccount(throttled)
	store.AddAccount(exhausted)

	store.MarkTransientRateLimited(throttled, 0)
	store.MarkCooldown(exhausted, 2*time.Hour, "usage_limit")

	got := store.UsageLimitedCandidateSummary(0, nil, nil, DispatchPolicyStandard)
	if !got.Found || got.TransientOnly {
		t.Fatalf("summary = %#v, want Found && !TransientOnly", got)
	}
	if got.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 when a quota window is exhausted", got.RetryAfter)
	}
}

func TestTransientRateLimitRemainingIgnoresQuotaCooldown(t *testing.T) {
	store := newTransientRateLimitTestStore()
	acc := &Account{DBID: 31, AccessToken: "token", Status: StatusReady}
	store.MarkTransientRateLimited(acc, 0)
	// A later, longer quota cooldown replaces the transient deadline.
	store.MarkCooldown(acc, time.Hour, "usage_limit")
	if _, ok := acc.TransientRateLimitRemaining(time.Now()); ok {
		t.Fatal("quota cooldown must not be reported as transient")
	}
	// The same reason with a different deadline (real usage_limit via
	// MarkResponsesRateLimited) is not transient either.
	acc2 := &Account{DBID: 32, AccessToken: "token", Status: StatusReady}
	store.MarkResponsesRateLimited(acc2, time.Hour)
	if _, ok := acc2.TransientRateLimitRemaining(time.Now()); ok {
		t.Fatal("MarkResponsesRateLimited cooldown must not be reported as transient")
	}
}
