package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

type cancellationTestCache struct {
	cache.TokenCache
	entered     chan struct{}
	reads       atomic.Int64
	readContext context.Context
}

func (c *cancellationTestCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	if c.reads.Add(1) == 1 {
		c.readContext = ctx
		close(c.entered)
	}
	<-ctx.Done()
	return nil, false, ctx.Err()
}

func TestModelCooldownFilterCancelsInFlightCacheRead(t *testing.T) {
	tc := &cancellationTestCache{TokenCache: cache.NewMemory(1), entered: make(chan struct{})}
	defer tc.Close()
	store := &Store{tokenCache: tc}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	filter := store.WithModelCooldownFilterContext(ctx, "grok-4.6", nil)
	done := make(chan bool, 1)
	go func() { done <- filter(&Account{DBID: 1}) }()
	select {
	case <-tc.entered:
	case <-time.After(time.Second):
		t.Fatal("cache read was not started")
	}
	cancel()
	if err := tc.readContext.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cache read context = %v, want cancellation from the caller", err)
	}
	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("canceled cache read admitted an upstream candidate")
		}
	case <-time.After(time.Second):
		t.Fatal("cache read did not inherit downstream cancellation")
	}
	for id := int64(2); id <= 1000; id++ {
		if filter(&Account{DBID: id}) {
			t.Fatal("canceled request admitted another candidate")
		}
	}
	if got := tc.reads.Load(); got != 1 {
		t.Fatalf("cache reads after cancellation: %d, want 1", got)
	}
}

type cancellationCleanupTestCache struct {
	cache.TokenCache
	payload       json.RawMessage
	entered       chan struct{}
	deleteContext context.Context
}

func (c *cancellationCleanupTestCache) GetRuntime(context.Context, string, string) (json.RawMessage, bool, error) {
	return c.payload, true, nil
}

func (c *cancellationCleanupTestCache) DeleteRuntime(ctx context.Context, namespace, key string) error {
	c.deleteContext = ctx
	close(c.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestModelCooldownFilterCancelsCacheCleanup(t *testing.T) {
	for _, payload := range []string{`invalid-json`, `{"reset_at":"2000-01-01T00:00:00Z"}`} {
		t.Run(payload, func(t *testing.T) {
			tc := &cancellationCleanupTestCache{TokenCache: cache.NewMemory(1), payload: json.RawMessage(payload), entered: make(chan struct{})}
			defer tc.Close()
			store := &Store{tokenCache: tc}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan bool, 1)
			go func() { done <- store.WithModelCooldownFilterContext(ctx, "grok-4.6", nil)(&Account{DBID: 1}) }()
			select {
			case <-tc.entered:
			case <-time.After(time.Second):
				t.Fatal("cache cleanup was not started")
			}
			cancel()
			if err := tc.deleteContext.Err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cleanup context = %v, want cancellation from the caller", err)
			}
			select {
			case allowed := <-done:
				if allowed {
					t.Fatal("canceled cache cleanup admitted an account")
				}
			case <-time.After(time.Second):
				t.Fatal("cache cleanup did not inherit request cancellation")
			}
		})
	}
}

func TestModelCooldownFilterRejectsCanceledRequestBeforeOtherFilters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &Store{}
	calls := 0
	filter := store.WithModelCooldownFilterContext(ctx, "", func(*Account) bool {
		calls++
		return true
	})
	if filter(&Account{DBID: 1}) || calls != 0 {
		t.Fatalf("canceled request reached the underlying filter %d times", calls)
	}
}

func TestModelCooldownContextPreservesRemoteGate(t *testing.T) {
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := &Store{tokenCache: tc}
	account := &Account{DBID: 1}
	store.setCachedModelCooldown(1, ModelCooldown{
		Model: "grok-4.6", Reason: "rate_limited",
		ResetAt: time.Now().Add(time.Minute), UpdatedAt: time.Now(),
	})
	if store.WithModelCooldownFilterContext(context.Background(), "grok-4.6", nil)(account) {
		t.Fatal("remote model cooldown was bypassed")
	}
}
