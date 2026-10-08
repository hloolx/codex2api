package proxy

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestSelectionCancellationStopsInitialScan(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		for _, cancelBefore := range []bool{true, false} {
			name := engine + "/during_scan"
			if cancelBefore {
				name = engine + "/before_scan"
			}
			t.Run(name, func(t *testing.T) {
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine})
				t.Cleanup(store.Stop)
				for id := int64(1); id <= 32; id++ {
					store.AddAccount(&auth.Account{DBID: id, AccessToken: "test-token", Status: auth.StatusReady})
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if cancelBefore {
					cancel()
				}
				calls := 0
				filter := func(*auth.Account) bool {
					calls++
					cancel()
					return false
				}
				h := &Handler{store: store}
				account, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), filter, false, auth.DispatchPolicyStandard)
				if account != nil {
					store.Release(account)
					t.Fatal("selected an account after cancellation")
				}
				wantCalls := 1
				if cancelBefore {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Errorf("expensive filter calls = %d, want %d", calls, wantCalls)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("selection error = %v, want context.Canceled", err)
				}
				if got := store.GetSchedulerMetrics().WaitStarted; got != 0 {
					t.Errorf("started %d queue waits after cancellation", got)
				}
			})
		}
	}
}

func TestSelectionBudgetIncludesInitialScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "legacy"})
		defer store.Stop()
		for id := int64(1); id <= 4; id++ {
			store.AddAccount(&auth.Account{DBID: id, AccessToken: "test-token", Status: auth.StatusReady})
		}
		calls := 0
		filter := func(*auth.Account) bool {
			calls++
			// A synchronous filter cannot be preempted, but after it returns
			// the expired budget must stop all subsequent expensive checks.
			time.Sleep(16 * time.Second)
			return false
		}
		h := &Handler{store: store}
		account, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, newRetryAccountExclusions(), filter, false, auth.DispatchPolicyStandard)
		if account != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
			t.Fatalf("selection returned account=%v, error=%v, filter calls=%d", account, err, calls)
		}
		if got := store.GetSchedulerMetrics().WaitStarted; got != 0 {
			t.Fatalf("expired initial scan started %d queue waits", got)
		}
	})
}

func TestSelectionSuccessPreservesRequestLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		defer store.Stop()
		account := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
		store.AddAccount(account)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h := &Handler{store: store}
		got, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), nil, false, auth.DispatchPolicyStandard)
		if err != nil || got != account {
			t.Fatalf("selection = %v, %v", got, err)
		}
		defer store.Release(got)
		time.Sleep(31 * time.Second)
		if ctx.Err() != nil || got.GetActiveRequests() != 1 {
			t.Fatalf("selection timeout affected upstream lifetime: context=%v, active=%d", ctx.Err(), got.GetActiveRequests())
		}
	})
}

func TestSelectionDeadlineStopsPoolWait(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	h := &Handler{store: store}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	account, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), nil, false, auth.DispatchPolicyStandard)
	if account != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("selection returned account=%v, error=%v", account, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pool wait ignored request deadline: %s", elapsed)
	}
	if got := store.GetSchedulerMetrics().Waiters; got != 0 {
		t.Fatalf("leaked %d queue waiters", got)
	}
}

// issue #719: when the request's own soft/transient exclusions cover every
// eligible account, the queue wait cannot make progress; selection must reset
// them instead of spending the whole selection budget waiting.
func TestSelectionResetsRequestExclusionsCoveringWholePool(t *testing.T) {
	marks := map[string]func(*retryAccountExclusions, int64){
		"transient": (*retryAccountExclusions).MarkTransient,
		"soft":      (*retryAccountExclusions).MarkSoft,
	}
	for _, engine := range []string{"legacy", "indexed"} {
		for kind, mark := range marks {
			t.Run(engine+"/"+kind, func(t *testing.T) {
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine})
				t.Cleanup(store.Stop)
				account := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
				store.AddAccount(account)
				exclusions := newRetryAccountExclusions()
				mark(exclusions, account.DBID)

				h := &Handler{store: store}
				start := time.Now()
				got, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
				if err != nil || got != account {
					t.Fatalf("selection = %v, %v; want the %s-excluded account after reset", got, err, kind)
				}
				store.Release(got)
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("selection took %s", elapsed)
				}
				metrics := store.GetSchedulerMetrics()
				if metrics.WaitStarted != 0 || metrics.WaitTimeouts != 0 {
					t.Fatalf("queue waits started=%d timeouts=%d, want 0", metrics.WaitStarted, metrics.WaitTimeouts)
				}
			})
		}
	}
}

func TestSelectionTransientResetKeepsHardExclusions(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine})
			t.Cleanup(store.Stop)
			hard := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
			transient := &auth.Account{DBID: 2, AccessToken: "test-token", Status: auth.StatusReady}
			store.AddAccount(hard)
			store.AddAccount(transient)
			exclusions := newRetryAccountExclusions()
			exclusions.MarkHard(hard.DBID)
			exclusions.MarkTransient(transient.DBID)

			h := &Handler{store: store}
			for i := 0; i < 3; i++ {
				got, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
				if err != nil || got != transient {
					t.Fatalf("round %d selection = %v, %v; want only the transient account", i, got, err)
				}
				store.Release(got)
				exclusions.MarkTransient(transient.DBID)
			}
		})
	}
}

func TestSelectionTransientResetKeepsAccountCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
	store.AddAccount(account)
	store.MarkCooldown(account, time.Hour, "rate_limited")
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(account.DBID)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	h := &Handler{store: store}
	got, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	if got != nil {
		store.Release(got)
		t.Fatal("transient reset bypassed the account cooldown")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("selection error = %v, want context.DeadlineExceeded", err)
	}
}

// A saturated but otherwise eligible account is real queue progress: the
// transient exclusion on another account must not short-circuit that wait.
func TestSelectionStillWaitsForSaturatedCandidate(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(store.Stop)
	busy := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
	failed := &auth.Account{DBID: 2, AccessToken: "test-token", Status: auth.StatusReady}
	store.AddAccount(busy)
	store.AddAccount(failed)
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(failed.DBID)

	h := &Handler{store: store}
	held, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	if err != nil || held != busy {
		t.Fatalf("setup selection = %v, %v", held, err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.Release(held)
		close(released)
	}()
	got, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	<-released
	if err != nil || got != busy {
		t.Fatalf("selection = %v, %v; want the released saturated account", got, err)
	}
	store.Release(got)
	if metrics := store.GetSchedulerMetrics(); metrics.WaitStarted != 1 {
		t.Fatalf("queue waits started = %d, want 1", metrics.WaitStarted)
	}
	if !exclusions.transient[failed.DBID] {
		t.Fatal("waiting for a saturated account cleared the transient exclusion")
	}
}
