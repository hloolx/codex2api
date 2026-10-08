package database

import (
	"context"
	"testing"
	"time"
)

func TestGrokPlanDisplayPrecedenceAndFailure(t *testing.T) {
	now := time.Now()
	fact := func(plan, status string, code int, expiry time.Time) grokDisplayFact {
		return grokDisplayFact{Plan: plan, Status: status, HTTPStatus: code, ObservedAt: now.Add(-time.Minute), ExpiresAt: expiry}
	}
	cases := []struct {
		name, history        string
		user, settings       grokDisplayFact
		want, source, status string
	}{
		{"upgrade", "Free", fact("SuperGrok", "ok", 200, now.Add(time.Minute)), fact("Free", "ok", 200, now.Add(time.Minute)), "SuperGrok", "user", "fresh"},
		{"downgrade", "SuperGrok", fact("Free", "ok", 200, now.Add(time.Minute)), fact("SuperGrok", "ok", 200, now.Add(time.Minute)), "Free", "user", "fresh"},
		{"missing-jwt-tier", "", fact("", "ok", 200, now.Add(time.Minute)), fact("Free", "ok", 200, now.Add(time.Minute)), "Free", "settings", "fresh"},
		{"null-user", "SuperGrok", fact("", "ok", 200, now.Add(time.Minute)), fact("Free", "ok", 200, now.Add(time.Minute)), "Free", "settings", "fresh"},
		{"401", "SuperGrok", fact("", "unauthorized", 401, now), grokDisplayFact{}, "SuperGrok", "historical", "stale"},
		{"timeout-unknown", "", fact("", "unavailable", 0, now), grokDisplayFact{}, "", "historical", "unknown"},
		{"expired", "Free", fact("SuperGrok", "ok", 200, now.Add(-time.Second)), grokDisplayFact{}, "SuperGrok", "user", "stale"},
		{"failed-payload", "SuperGrok", fact("Free", "unavailable", 0, now.Add(time.Minute)), grokDisplayFact{}, "Free", "user", "stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseGrokPlanDisplay(tc.history, map[string]grokDisplayFact{GrokFactUser: tc.user, GrokFactSettings: tc.settings}, now)
			if got.Plan != tc.want || got.Source != tc.source || got.Status != tc.status {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestGrokDisplayHydrationSeparatesModelsAndCredentials(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := db.InsertAccountWithUpstream(ctx, "display", "xai", "grok", map[string]any{"upstream_type": "grok", "access_token": "test", "plan_type": "SuperGrok", "models": []string{"grok-4.6"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UpsertGrokAccountFact(ctx, GrokAccountFact{AccountID: id, CredentialGeneration: 1, Kind: GrokFactSettings, Status: "ok", HTTPStatus: 200, Payload: map[string]any{"subscription_tier_display": "Free"}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{AccountID: id, Origin: "https://api.x.ai/v1", CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour)}, []GrokModelCatalogItem{{ModelID: "grok-4.6"}, {ModelID: "grok-4.7"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListAccountListProjection(ctx, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GrokPlanDisplay.Plan != "Free" || rows[0].GrokPlanDisplay.Status != "fresh" || len(rows[0].GrokModels.Models) != 2 {
		t.Fatalf("bad projection: %+v", rows)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("plan_type") != "SuperGrok" || len(row.GetCredentialStringSlice("models")) != 1 {
		t.Fatal("display modified credentials")
	}
	row.CredentialGeneration = 2
	if err = db.HydrateGrokDisplay(ctx, []*AccountRow{row}); err != nil {
		t.Fatal(err)
	}
	if row.GrokPlanDisplay.Status != "stale" || row.GrokModels.Status != "unknown" || len(row.GrokModels.Models) != 0 {
		t.Fatal("cross-generation display leak")
	}
}
