package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestParseGrokFreeQuotaUsage(t *testing.T) {
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000. Upgrade to a Grok subscription for higher limits: https://grok.com/supergrok"}`)

	if !IsGrokFreeQuotaExhaustedError(body) {
		t.Fatal("expected free quota exhausted error to be detected")
	}
	used, limit, ok := parseGrokFreeQuotaUsage(body)
	if !ok {
		t.Fatal("expected usage numbers to parse")
	}
	if used != 1003617 || limit != 1000000 {
		t.Fatalf("unexpected usage: used=%d limit=%d", used, limit)
	}
	if model := parseGrokFreeQuotaModel(body); model != "grok-4.5-build-free" {
		t.Fatalf("unexpected model: %q", model)
	}
}

func TestApplyGrokCooldownSeparatesAuthPaymentPolicyAndVersion(t *testing.T) {
	t.Run("unknown 402 is not hard disabled", func(t *testing.T) {
		store, acc := newGrokTestAccount("SuperGrok")
		decision := applyGrokCooldown(store, acc, http.StatusPaymentRequired, []byte(`{"error":{"code":"provider_unknown"}}`), nil, "grok-4.5")
		if decision.Reason != "payment_required_unknown" {
			t.Fatalf("reason = %q", decision.Reason)
		}
		if got := acc.RuntimeStatus(); got != "active" {
			t.Fatalf("runtime status = %q, want active", got)
		}
	})

	t.Run("explicit balance exhaustion is hard gated", func(t *testing.T) {
		store, acc := newGrokTestAccount("SuperGrok")
		decision := applyGrokCooldown(store, acc, http.StatusPaymentRequired, []byte(`{"error":{"message":"Grok Build usage balance exhausted"}}`), nil, "grok-4.5")
		if decision.Reason != "usage_limited" || acc.RuntimeStatus() != "usage_limited" {
			t.Fatalf("decision=%+v status=%q", decision, acc.RuntimeStatus())
		}
	})

	t.Run("403 is policy and never unauthorized", func(t *testing.T) {
		store, acc := newGrokTestAccount("SuperGrok")
		decision := applyGrokCooldown(store, acc, http.StatusForbidden, []byte(`{"error":{"code":"subscription_required"}}`), nil, "grok-4.5")
		if decision.Reason != "forbidden" || acc.GetCooldownReason() != "forbidden" {
			t.Fatalf("decision=%+v cooldown=%q", decision, acc.GetCooldownReason())
		}
		if acc.IsBanned() {
			t.Fatal("403 must not mark the credential banned")
		}
	})

	t.Run("401 schedules neutral refresh isolation", func(t *testing.T) {
		store, acc := newGrokTestAccount("SuperGrok")
		decision := applyGrokCooldown(store, acc, http.StatusUnauthorized, []byte(`{"error":{"code":"invalid_token"}}`), nil, "grok-4.5")
		if decision.Reason != "unauthorized" || acc.GetCooldownReason() != "credential_refresh" {
			t.Fatalf("decision=%+v cooldown=%q", decision, acc.GetCooldownReason())
		}
		if acc.IsBanned() {
			t.Fatal("a refreshable Grok 401 must not permanently ban before refresh result")
		}
	})

	t.Run("426 is distinct", func(t *testing.T) {
		store, acc := newGrokTestAccount("SuperGrok")
		decision := applyGrokCooldown(store, acc, http.StatusUpgradeRequired, nil, nil, "grok-4.5")
		if decision.Reason != "version_required" || acc.GetCooldownReason() != "version_required" {
			t.Fatalf("decision=%+v cooldown=%q", decision, acc.GetCooldownReason())
		}
	})
}

func TestParseGrokFreeQuotaUsageVariants(t *testing.T) {
	// 带空格的变体与缺失数字的降级
	spaced := []byte(`tokens (actual/limit) : 42 / 100`)
	used, limit, ok := parseGrokFreeQuotaUsage(spaced)
	if !ok || used != 42 || limit != 100 {
		t.Fatalf("spaced variant: used=%d limit=%d ok=%v", used, limit, ok)
	}

	noNumbers := []byte(`{"code":"subscription:free-usage-exhausted","error":"no numbers here"}`)
	if !IsGrokFreeQuotaExhaustedError(noNumbers) {
		t.Fatal("code-only body should still be detected as exhausted")
	}
	if _, _, ok := parseGrokFreeQuotaUsage(noNumbers); ok {
		t.Fatal("expected parse failure without numbers")
	}

	if _, _, ok := parseGrokFreeQuotaUsage([]byte(`tokens (actual/limit): 5/0`)); ok {
		t.Fatal("zero limit must not parse")
	}

	// model 字段优先于错误文案
	withField := []byte(`{"model":"grok-4-fast","error":"used all the included free usage for model grok-4.5-build-free"}`)
	if model := parseGrokFreeQuotaModel(withField); model != "grok-4-fast" {
		t.Fatalf("model field should win, got %q", model)
	}
}

func newGrokTestAccount(plan string) (*auth.Store, *auth.Account) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	acc := &auth.Account{DBID: 1, APIKey: "xai-test", UpstreamType: auth.UpstreamGrok, PlanType: plan}
	store.AddAccount(acc)
	return store, acc
}

