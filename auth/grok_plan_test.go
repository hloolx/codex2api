package auth

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestGrokPlanFromTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tier   any
		want   GrokPlan
		wantOK bool
	}{
		{"free", 0, GrokPlan{Key: "free", Display: "Free"}, true},
		{"supergrok", 1, GrokPlan{Key: "supergrok", Display: "SuperGrok", Paid: true, Billing: true}, true},
		{"x basic", 2, GrokPlan{Key: "x_basic", Display: "X Basic", Paid: true}, true},
		{"x premium", 3, GrokPlan{Key: "x_premium", Display: "X Premium", Paid: true, Billing: true}, true},
		{"x premium plus", 4, GrokPlan{Key: "x_premium_plus", Display: "X Premium+", Paid: true, Billing: true}, true},
		{"supergrok heavy", 5, GrokPlan{Key: "supergrok_heavy", Display: "SuperGrok Heavy", Paid: true, Billing: true}, true},
		{"supergrok lite", 6, GrokPlan{Key: "supergrok_lite", Display: "SuperGrok Lite", Paid: true, Billing: true}, true},
		{"supergrok plus", 7, GrokPlan{Key: "supergrok_plus", Display: "SuperGrok Plus", Paid: true, Billing: true}, true},
		{"numeric string", "6", GrokPlan{Key: "supergrok_lite", Display: "SuperGrok Lite", Paid: true, Billing: true}, true},
		{"future positive tier", 8, GrokPlan{Key: "8", Display: "8", Paid: true, Billing: true}, true},
		{"future decimal tier", json.Number("7.5"), GrokPlan{Key: "7.5", Display: "7.5", Paid: true, Billing: true}, true},
		{"negative tier", -1, GrokPlan{}, false},
		{"missing tier", nil, GrokPlan{}, false},
		{"invalid tier", "invalid", GrokPlan{}, false},
		{"nan tier", math.NaN(), GrokPlan{}, false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := GrokPlanFromTier(tt.tier)
			if ok != tt.wantOK {
				t.Fatalf("GrokPlanFromTier(%v) ok = %v, want %v", tt.tier, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("GrokPlanFromTier(%v) = %#v, want %#v", tt.tier, got, tt.want)
			}
		})
	}
}

func TestResolveGrokPlanAliases(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"free":            "free",
		"SuperGrok":       "supergrok",
		"GrokPro":         "supergrok",
		"X Basic":         "x_basic",
		"x_premium":       "x_premium",
		"X Premium+":      "x_premium_plus",
		"X Premium Plus":  "x_premium_plus",
		"SuperGrok Heavy": "supergrok_heavy",
		"SuperGrokPro":    "supergrok_heavy",
		"supergrok_lite":  "supergrok_lite",
		"SuperGrokPlus":   "supergrok_plus",
		"6":               "supergrok_lite",
	}
	for input, wantKey := range tests {
		input, wantKey := input, wantKey
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, ok := ResolveGrokPlan(input)
			if !ok {
				t.Fatalf("ResolveGrokPlan(%q) returned !ok", input)
			}
			if got.Key != wantKey {
				t.Fatalf("ResolveGrokPlan(%q).Key = %q, want %q", input, got.Key, wantKey)
			}
		})
	}

	for _, input := range []any{"api", "unknown", "", "-2", nil} {
		if got, ok := ResolveGrokPlan(input); ok {
			t.Fatalf("ResolveGrokPlan(%v) = %#v, want invalid", input, got)
		}
	}
}

