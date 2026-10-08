package auth

import (
	"maps"
	"strings"
	"sync"

	"github.com/codex2api/database"
)

type modelQualityGate struct {
	mu      sync.RWMutex
	blocked map[int64]map[string]int64
}

// Reserve normal account capacity, while deliberately bypassing only the quality gate.
func (s *Store) AcquireModelQualityProbe(a *Account) bool {
	if a == nil || !a.IsAvailable() {
		return false
	}
	a.mu.Lock()
	a.recomputeSchedulerLocked(s.maxConcurrency.Load())
	limit := a.DynamicConcurrencyLimit
	a.mu.Unlock()
	return s.tryAcquireAccount(a, limit, true)
}

// ModelQualityCapacity returns the same account capacity used by probe admission.
func (s *Store) ModelQualityCapacity(a *Account) (occupied, limit int64) {
	if a == nil {
		return 0, 0
	}
	a.mu.Lock()
	a.recomputeSchedulerLocked(s.maxConcurrency.Load())
	limit = a.DynamicConcurrencyLimit
	a.mu.Unlock()
	return a.OccupiedRequests.Load(), limit
}

func (s *Store) ApplyModelQualitySnapshot(cfg database.ModelQualityConfig, states []database.ModelQualityState) {
	blocked := make(map[int64]map[string]int64)
	selected := make(map[string]bool)
	for _, model := range cfg.Models {
		selected[strings.ToLower(model)] = true
	}
	if cfg.Enabled {
		for _, state := range states {
			model := strings.ToLower(state.Model)
			if state.Status != "fail" || !selected[model] {
				continue
			}
			if blocked[state.AccountID] == nil {
				blocked[state.AccountID] = make(map[string]int64)
			}
			blocked[state.AccountID][model] = state.Generation
		}
	}
	s.modelQuality.mu.Lock()
	unchanged := len(s.modelQuality.blocked) == len(blocked)
	if unchanged {
		for id, models := range blocked {
			if !maps.Equal(models, s.modelQuality.blocked[id]) {
				unchanged = false
				break
			}
		}
	}
	if unchanged {
		s.modelQuality.mu.Unlock()
		return
	}
	s.modelQuality.blocked = blocked
	s.modelQuality.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.notifySchedulerAvailability()
}

// A quality verdict is independent of quota cooldowns and account availability.
func (a *Account) IsModelQualityBlocked(model string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	gate, id := a.modelQuality, a.DBID
	a.mu.RUnlock()
	if gate == nil {
		return false
	}
	gate.mu.RLock()
	_, blocked := gate.blocked[id][strings.ToLower(strings.TrimSpace(model))]
	gate.mu.RUnlock()
	return blocked
}
