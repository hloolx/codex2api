package ipv6state

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/statepool"
)

func TestSummarySelectedScopeIdentityExpiryAndBusySlots(t *testing.T) {
	m, account, now := fixture(t, "summary", false)
	m.config.Models = []string{"gpt-5.6-sol", "gpt-5.6-luna"}
	identity, _ := statepool.Snapshot(account, "")
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.6-terra"} {
		if err := m.Import(context.Background(), Portable{Format: "codex2api-ipv6-292-v1", MemberHash: identity.MemberHash, WorkspaceHash: identity.WorkspaceHash, Model: model, Value: tokenAt(now.Unix() - 3000)}); err != nil {
			t.Fatal(err)
		}
	}
	first := m.Snapshot()
	if first.Summary.ReuseAccounts != 1 || first.Summary.ValidCombinations != 2 || first.Summary.AvailableAccounts != 1 || first.Summary.CoveredModels != 2 || len(first.Accounts[account.ID()]) != 2 {
		t.Fatalf("wrong coverage: %+v", first.Summary)
	}
	for _, model := range first.Summary.Models {
		if model.AvailableAccounts != 1 {
			t.Fatal("per-model count missing")
		}
	}
	account.ActiveRequests.Store(20)
	if snapshot := m.Snapshot(); snapshot.Summary.Revision != first.Summary.Revision || snapshot.Summary.AvailableAccounts != 1 {
		t.Fatal("busy slots changed coverage")
	}
	account.ActiveRequests.Store(0)
	account.SetCooldownUntil(now.Add(time.Minute), "rate_limited")
	if summary := m.Snapshot().Summary; summary.ReuseAccounts != 1 || summary.ValidCombinations != 2 || summary.AvailableAccounts != 0 {
		t.Fatal("cooldown conflated with validity")
	}
	*now = now.Add(time.Minute)
	if m.Snapshot().Summary.AvailableAccounts != 1 {
		t.Fatal("cooldown did not expire at the boundary")
	}
	m.store.ApplyAccountEnabled(account.ID(), false)
	if summary := m.Snapshot().Summary; summary.ReuseAccounts != 1 || summary.AvailableAccounts != 0 {
		t.Fatal("disabled account remained schedulable")
	}
	m.store.ApplyAccountEnabled(account.ID(), true)
	config := m.Status().Config
	config.Enabled, config.Models = false, []string{"gpt-5.6-sol"}
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if summary := m.Snapshot().Summary; summary.TotalAccounts != 1 || summary.ReuseAccounts != 1 || summary.ValidCombinations != 1 || summary.Enabled {
		t.Fatal("disable/deselection changed total or lost saved coverage")
	}
	config.AccountIDs = []int64{account.ID() + 99}
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if summary := m.Snapshot().Summary; summary.ReuseAccounts != 0 || summary.ValidCombinations != 0 {
		t.Fatal("out-of-scope history counted")
	}
	if m.AccountModels(account)[0].InScope {
		t.Fatal("out-of-scope marker missing")
	}
	config.AccountIDs, config.Models = nil, []string{"gpt-5.6-terra"}
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if !m.AccountModels(account)[0].Valid {
		t.Fatal("unselected history was deleted")
	}
	config.AcceptedLengths = []int{332}
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Summary.ValidCombinations != 0 {
		t.Fatal("old length stayed valid")
	}
	config.AcceptedLengths = []int{292}
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	account.CredentialGeneration++
	if m.Snapshot().Summary.ReuseAccounts != 0 {
		t.Fatal("credential change reused state")
	}
	account.CredentialGeneration--
	*now = now.Add(9 * time.Minute)
	if m.Snapshot().Summary.ReuseAccounts != 0 || m.AccountModels(account)[0].Valid {
		t.Fatal("exact expiry boundary retained coverage")
	}
	encoded, _ := json.Marshal(first)
	for _, secret := range []string{account.AccessToken, tokenAt(now.Unix() - 3600), identity.MemberHash, identity.CredentialHash} {
		if secret != "" && strings.Contains(string(encoded), secret) {
			t.Fatal("summary exposed secret or identity")
		}
	}
}

