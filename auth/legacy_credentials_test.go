package auth

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/codex2api/database"
)

func TestAccountLoadingIgnoresRetiredCredentialFlags(t *testing.T) {
	accountTypes := []struct {
		name        string
		credentials map[string]any
	}{
		{
			name: "codex_oauth",
			credentials: map[string]any{
				"access_token":  "legacy-access-token",
				"refresh_token": "legacy-refresh-token",
				"plan_type":     "plus",
			},
		},
		{
			name: "responses_relay",
			credentials: map[string]any{
				"upstream_type": UpstreamOpenAIResponses,
				"base_url":      "https://relay.invalid/v1",
				"api_key":       "legacy-relay-key",
			},
		},
		{
			name: "agent_identity",
			credentials: map[string]any{
				"auth_mode":         CodexAuthModeAgentIdentity,
				"agent_runtime_id":  "legacy-runtime-id",
				"agent_private_key": "legacy-private-key",
				"task_id":           "legacy-task-id",
			},
		},
	}
	legacyFlags := []struct {
		name    string
		enabled bool
		optOut  bool
	}{
		{name: "enabled", enabled: true},
		{name: "opt_out", optOut: true},
		{name: "both", enabled: true, optOut: true},
	}
	for _, accountType := range accountTypes {
		for _, flags := range legacyFlags {
			t.Run(accountType.name+"/"+flags.name, func(t *testing.T) {
				store := NewStore(nil, nil, &database.SystemSettings{
					MaxConcurrency:       2,
					TestConcurrency:      1,
					FastSchedulerEnabled: true,
				})
				t.Cleanup(store.Stop)
				row := &database.AccountRow{ID: 1, Enabled: true, Credentials: maps.Clone(accountType.credentials)}
				row.Credentials["models"] = []string{"gpt-5.4"}
				baseline := store.buildAccountFromRow(context.Background(), row, nil)
				if baseline == nil {
					t.Fatal("account fixture failed to load")
				}

				// Older databases may still contain these keys after upgrading.
				row.Credentials["openai_excel_bps"] = flags.enabled
				row.Credentials["openai_excel_bps_opt_out"] = flags.optOut
				account := store.buildAccountFromRow(context.Background(), row, nil)
				if !reflect.DeepEqual(account, baseline) {
					t.Fatal("retired credential flags changed the runtime account")
				}
				store.AddAccount(account)
				selected := store.NextExcludingWithFilter(0, nil, func(candidate *Account) bool {
					return candidate.SupportsCodexModel("gpt-5.4")
				})
				if selected != account {
					t.Fatal("account with retired credential flags was not schedulable")
				}
				store.Release(selected)
			})
		}
	}
}
