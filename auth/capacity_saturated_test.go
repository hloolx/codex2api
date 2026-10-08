package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestCapacitySaturatedCandidateSummary(t *testing.T) {
	newAccount := func(id int64) *Account {
		return &Account{DBID: id, AccessToken: "token", Status: StatusReady, PlanType: "plus", AccountID: "acct"}
	}
	occupy := func(acc *Account) {
		acc.ActiveRequests.Store(1)
		acc.OccupiedRequests.Store(1)
	}

	t.Run("empty", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		if store.CapacitySaturatedCandidateSummary(0, nil, nil, DispatchPolicyStandard).Found {
			t.Fatal("empty pool reported concurrency saturation")
		}
	})

	t.Run("free slot", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		store.AddAccount(newAccount(1))
		if store.CapacitySaturatedCandidateSummary(0, nil, nil, DispatchPolicyStandard).Found {
			t.Fatal("an idle account must not be reported as saturated")
		}
	})

	t.Run("all matching accounts occupied", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		busy := newAccount(1)
		store.AddAccount(busy)
		occupy(busy)
		if !store.CapacitySaturatedCandidateSummary(0, nil, nil, DispatchPolicyStandard).Found {
			t.Fatal("occupied matching account was not reported as saturated")
		}
	})

	t.Run("one free account keeps the pool unsaturated", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		busy := newAccount(1)
		idle := newAccount(2)
		store.AddAccount(busy)
		store.AddAccount(idle)
		occupy(busy)
		if store.CapacitySaturatedCandidateSummary(0, nil, nil, DispatchPolicyStandard).Found {
			t.Fatal("a free matching account must keep the pool unsaturated")
		}
	})

	t.Run("cooldown is not a concurrency miss", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		cooling := newAccount(1)
		store.AddAccount(cooling)
		occupy(cooling)
		store.MarkCooldown(cooling, time.Hour, "rate_limited")
		if store.CapacitySaturatedCandidateSummary(0, nil, nil, DispatchPolicyStandard).Found {
			t.Fatal("a cooling account was reported as concurrency saturation")
		}
	})

	t.Run("excluded or filtered accounts do not count", func(t *testing.T) {
		store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		busy := newAccount(1)
		store.AddAccount(busy)
		occupy(busy)
		if store.CapacitySaturatedCandidateSummary(0, map[int64]bool{1: true}, nil, DispatchPolicyStandard).Found {
			t.Fatal("an excluded account was reported as saturation")
		}
		if store.CapacitySaturatedCandidateSummary(0, nil, func(*Account) bool { return false }, DispatchPolicyStandard).Found {
			t.Fatal("a filtered-out account was reported as saturation")
		}
	})
}
