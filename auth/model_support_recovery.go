package auth

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/codex2api/cache"
)

// ClearUnsupportedModelSince accepts a completed, fixed-account model test as
// evidence. It never releases quota, manual, quality, or newer failure gates.
func (s *Store) ClearUnsupportedModelSince(acc *Account, model string, startedAt time.Time) bool {
	if s == nil || acc == nil || startedAt.IsZero() {
		return false
	}
	key := normalizeModelCooldownKey(model)
	acc.mu.RLock()
	previous, ok := acc.ModelCooldowns[key]
	acc.mu.RUnlock()
	if !ok || previous.Reason != "model_not_supported" || previous.UpdatedAt.After(startedAt) {
		return false
	}
	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cleared, err := s.db.ClearUnsupportedModelSince(ctx, acc.DBID, key, startedAt, previous.ResetAt)
		cancel()
		if err != nil {
			log.Printf("[model-support] recovery account=%d model=%s: %v", acc.DBID, key, err)
			return false
		}
		if !cleared {
			return false
		}
	}
	acc.mu.Lock()
	current, ok := acc.ModelCooldowns[key]
	if !ok || current != previous {
		acc.mu.Unlock()
		return false
	}
	delete(acc.ModelCooldowns, key)
	acc.mu.Unlock()
	// Shared cache deletion is compare-and-delete: a concurrent failure on any
	// replica must survive this older success.
	if atomicCache, ok := s.tokenCache.(cache.RuntimeOwnerStore); ok {
		ctx, cancel := cooldownRuntimeContext()
		defer cancel()
		raw, found, err := s.tokenCache.GetRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, key))
		var record runtimeCooldownRecord
		if err == nil && found && json.Unmarshal(raw, &record) == nil && record.Reason == "model_not_supported" &&
			!record.UpdatedAt.After(startedAt) && !record.ResetAt.After(previous.ResetAt) {
			_, err = atomicCache.CompareAndDeleteRuntimeOwner(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, key), raw)
		}
		if err != nil {
			log.Printf("[model-support] cache recovery account=%d model=%s: %v", acc.DBID, key, err)
		}
	}
	s.fastSchedulerUpdate(acc)
	s.notifySchedulerAvailability()
	return true
}
