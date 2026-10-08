package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func parkTestWaiter(t *testing.T, h *availabilityHub, key, bound int64, exclude map[int64]bool) *availabilityWaiter {
	t.Helper()
	w, err := h.join(key, bound, exclude)
	if err != nil {
		t.Fatal(err)
	}
	h.finish(w, false, false, bound)
	t.Cleanup(func() { h.finish(w, false, true, 0) })
	return w
}

func takeTestWake(t *testing.T, w *availabilityWaiter) {
	t.Helper()
	select {
	case <-w.ready:
	case <-time.After(time.Second):
		t.Fatal("expected a capacity notification")
	}
}

func TestSchedulerQueueLimitsAndCancellationReclaim(t *testing.T) {
	h := newAvailabilityHub()
	defer h.stop()
	h.maxWaiters, h.maxWaitersPerKey = 3, 2
	a := parkTestWaiter(t, h, 1, 0, nil)
	parkTestWaiter(t, h, 1, 0, nil)
	parkTestWaiter(t, h, 2, 0, nil)
	if _, err := h.join(1, 0, nil); !errors.Is(err, ErrSchedulerKeyQueueFull) || !errors.Is(err, ErrSchedulerQueueFull) {
		t.Fatalf("per-key admission error = %v", err)
	}
	if _, err := h.join(3, 0, nil); !errors.Is(err, ErrSchedulerQueueFull) || errors.Is(err, ErrSchedulerKeyQueueFull) {
		t.Fatalf("global admission error = %v", err)
	}
	h.finish(a, false, true, 0)
	parkTestWaiter(t, h, 1, 0, nil)
	h.stop()
	if _, err := h.join(4, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped queue admission = %v", err)
	}
}

func TestSchedulerQueueRotatesKeysAndPreservesEligibleFIFO(t *testing.T) {
	h := newAvailabilityHub()
	defer h.stop()
	a := parkTestWaiter(t, h, 1, 0, nil)
	b := parkTestWaiter(t, h, 1, 0, nil)
	c := parkTestWaiter(t, h, 1, 0, nil)
	d := parkTestWaiter(t, h, 2, 0, nil)
	e := parkTestWaiter(t, h, 2, 0, nil)
	for _, want := range []*availabilityWaiter{a, d, b, e, c} {
		h.notifyAccount(10, false)
		takeTestWake(t, want)
		for _, other := range []*availabilityWaiter{a, b, c, d, e} {
			if len(other.ready) != 0 {
				t.Fatal("a single release woke multiple waiters")
			}
		}
		h.finish(want, true, true, 0)
	}
	if h.count != 0 || h.activeWaves != 0 || h.timer != nil || len(h.lanes) != 0 {
		t.Fatal("drained queue retained runtime state")
	}
}

func TestSchedulerQueueTargetingAndCanceledHandoff(t *testing.T) {
	h := newAvailabilityHub()
	defer h.stop()
	a := parkTestWaiter(t, h, 1, 11, nil)
	b := parkTestWaiter(t, h, 2, 22, nil)
	c := parkTestWaiter(t, h, 3, 0, map[int64]bool{22: true})
	d := parkTestWaiter(t, h, 4, 22, nil)
	h.notifyAccount(22, false)
	if len(a.ready) != 0 || len(c.ready) != 0 || len(b.ready) != 1 {
		t.Fatal("release woke an excluded account or an unrelated continuation")
	}
	// Cancellation wins the request's select before it consumes ready.
	h.finish(b, false, true, 0)
	takeTestWake(t, d)
	h.finish(d, true, true, 0)
	if h.activeWaves != 0 {
		t.Fatal("canceled recipient leaked a wake permit")
	}
}

func TestSchedulerQueueIncompatibleFiltersMakeOneFinitePass(t *testing.T) {
	h := newAvailabilityHub()
	defer h.stop()
	var waiters []*availabilityWaiter
	for _, key := range []int64{1, 1, 1, 1, 2} {
		waiters = append(waiters, parkTestWaiter(t, h, key, 0, nil))
	}
	h.notifyAccount(11, false)
	visited := make(map[*availabilityWaiter]bool)
	for i := 0; i < len(waiters); i++ {
		var selected *availabilityWaiter
		for _, w := range waiters {
			if len(w.ready) != 0 {
				selected = w
				break
			}
		}
		if selected == nil || visited[selected] {
			t.Fatalf("handoff skipped/repeated a waiter after %d attempts", i)
		}
		visited[selected] = true
		takeTestWake(t, selected)
		h.finish(selected, false, false, 0)
	}
	if h.activeWaves != 0 || h.pending || h.count != len(waiters) {
		t.Fatal("failed filtering must park all waiters without spinning")
	}
}

