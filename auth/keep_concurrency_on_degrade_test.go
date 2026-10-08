package auth

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestKeepConcurrencyOnDegradeKeepsBaseLimitForWarmAndRisky(t *testing.T) {
	cases := []struct {
		name string
		acc  *Account
		want AccountHealthTier
	}{
		{
			name: "warm after recent failure",
			acc: &Account{
				AccessToken:              "token",
				Status:                   StatusReady,
				PlanType:                 "pro",
				KeepConcurrencyOnDegrade: true,
				LastFailureAt:            time.Now(),
			},
			want: HealthTierWarm,
		},
		{
			name: "risky after unauthorized",
			acc: &Account{
				AccessToken:              "token",
				Status:                   StatusReady,
				PlanType:                 "pro",
				KeepConcurrencyOnDegrade: true,
				LastUnauthorizedAt:       time.Now(),
			},
			want: HealthTierRisky,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recomputeTestAccount(tc.acc, 8)
			// 层级照算,只解开并发折算。
			if tc.acc.HealthTier != tc.want {
				t.Fatalf("HealthTier = %s, want %s", tc.acc.HealthTier, tc.want)
			}
			if tc.acc.DynamicConcurrencyLimit != 8 {
				t.Fatalf("DynamicConcurrencyLimit = %d, want base limit 8", tc.acc.DynamicConcurrencyLimit)
			}
		})
	}
}

func TestKeepConcurrencyOnDegradeKeepsRiskyScoringUntouched(t *testing.T) {
	newAccount := func(keep bool) *Account {
		return &Account{
			AccessToken:              "token",
			Status:                   StatusReady,
			PlanType:                 "pro",
			KeepConcurrencyOnDegrade: keep,
			LastUnauthorizedAt:       time.Now(),
		}
	}
	off := newAccount(false)
	on := newAccount(true)
	recomputeTestAccount(off, 6)
	recomputeTestAccount(on, 6)

	if off.DynamicConcurrencyLimit != 1 {
		t.Fatalf("default risky DynamicConcurrencyLimit = %d, want 1", off.DynamicConcurrencyLimit)
	}
	if on.HealthTier != off.HealthTier || on.ScoreBiasEffective != off.ScoreBiasEffective {
		t.Fatalf("switch changed tier/bias: on=(%s,%d) off=(%s,%d)", on.HealthTier, on.ScoreBiasEffective, off.HealthTier, off.ScoreBiasEffective)
	}
	if diff := on.DispatchScore - off.DispatchScore; diff > 0.01 || diff < -0.01 {
		t.Fatalf("switch changed DispatchScore: on=%v off=%v", on.DispatchScore, off.DispatchScore)
	}
}

func TestKeepConcurrencyOnDegradeDoesNotLiftIndependentGuards(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  *Account
		want int64
	}{
		{
			name: "banned stays zero",
			acc: &Account{
				AccessToken:              "token",
				Status:                   StatusReady,
				PlanType:                 "pro",
				HealthTier:               HealthTierBanned,
				KeepConcurrencyOnDegrade: true,
			},
			want: 0,
		},
		{
			name: "premium 5h limit stays one",
			acc: &Account{
				AccessToken:              "token",
				Status:                   StatusReady,
				PlanType:                 "plus",
				KeepConcurrencyOnDegrade: true,
				UsagePercent5h:           100,
				UsagePercent5hValid:      true,
				Reset5hAt:                now.Add(time.Hour),
			},
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recomputeTestAccount(tc.acc, 6)
			if tc.acc.DynamicConcurrencyLimit != tc.want {
				t.Fatalf("DynamicConcurrencyLimit = %d, want %d", tc.acc.DynamicConcurrencyLimit, tc.want)
			}
		})
	}
}

func TestKeepConcurrencyOnDegradeFastSchedulerFallbackPaths(t *testing.T) {
	newRisky := func() *Account {
		return &Account{
			AccessToken:              "token",
			Status:                   StatusReady,
			PlanType:                 "pro",
			HealthTier:               HealthTierRisky,
			BaseConcurrencyEffective: 6,
			KeepConcurrencyOnDegrade: true,
		}
	}

	// DynamicConcurrencyLimit 尚未算出时,快照走兜底折算,必须与主路径一致。
	if _, _, limit, _, _ := newRisky().fastSchedulerSnapshot(6, time.Now()); limit != 6 {
		t.Fatalf("snapshot fallback limit = %d, want 6", limit)
	}
	if _, _, limit, _, _ := newRisky().fastSchedulerSnapshotForSpark(6, time.Now()); limit != 6 {
		t.Fatalf("spark snapshot limit = %d, want 6", limit)
	}
}

func TestKeepConcurrencyOnDegradeLoadsFromCredentials(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1})
	t.Cleanup(store.Stop)

	row := &database.AccountRow{ID: 1, Enabled: true, Credentials: map[string]any{
		"upstream_type":                       UpstreamOpenAIResponses,
		"base_url":                            "https://relay.invalid/v1",
		"api_key":                             "relay-key",
		"models":                              []string{"gpt-5.4"},
		KeepConcurrencyOnDegradeCredentialKey: true,
	}}
	account := store.buildAccountFromRow(context.Background(), row, nil)
	if account == nil {
		t.Fatal("account fixture failed to load")
	}
	if !account.KeepConcurrencyOnDegrade {
		t.Fatal("KeepConcurrencyOnDegrade = false, want true from credentials")
	}

	store.AddAccount(account)
	account.mu.Lock()
	account.LastUnauthorizedAt = time.Now()
	account.mu.Unlock()
	if !store.ApplyAccountKeepConcurrencyOnDegrade(1, false) {
		t.Fatal("ApplyAccountKeepConcurrencyOnDegrade returned false")
	}
	if got := account.GetDynamicConcurrencyLimit(); got != 1 {
		t.Fatalf("after disabling, risky limit = %d, want 1", got)
	}
	store.ApplyAccountKeepConcurrencyOnDegrade(1, true)
	if got := account.GetDynamicConcurrencyLimit(); got != 4 {
		t.Fatalf("after enabling, risky limit = %d, want 4", got)
	}
}
