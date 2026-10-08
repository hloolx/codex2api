package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

const (
	grokUnifiedWeeklyBillingBody  = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-01T09:19:44.237769+00:00","end":"2026-10-08T09:19:44.237769+00:00"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"billingPeriodStart":"2026-10-01T09:19:44.237769+00:00","billingPeriodEnd":"2026-10-08T09:19:44.237769+00:00"}}`
	grokUnifiedMonthlyBillingBody = `{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodStart":"2026-10-01T00:00:00+00:00","billingPeriodEnd":"2026-11-01T00:00:00+00:00"}}`
)

func TestGrokBillingSummaryFromFactTreatsOmittedWeeklyPercentAsZero(t *testing.T) {
	payload := map[string]any{"config": map[string]any{
		"currentPeriod": map[string]any{"type": "USAGE_PERIOD_TYPE_WEEKLY", "start": "2026-10-01T09:19:44Z", "end": "2026-10-08T09:19:44Z"},
	}}
	summary := GrokBillingSummaryFromFact(payload, map[string]string{"creditUsagePercent": "missing"})
	if summary == nil || summary.WeeklyPercent == nil || *summary.WeeklyPercent != 0 {
		t.Fatalf("omitted weekly creditUsagePercent must project as 0%%, got %+v", summary)
	}
	if summary.WeeklyPeriodEnd != "2026-10-08T09:19:44Z" {
		t.Fatalf("weekly period end = %q", summary.WeeklyPeriodEnd)
	}

	for _, state := range []string{"invalid", "null"} {
		config := map[string]any{
			"currentPeriod": map[string]any{"type": "USAGE_PERIOD_TYPE_WEEKLY"},
		}
		if state == "null" {
			config["creditUsagePercent"] = nil
		}
		summary = GrokBillingSummaryFromFact(map[string]any{"config": config}, map[string]string{"creditUsagePercent": state})
		if summary == nil || summary.WeeklyPercent != nil {
			t.Fatalf("%s creditUsagePercent must stay unknown, got %+v", state, summary)
		}
	}

	monthly := GrokBillingSummaryFromFact(map[string]any{"config": map[string]any{
		"currentPeriod": map[string]any{"type": "USAGE_PERIOD_TYPE_MONTHLY"},
	}}, map[string]string{"creditUsagePercent": "missing"})
	if monthly == nil || monthly.WeeklyPercent != nil || monthly.MonthlyPercent != nil {
		t.Fatalf("monthly period without amounts must not invent a percentage, got %+v", monthly)
	}
}

func TestFetchGrokBillingTreatsOmittedWeeklyPercentAsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(grokUnifiedWeeklyBillingBody))
			return
		}
		_, _ = w.Write([]byte(grokUnifiedMonthlyBillingBody))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: server.URL}
	summary, err := FetchGrokBilling(context.Background(), account, "")
	if err != nil {
		t.Fatalf("FetchGrokBilling: %v", err)
	}
	if summary.WeeklyPercent == nil || *summary.WeeklyPercent != 0 {
		t.Fatalf("weekly percent = %v, want 0", summary.WeeklyPercent)
	}
	if summary.WeeklyPeriodEnd != "2026-10-08T09:19:44.237769+00:00" {
		t.Fatalf("weekly period end = %q", summary.WeeklyPeriodEnd)
	}
	if summary.MonthlyPercent != nil {
		t.Fatalf("zero monthly limit must not produce a monthly percentage, got %v", *summary.MonthlyPercent)
	}
}

func TestApplyGrokBillingDoesNotInferPlanOrQuotaWindows(t *testing.T) {
	weekly, monthly := 100.0, 75.0
	account := &auth.Account{PlanType: "archive-label"}
	credentials := ApplyGrokBilling(nil, account, &GrokBillingSummary{
		Plan: "supergrok", WeeklyPercent: &weekly, MonthlyPercent: &monthly,
		WeeklyPeriodEnd:  time.Now().Add(time.Hour).Format(time.RFC3339),
		MonthlyPeriodEnd: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if got := account.GetPlanType(); got != "archive-label" {
		t.Fatalf("plan_type mutated from billing: %q", got)
	}
	if _, exists := credentials["plan_type"]; exists {
		t.Fatal("billing credentials must not contain plan_type")
	}
	if _, _, ok := account.GetUsageSnapshot5h(); ok {
		t.Fatal("weekly billing was copied into the generic 5h window")
	}
	if _, ok := account.GetUsagePercent7d(); ok {
		t.Fatal("monthly billing was copied into the generic 7d window")
	}
	if _, ok := credentials["grok_billing_detail"]; !ok {
		t.Fatal("legacy grok_billing_detail must still be dual-written")
	}
}
