package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestUnsupportedModelRecoveryPreservesOtherGates(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	s := NewStore(nil, tc, nil)
	a := &Account{DBID: 1, AccessToken: "fixture", Status: StatusReady}
	s.AddAccount(a)
	old := time.Now().Add(-time.Minute)
	for model, reason := range map[string]string{"target": "model_not_supported", "other": "model_not_supported", "quota": "rate_limited_model"} {
		a.RestoreModelCooldown(model, reason, old.Add(time.Hour), old)
		s.setCachedModelCooldown(a.DBID, a.ModelCooldowns[model])
	}
	s.ApplyModelQualitySnapshot(database.ModelQualityConfig{Enabled: true, Models: []string{"target"}}, []database.ModelQualityState{{AccountID: 1, Model: "target", Status: "fail"}})
	a.Disabled = 1
	a.DispatchPaused = 1
	if !s.ClearUnsupportedModelSince(a, "TARGET", old.Add(time.Second)) {
		t.Fatal("completed test did not clear stale model rejection")
	}
	if a.IsModelRateLimited("target") || !a.IsModelRateLimited("other") || !a.IsModelRateLimited("quota") ||
		!a.IsModelQualityBlocked("target") || a.Disabled != 1 || a.DispatchPaused != 1 {
		t.Fatal("recovery changed unrelated restrictions")
	}
	if _, found, _ := tc.GetRuntime(context.Background(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(1, "target")); found {
		t.Fatal("stale shared cache still blocks recovered model")
	}
	if s.ClearUnsupportedModelSince(a, "quota", time.Now()) || s.ClearUnsupportedModelSince(a, "other", old.Add(-time.Second)) {
		t.Fatal("quota or newer unsupported evidence was cleared")
	}
}

type replacingCooldownCache struct {
	*cache.MemoryTokenCache
	replacement json.RawMessage
}

func (c *replacingCooldownCache) CompareAndDeleteRuntimeOwner(ctx context.Context, ns, key string, expected []byte) (bool, error) {
	if err := c.SetRuntime(ctx, ns, key, c.replacement, time.Hour); err != nil {
		return false, err
	}
	return c.MemoryTokenCache.CompareAndDeleteRuntimeOwner(ctx, ns, key, expected)
}

func TestUnsupportedModelRecoveryFencesSharedCacheRace(t *testing.T) {
	old := time.Now().Add(-time.Minute)
	newer := runtimeCooldownRecord{Model: "target", Reason: "rate_limited_model", ResetAt: old.Add(2 * time.Hour), UpdatedAt: old.Add(2 * time.Minute)}
	raw, _ := json.Marshal(newer)
	tc := &replacingCooldownCache{MemoryTokenCache: cache.NewMemory(4).(*cache.MemoryTokenCache), replacement: raw}
	t.Cleanup(func() { _ = tc.Close() })
	s := NewStore(nil, tc, nil)
	a := &Account{DBID: 1, AccessToken: "fixture", Status: StatusReady}
	s.AddAccount(a)
	a.RestoreModelCooldown("target", "model_not_supported", old.Add(time.Hour), old)
	s.setCachedModelCooldown(1, a.ModelCooldowns["target"])
	if !s.ClearUnsupportedModelSince(a, "target", old.Add(time.Second)) {
		t.Fatal("local recovery failed")
	}
	if cached, found := s.getCachedModelCooldown(1, "target"); !found || cached.Reason != "rate_limited_model" {
		t.Fatal("concurrent replica cooldown was erased")
	}
}