// free 账号免费额度耗尽 → 整号冷却 24h(usage_limited),权威用量快照落运行时。
func TestApplyGrokCooldownFreeQuotaExhausted_FreePlan(t *testing.T) {
	store, acc := newGrokTestAccount("free")
	h := NewHandler(store, nil, nil, nil)
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000."}`)

	decision := h.applyGrokCooldownForModel(acc, 429, body, nil, "grok-4.5-build-free")
	if decision.Reason != "usage_limited" {
		t.Fatalf("reason = %q, want usage_limited", decision.Reason)
	}
	if decision.Scope == rateLimitScopeModel {
		t.Fatal("free plan should get account-level cooldown, not model scope")
	}
	if got := acc.RuntimeStatus(); got != "usage_limited" {
		t.Fatalf("runtime status = %q, want usage_limited", got)
	}
	snap, ok := acc.GetGrokFreeQuotaSnapshot()
	if !ok {
		t.Fatal("expected free quota snapshot recorded")
	}
	if snap.UsedTokens != 1003617 || snap.LimitTokens != 1000000 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Model != "grok-4.5-build-free" {
		t.Fatalf("snapshot model = %q", snap.Model)
	}
}

// 批量测试/连通性测试经 Apply429Cooldown 也必须走 Grok 专用映射:免费额度耗尽须落
// 权威用量快照并标 usage_limited,而非误标 rate_limited、丢失快照(条会退化成 "usage —")。
func TestApply429CooldownRoutesGrokFreeQuota(t *testing.T) {
	store, acc := newGrokTestAccount("free")
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000."}`)

	decision := Apply429Cooldown(store, acc, body, nil, "grok-4.5-build-free")
	if decision.Reason != "usage_limited" {
		t.Fatalf("reason = %q, want usage_limited", decision.Reason)
	}
	if got := acc.RuntimeStatus(); got != "usage_limited" {
		t.Fatalf("runtime status = %q, want usage_limited", got)
	}
	snap, ok := acc.GetGrokFreeQuotaSnapshot()
	if !ok {
		t.Fatal("expected free quota snapshot recorded via batch-test path")
	}
	if snap.UsedTokens != 1003617 || snap.LimitTokens != 1000000 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// 付费账号免费模型耗尽 → 模型级冷却接近 24h(不被 30min 钳制),账号整体仍可用。
func TestApplyGrokCooldownFreeQuotaExhausted_PaidPlan(t *testing.T) {
	store, acc := newGrokTestAccount("SuperGrok")
	h := NewHandler(store, nil, nil, nil)
	body := []byte(`{"error":"You've used all the included free usage for model grok-4.5-build-free for now. tokens (actual/limit): 55/50."}`)

	decision := h.applyGrokCooldownForModel(acc, 429, body, nil, "grok-4.5-build-free")
	if decision.Scope != rateLimitScopeModel {
		t.Fatalf("scope = %v, want model scope", decision.Scope)
	}
	remaining := acc.ModelCooldownRemaining("grok-4.5-build-free")
	if remaining < 23*time.Hour {
		t.Fatalf("model cooldown remaining = %s, want ~24h (30min clamp bug)", remaining)
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("runtime status = %q, want active (only the model is cooled)", got)
	}
}

// issue #713: xAI AT 不再带 tier claim,导入包也常无 plan_type,PlanType 为空。
// 免费额度耗尽错误本身就是在耗免费额度的证据,须整号冷却而不是按付费号只冷却模型。
func TestApplyGrokCooldownFreeQuotaExhausted_UnknownPlanOAuth(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	acc := &auth.Account{DBID: 1, AccessToken: "at", UpstreamType: auth.UpstreamGrok}
	store.AddAccount(acc)
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.7 for now. tokens (actual/limit): 651613/500000."}`)

	decision := applyGrokCooldown(store, acc, http.StatusTooManyRequests, body, nil, "grok-4.7")
	if decision.Reason != "usage_limited" || decision.Scope == rateLimitScopeModel {
		t.Fatalf("decision = %+v, want account-level usage_limited", decision)
	}
	if got := acc.RuntimeStatus(); got != "usage_limited" {
		t.Fatalf("runtime status = %q, want usage_limited", got)
	}
}

// 套餐只能从控制面 display 事实得知时:Free → 整号冷却,付费 → 模型级冷却。
func TestApplyGrokCooldownFreeQuotaExhausted_PlanFromDisplayFact(t *testing.T) {
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 55/50."}`)
	for _, tc := range []struct {
		display   string
		wantModel bool
	}{
		{display: "Free", wantModel: false},
		{display: "SuperGrok", wantModel: true},
	} {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
		acc := &auth.Account{DBID: 1, AccessToken: "at", UpstreamType: auth.UpstreamGrok,
			CredentialGeneration: 1, GrokFactsGeneration: 1, GrokDisplayPlan: tc.display}
		store.AddAccount(acc)
		decision := applyGrokCooldown(store, acc, http.StatusTooManyRequests, body, nil, "grok-4.7")
		if gotModel := decision.Scope == rateLimitScopeModel; gotModel != tc.wantModel {
			t.Fatalf("display %q: decision = %+v, want model scope %v", tc.display, decision, tc.wantModel)
		}
	}
}