func TestCanonicalGrokLivePlanFilter(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"GrokPro": "supergrok", "SuperGrokPro": "supergrok_heavy",
		"SuperGrokPlus": "supergrok_plus", "Future Ultra": "future_ultra",
	} {
		if got := CanonicalGrokLivePlanFilter(input); got != want {
			t.Fatalf("CanonicalGrokLivePlanFilter(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGrokPlanFromAccessToken(t *testing.T) {
	t.Parallel()

	token := makeJWT(map[string]any{"sub": "user-1", "tier": 5})
	plan, ok := GrokPlanFromAccessToken(token)
	if !ok {
		t.Fatal("GrokPlanFromAccessToken returned !ok")
	}
	if plan.Key != "supergrok_heavy" || plan.Display != "SuperGrok Heavy" || !plan.Paid || !plan.Billing {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if got := GrokPlanTypeFromAccessToken("not-a-jwt"); got != "" {
		t.Fatalf("invalid token plan = %q, want empty", got)
	}
}

func TestParseGrokAuthJSONExtractsPlanType(t *testing.T) {
	t.Parallel()

	accessToken := makeJWT(map[string]any{"sub": "user-1", "tier": 4})
	raw, err := json.Marshal(map[string]any{
		"access_token":  accessToken,
		"refresh_token": "rt-test",
		"client_id":     "client-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := ParseGrokAuthJSON(raw)
	if err != nil {
		t.Fatalf("ParseGrokAuthJSON: %v", err)
	}
	if len(credentials) != 1 || credentials[0].PlanType != "x_premium_plus" {
		t.Fatalf("parsed credentials = %#v, want x_premium_plus", credentials)
	}
}

func grokPlanHintTestAccount(planType string) *Account {
	return &Account{UpstreamType: UpstreamGrok, AccessToken: "at", CredentialGeneration: 1, PlanType: planType}
}

func grokPlanHintState(now time.Time, userTier any, display string, ttl time.Duration) *database.GrokAccountState {
	state := &database.GrokAccountState{CredentialGeneration: 1, Facts: map[string]database.GrokAccountFact{}}
	if userTier != nil {
		state.Facts[database.GrokFactUser] = database.GrokAccountFact{
			Kind: database.GrokFactUser, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200,
			Payload:       map[string]any{"subscriptionTier": userTier},
			FieldPresence: map[string]string{"subscriptionTier": "value"},
			ObservedAt:    now, ExpiresAt: now.Add(ttl),
		}
	}
	if display != "" {
		state.Facts[database.GrokFactSettings] = database.GrokAccountFact{
			Kind: database.GrokFactSettings, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200,
			Payload:       map[string]any{"subscription_tier_display": display},
			FieldPresence: map[string]string{"subscription_tier_display": "value"},
			ObservedAt:    now, ExpiresAt: now.Add(ttl),
		}
	}
	return state
}

// issue #713: current xAI access tokens carry no tier claim and many archives
// have no plan_type, so the settings display fact is the only plan evidence.
func TestGrokPlanHintFallsBackToSettingsDisplay(t *testing.T) {
	now := time.Now()
	account := grokPlanHintTestAccount("")
	if got := account.GrokPlanHint(now); got != "" {
		t.Fatalf("no evidence: hint = %q, want empty", got)
	}
	// /user.subscriptionTier explicit null + settings display "Free".
	state := grokPlanHintState(now, nil, "Free", time.Hour)
	state.Facts[database.GrokFactUser] = database.GrokAccountFact{
		Kind: database.GrokFactUser, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200,
		Payload:       map[string]any{"subscriptionTier": nil},
		FieldPresence: map[string]string{"subscriptionTier": "null"},
		ObservedAt:    now, ExpiresAt: now.Add(time.Hour),
	}
	applyGrokPersistentState(account, state)
	if got := account.GrokPlanHint(now); got != "free" {
		t.Fatalf("fresh display: hint = %q, want free", got)
	}
	if got := account.GrokPlanHint(now.Add(2 * time.Hour)); got != "free" {
		t.Fatalf("stale display without PlanType: hint = %q, want free", got)
	}
	if got := account.GetPlanType(); got != "" {
		t.Fatalf("PlanType must stay untouched, got %q", got)
	}
}

func TestGrokPlanHintPrecedence(t *testing.T) {
	now := time.Now()

	t.Run("fresh live tier beats stored plan", func(t *testing.T) {
		account := grokPlanHintTestAccount("supergrok")
		applyGrokPersistentState(account, grokPlanHintState(now, "Free", "SuperGrok", time.Hour))
		if got := account.GrokPlanHint(now); got != "free" {
			t.Fatalf("hint = %q, want free", got)
		}
	})

	t.Run("fresh display beats stored plan", func(t *testing.T) {
		account := grokPlanHintTestAccount("free")
		applyGrokPersistentState(account, grokPlanHintState(now, nil, "SuperGrok Heavy", time.Hour))
		if got := account.GrokPlanHint(now); got != "supergrok_heavy" {
			t.Fatalf("hint = %q, want supergrok_heavy", got)
		}
	})

	t.Run("stored plan beats stale facts", func(t *testing.T) {
		account := grokPlanHintTestAccount("SuperGrok")
		applyGrokPersistentState(account, grokPlanHintState(now, "Free", "Free", time.Minute))
		if got := account.GrokPlanHint(now.Add(time.Hour)); got != "supergrok" {
			t.Fatalf("hint = %q, want supergrok", got)
		}
	})

	t.Run("other generation facts are ignored", func(t *testing.T) {
		account := grokPlanHintTestAccount("")
		applyGrokPersistentState(account, grokPlanHintState(now, nil, "Free", time.Hour))
		account.invalidateGrokPersistentStateLocked(2)
		if got := account.GrokPlanHint(now); got != "" {
			t.Fatalf("hint = %q, want empty after generation change", got)
		}
	})

	t.Run("api key accounts report api", func(t *testing.T) {
		account := &Account{UpstreamType: UpstreamGrok, APIKey: "xai-test"}
		if got := account.GrokPlanHint(now); got != "api" {
			t.Fatalf("hint = %q, want api", got)
		}
	})
}
