package auth

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

type schedulerCountingCache struct {
	cache.TokenCache
	modelReads atomic.Int64
}

func (c *schedulerCountingCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	if namespace == modelCooldownCacheNamespace {
		c.modelReads.Add(1)
	}
	return c.TokenCache.GetRuntime(ctx, namespace, key)
}

func TestIndexedSelectionBoundsModelCacheReads(t *testing.T) {
	tokenCache := &schedulerCountingCache{TokenCache: cache.NewMemory(1)}
	defer tokenCache.Close()
	s := (&Store{tokenCache: tokenCache, schedulerMetrics: newSchedulerRuntimeMetrics()}).withMaxConcurrency(4)
	for id := int64(1); id <= 1000; id++ {
		s.accounts = append(s.accounts, newFastSchedulerTestAccount(id, HealthTierHealthy, 100, 4))
	}
	s.rebuildAccountIndex()
	s.SetSchedulerEngine("indexed")
	got := s.NextExcludingWithFilter(0, nil, s.WithModelCooldownFilter("gpt-5.4", nil))
	if got == nil {
		t.Fatal("no account selected")
	}
	s.Release(got)
	if reads := tokenCache.modelReads.Load(); reads != 1 {
		t.Fatalf("idle pool required %d model-cache reads, want 1", reads)
	}
	metrics := s.GetSchedulerMetrics()
	if metrics.FastScannedAccounts == 0 || metrics.FastScannedAccounts > fastSchedulerCandidateWindow || metrics.FastFilterChecks != 1 || metrics.ModelCooldownCacheReads != 1 {
		t.Fatalf("selection cost metrics do not reflect bounded work: %+v", metrics)
	}
	if metrics.SelectionDurationBuckets["+Inf"] != 1 {
		t.Fatalf("selection latency histogram lost an observation: %+v", metrics.SelectionDurationBuckets)
	}
}

func TestSchedulerAdmissionRejectsDisabledAccount(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
			s := (&Store{accounts: []*Account{acc}}).withMaxConcurrency(4)
			s.rebuildAccountIndex()
			s.SetSchedulerEngine(engine)
			atomic.StoreInt32(&acc.Disabled, 1)
			if got := s.Next(); got != nil {
				s.Release(got)
				t.Fatal("disabled account was selected")
			}
		})
	}
}

func TestAccountSlotRechecksDispatchFlags(t *testing.T) {
	for _, pause := range []bool{false, true} {
		acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
		if pause {
			atomic.StoreInt32(&acc.DispatchPaused, 1)
		} else {
			atomic.StoreInt32(&acc.Disabled, 1)
		}
		if reserveOccupiedAccountSlot(acc, 4) {
			t.Fatalf("blocked account reserved a slot (paused=%v)", pause)
		}
		if acc.GetOccupiedRequests() != 0 || acc.GetActiveRequests() != 0 {
			t.Fatal("rejected admission leaked a slot")
		}
	}
}

func TestFastSchedulerFilterDoesNotBlockUpdates(t *testing.T) {
	a := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	b := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 4)
	s := NewFastScheduler(4, "round_robin")
	s.Rebuild([]*Account{a, b})
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	result := make(chan *Account, 1)
	go func() {
		result <- s.AcquireExcludingWithFilter(0, nil, func(acc *Account) bool {
			if acc == a {
				once.Do(func() { close(entered) })
				<-resume
			}
			return true
		})
	}()
	<-entered
	atomic.StoreInt32(&a.Disabled, 1)
	updated := make(chan struct{})
	go func() { s.Update(a); close(updated) }()
	select {
	case <-updated:
	case <-time.After(time.Second):
		close(resume)
		if got := <-result; got != nil {
			s.Release(got)
		}
		<-updated
		t.Fatal("filter held the scheduler lock during a state update")
	}
	close(resume)
	if got := <-result; got != b {
		if got != nil {
			s.Release(got)
		}
		t.Fatal("selection did not recheck a disabled candidate after filtering")
	}
	s.Release(b)
}

func TestFastSchedulerContinuesPastRejectedCandidateWindow(t *testing.T) {
	s := NewFastScheduler(4, "remaining_quota")
	var accounts []*Account
	for id := int64(1); id <= 40; id++ {
		accounts = append(accounts, newFastSchedulerTestAccount(id, HealthTierHealthy, 100, 4))
	}
	s.Rebuild(accounts)
	got := s.AcquireExcludingWithFilter(0, nil, func(acc *Account) bool { return acc.DBID == 40 })
	if got == nil || got.DBID != 40 {
		t.Fatal("a rejected candidate window hid an eligible account")
	}
	s.Release(got)
}

func TestFastSchedulerRechecksTierAfterFilter(t *testing.T) {
	a := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	b := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 4)
	s := NewFastScheduler(4, "round_robin")
	s.Rebuild([]*Account{a, b})
	got := s.AcquireExcludingWithFilter(0, nil, func(acc *Account) bool {
		if acc == a {
			acc.mu.Lock()
			acc.HealthTier = HealthTierWarm
			acc.mu.Unlock()
		}
		return true
	})
	if got != b {
		t.Fatal("a newly degraded candidate bypassed the healthy tier")
	}
	s.Release(got)
}