func TestSchedulerQueueRegistrationAndBurstDoNotLoseCapacity(t *testing.T) {
	h := newAvailabilityHub()
	defer h.stop()
	w, err := h.join(1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Release after the initial failed CAS, before the request parks.
	h.notifyAccount(11, false)
	h.finish(w, false, false, 0)
	takeTestWake(t, w)
	h.finish(w, true, true, 0)
	var waiters []*availabilityWaiter
	for i := 0; i < 40; i++ {
		waiters = append(waiters, parkTestWaiter(t, h, int64(i%4), 0, nil))
	}
	for i := 0; i < 40; i++ {
		h.notifyAccount(11, false)
	}
	if h.activeWaves != availabilityMaxConcurrentWakes || !h.pending {
		t.Fatal("burst must bound concurrent selectors and retain a trailing pass")
	}
	for completed := 0; completed < len(waiters); {
		progress := false
		for _, waiter := range waiters {
			if len(waiter.ready) == 0 {
				continue
			}
			takeTestWake(t, waiter)
			h.finish(waiter, true, true, 0)
			completed++
			progress = true
		}
		if !progress {
			t.Fatal("coalesced releases stranded available capacity")
		}
	}
	if h.activeWaves != 0 || h.count != 0 || h.pending {
		t.Fatal("burst leaked queue state")
	}
}

func newSchedulerWaitTestStore(t *testing.T, size int) *Store {
	t.Helper()
	s := (&Store{schedulerMetrics: newSchedulerRuntimeMetrics()}).withMaxConcurrency(1)
	for i := 0; i < size; i++ {
		s.accounts = append(s.accounts, newFastSchedulerTestAccount(int64(i+1), HealthTierHealthy, 90, 1))
	}
	s.rebuildAccountIndex()
	s.SetSchedulerEngine("indexed")
	t.Cleanup(s.Stop)
	return s
}

func waitForParkedRequests(t *testing.T, s *Store, count int) {
	t.Helper()
	h := s.schedulerAvailabilityHub()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		h.mu.Lock()
		ready := 0
		for _, lane := range h.lanes {
			ready += lane.ready.Len()
		}
		h.mu.Unlock()
		if ready == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d parked requests", count)
}

func TestSchedulerWaitContinuationAndFilterHandoff(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 2)
	a, b := s.Next(), s.Next()
	if a == nil || b == nil {
		t.Fatal("failed to occupy accounts")
	}
	s.BindSessionAffinity("continuation", a, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	boundResult, ordinaryResult := make(chan *Account, 1), make(chan *Account, 1)
	go func() {
		acc, _, _, _ := s.WaitForDispatchAvailable(ctx, "continuation", time.Second, 1, nil, nil, true, DispatchPolicyStandard)
		boundResult <- acc
	}()
	waitForParkedRequests(t, s, 1)
	go func() {
		acc, _, _, _ := s.WaitForDispatchAvailable(ctx, "", time.Second, 2, nil, func(acc *Account) bool {
			// Taking the queue lock here also checks that user filters run outside it.
			s.SetSchedulerWaitLimits(100, 50)
			return acc == b
		}, false, DispatchPolicyStandard)
		ordinaryResult <- acc
	}()
	waitForParkedRequests(t, s, 2)
	s.Release(b)
	select {
	case got := <-ordinaryResult:
		if got != b {
			t.Fatalf("ordinary waiter selected %p, want %p", got, b)
		}
		defer s.Release(got)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unrelated continuation blocked eligible request")
	}
	if len(boundResult) != 0 {
		t.Fatal("continuation migrated to another account")
	}
	s.Release(a)
	select {
	case got := <-boundResult:
		if got != a {
			t.Fatalf("continuation selected %p, want %p", got, a)
		}
		s.Release(got)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("bound release did not wake continuation")
	}
}

func TestSchedulerWaitCancellationAfterAdmissionReleasesSlot(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc, _, _, err := s.WaitForDispatchAvailable(ctx, "", time.Second, 0, nil, func(*Account) bool { cancel(); return true }, false, DispatchPolicyStandard)
	if acc != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("selection after cancellation = %v, %v", acc, err)
	}
	if active, occupied := s.accounts[0].GetActiveRequests(), s.accounts[0].OccupiedRequests.Load(); active != 0 || occupied != 0 {
		t.Fatalf("cancellation leaked active=%d occupied=%d", active, occupied)
	}
	metrics := s.GetSchedulerMetrics()
	if metrics.Waiters != 0 || metrics.WaitCanceled != 1 || metrics.WaitDurationBuckets["inf"] != 1 {
		t.Fatalf("cancellation metrics = %+v", metrics)
	}
}

func TestSchedulerWaitStopAndTimeoutDrainQueue(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "stop"}[stop], func(t *testing.T) {
			s := newSchedulerWaitTestStore(t, 0)
			done := make(chan error, 1)
			go func() {
				_, _, _, err := s.WaitForDispatchAvailable(context.Background(), "", 30*time.Millisecond, 1, nil, nil, false, DispatchPolicyStandard)
				done <- err
			}()
			waitForParkedRequests(t, s, 1)
			if stop {
				s.Stop()
			}
			select {
			case err := <-done:
				if stop && !errors.Is(err, context.Canceled) {
					t.Fatalf("stop error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("wait did not stop")
			}
			if metrics := s.GetSchedulerMetrics(); metrics.Waiters != 0 || !stop && metrics.WaitTimeouts != 1 {
				t.Fatalf("drain metrics = %+v", metrics)
			}
		})
	}
}

