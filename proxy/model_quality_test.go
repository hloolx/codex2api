package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestModelQualityFiltersOnlySelectedAccountModel(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	a := &auth.Account{DBID: 1, AccessToken: "fixture", CredentialGeneration: 7}
	b := &auth.Account{DBID: 2, AccessToken: "fixture", CredentialGeneration: 7}
	s.AddAccounts([]*auth.Account{a, b})
	cfg := database.ModelQualityConfig{Enabled: true, Models: []string{"gpt-a", "gpt-b"}}
	states := []database.ModelQualityState{{AccountID: 1, Model: "gpt-a", Status: "fail", Generation: 7}, {AccountID: 1, Model: "gpt-b", Status: "pass", Generation: 7}}
	s.ApplyModelQualitySnapshot(cfg, states)
	filters := []auth.AccountFilter{accountFilterForModel("gpt-a"), accountFilterForResponsesWebSocket("gpt-a"), accountFilterForResponsesModelCandidates([]string{"gpt-a"}, "gpt-a", true), accountFilterForCompactResponsesModelWithOriginal("gpt-a", "gpt-a", true)}
	for _, filter := range filters {
		if filter(a) || !filter(b) {
			t.Fatal("failed model not isolated across HTTP/WS/compact filters")
		}
	}
	if !accountFilterForModel("gpt-b")(a) || !accountFilterForModel("gpt-unselected")(a) || !a.SupportsCodexModel("gpt-a") || a.IsModelRateLimited("gpt-a") {
		t.Fatal("quality gate changed capability or other models")
	}
	cfg.Enabled = false
	s.ApplyModelQualitySnapshot(cfg, states)
	if !accountFilterForModel("gpt-a")(a) {
		t.Fatal("disabled guard still blocks")
	}
	cfg.Enabled = true
	cfg.Models = []string{"gpt-b"}
	s.ApplyModelQualitySnapshot(cfg, states)
	if !accountFilterForModel("gpt-a")(a) {
		t.Fatal("deselected model still blocked")
	}
	cfg.Models = []string{"gpt-a"}
	a.CredentialGeneration = 8
	s.ApplyModelQualitySnapshot(cfg, states)
	if !accountFilterForModel("gpt-a")(a) {
		t.Fatal("replacement credential inherited old verdict")
	}
}