func TestStagesUseConfiguredWindowAndSharedAccountBudget(t *testing.T) {
	m, account, now := fixture(t, "stages", false)
	m.config.RefreshBeforeMinutes, m.config.StagedConcurrency = 30, true
	m.config.Concurrency, m.config.EarlyConcurrency, m.config.UrgentConcurrency, m.config.ExpiredConcurrency = 20, 1, 3, 10
	old := importForTest(t, m, account, now.Unix()-1799)
	started := make(chan struct{}, 20)
	m.execute = func(ctx context.Context, _ *auth.Account, _ string, _ Route) (*http.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.step()
	if m.Status().ActiveRequests != 0 {
		t.Fatal("renewed before configured boundary")
	}
	*now = now.Add(time.Second)
	m.step()
	receive(t, started)
	if entry := m.Status().Entries[0]; m.Status().ActiveRequests != 1 || !entry.Valid || !entry.Refreshing || entry.CapturePhase != "collecting" || entry.CaptureStage != "early" {
		t.Fatalf("wrong early state: %+v", entry)
	}
	if value, _, _ := m.Resolve(account, m.config.Models[0]); value != old {
		t.Fatal("early renewal lost old value")
	}
	*now = now.Add(20 * time.Minute)
	m.step()
	receive(t, started)
	receive(t, started)
	if m.Status().ActiveRequests != 3 {
		t.Fatal("urgent budget not applied")
	}
	*now = now.Add(10 * time.Minute)
	m.step()
	receive(t, started)
	if status := m.Status(); status.ActiveRequests != 4 || status.Entries[0].Valid || status.Summary.ReuseAccounts != 0 {
		t.Fatal("expiry or shared system account limit ignored")
	}
	config := m.Status().Config
	config.Enabled = false
	if err := m.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	m.workers.Wait()
	if m.Status().ActiveRequests != 0 {
		t.Fatal("disable did not cancel stage workers")
	}
}

func TestImportKeepsLaterExpiryAndMixedRoutes(t *testing.T) {
	m, account, now := fixture(t, "imports", false)
	latest := importForTest(t, m, account, now.Unix()-30)
	before := m.entries[key(account.ID(), m.config.Models[0])]
	for _, age := range []int64{30, 100, 3000} {
		importForTest(t, m, account, now.Unix()-age)
	}
	after := m.entries[key(account.ID(), m.config.Models[0])]
	if after.Value != latest || before.ExpiresAt != after.ExpiresAt || before.CapturedAt != after.CapturedAt {
		t.Fatal("older import overwrote or renewed saved state")
	}
	m.config.CaptureMode, m.config.ProxyIDs = "mixed", []int64{7}
	m.route = func(context.Context, Config, int64) (Route, error) {
		return Route{ProxyID: 7, ProxyURL: "http://fake.invalid"}, nil
	}
	var routes []string
	for i := range 4 {
		route, err := m.captureRoute(context.Background(), m.config, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if route.ProxyID != 0 {
			routes = append(routes, "proxy")
		} else {
			routes = append(routes, route.SourceIP)
		}
	}
	if !slices.Equal(routes, []string{"proxy", "2001:db8::1", "proxy", "2001:db8::2"}) {
		t.Fatalf("mixed routes not rotated: %v", routes)
	}
	m.localIPs = func() ([]string, error) { return nil, nil }
	if route, err := m.captureRoute(context.Background(), m.config, 1); err != nil || route.ProxyID != 7 {
		t.Fatal("mixed mode failed to use remaining proxy route")
	}
}

func TestIdentityChangeCancelsWorkerWithoutLateSave(t *testing.T) {
	m, account, _ := fixture(t, "cancel", false)
	started := make(chan struct{})
	m.execute = func(ctx context.Context, _ *auth.Account, _ string, _ Route) (*http.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.step()
	receive(t, started)
	account.Mu().Lock()
	account.CredentialGeneration++
	account.Mu().Unlock()
	m.step()
	m.workers.Wait()
	if m.AccountModels(account)[0].Valid {
		t.Fatal("cancelled identity task published")
	}
}