func TestSchedulerWaitConcurrentReleaseAndCancel(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 4)
	var held []*Account
	for a := s.Next(); a != nil; a = s.Next() {
		held = append(held, a)
	}
	const requests = 128
	var done sync.WaitGroup
	var canceled []context.CancelFunc
	var grants atomic.Int64
	for i := 0; i < requests; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if i%2 == 0 {
			canceled = append(canceled, cancel)
		}
		done.Add(1)
		go func(key int64) {
			defer done.Done()
			acc, _, _, _ := s.WaitForDispatchAvailable(ctx, "", 2*time.Second, key, nil, nil, false, DispatchPolicyStandard)
			if acc != nil {
				grants.Add(1)
				time.Sleep(100 * time.Microsecond)
				s.Release(acc)
			}
		}(int64(i % 4))
	}
	waitForParkedRequests(t, s, requests)
	for _, cancel := range canceled {
		cancel()
	}
	for _, acc := range held {
		s.Release(acc)
	}
	done.Wait()
	if grants.Load() != requests/2 {
		t.Fatalf("granted %d of %d uncanceled requests", grants.Load(), requests/2)
	}
	for _, acc := range s.accounts {
		if acc.GetActiveRequests() != 0 || acc.OccupiedRequests.Load() != 0 {
			t.Fatal("release/cancel race leaked a slot")
		}
	}
	m := s.GetSchedulerMetrics()
	if m.Waiters != 0 || m.WaitCanceled != requests/2 || m.WaitGranted != requests/2 {
		t.Fatalf("race metrics = %+v", m)
	}
}

func TestSchedulerWaitRecoveryTimerWakesBoundContinuation(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 2)
	s.backgroundCtx, s.backgroundCancel = context.WithCancel(context.Background())
	acc := s.accounts[0]
	s.BindSessionAffinity("recover", acc, "")
	s.MarkTransientRateLimited(acc, 0)
	result := make(chan *Account, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		a, _, _, _ := s.WaitForDispatchAvailable(ctx, "recover", time.Second, 1, nil, nil, true, DispatchPolicyStandard)
		result <- a
	}()
	waitForParkedRequests(t, s, 1)
	acc.mu.Lock()
	acc.CooldownUtil = time.Now().Add(30 * time.Millisecond)
	acc.transientRateLimitUntil = acc.CooldownUtil
	acc.armTransientRateLimitRecoveryLocked(s)
	acc.mu.Unlock()
	select {
	case got := <-result:
		if got != acc {
			t.Fatalf("recovery selected %p, want %p", got, acc)
		}
		s.Release(got)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timer recovery missed the bound waiter")
	}
}

func TestSchedulerWaitSharedRecheckRecoversWithoutNotification(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 1)
	acc := s.Next()
	if acc == nil {
		t.Fatal("failed to occupy account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan *Account, 1)
	go func() {
		a, _, _, _ := s.WaitForDispatchAvailable(ctx, "", 2*time.Second, 0, nil, nil, false, DispatchPolicyStandard)
		result <- a
	}()
	waitForParkedRequests(t, s, 1)
	// Simulate time-based eligibility recovery without an availability event.
	if !releaseOccupiedAccountSlot(acc) {
		t.Fatal("failed to free account")
	}
	select {
	case got := <-result:
		if got != acc {
			t.Fatal("shared recovery check missed available account")
		}
		s.Release(got)
	case <-ctx.Done():
		t.Fatal("shared recovery check did not run")
	}
	h := s.schedulerAvailabilityHub()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count != 0 || h.timer != nil {
		t.Fatal("empty queue retained its recovery timer")
	}
}

type schedulerWaitBindingCache struct {
	cache.TokenCache
	reads atomic.Int64
}

func (c *schedulerWaitBindingCache) GetSessionAffinity(ctx context.Context, key string) (cache.SessionAffinityBinding, bool, error) {
	c.reads.Add(1)
	return c.TokenCache.GetSessionAffinity(ctx, key)
}

func TestSchedulerWaitHintDoesNotAddRemoteBindingReads(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 1)
	held := s.Next()
	if held == nil {
		t.Fatal("failed to occupy account")
	}
	defer s.Release(held)
	tc := &schedulerWaitBindingCache{TokenCache: cache.NewMemory(1)}
	defer tc.Close()
	s.tokenCache = tc
	if err := tc.SetSessionAffinity(context.Background(), "cached-owner", cache.SessionAffinityBinding{AccountID: held.ID()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	acc, _, _, _ := s.WaitForDispatchAvailable(context.Background(), "cached-owner", 30*time.Millisecond, 0, nil, nil, true, DispatchPolicyStandard)
	if acc != nil {
		s.Release(acc)
		t.Fatal("busy cached owner unexpectedly acquired")
	}
	if tc.reads.Load() != 1 {
		t.Fatalf("wait hint added binding reads: got %d, want one selection read", tc.reads.Load())
	}
}
