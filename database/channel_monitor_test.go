package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChannelMonitorConfigHistoryAndBillingLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "channel-monitor.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "relay", map[string]interface{}{
		"upstream_type": "openai_responses",
		"base_url":      "https://relay.example/v1",
		"api_key":       "sk-test",
		"models":        []string{"gpt-5.4"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	if _, err := db.GetChannelMonitorConfig(ctx, accountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fresh config error = %v, want sql.ErrNoRows", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	config, err := db.UpsertChannelMonitorConfig(ctx, accountID, true, 5, "gpt-5.4", now)
	if err != nil {
		t.Fatalf("UpsertChannelMonitorConfig: %v", err)
	}
	if !config.Enabled || config.IntervalMinutes != 5 || config.Model != "gpt-5.4" || !config.NextCheckAt.Valid {
		t.Fatalf("enabled config = %#v", config)
	}
	due, err := db.ListDueChannelMonitors(ctx, now.Add(time.Second), 20)
	if err != nil || len(due) != 1 || due[0].AccountID != accountID {
		t.Fatalf("due configs = %#v, err=%v", due, err)
	}

	checkedAt := now.Add(2 * time.Second)
	if err := db.RecordChannelMonitorHealth(ctx, ChannelMonitorHealthResult{
		AccountID: accountID, Status: ChannelMonitorStatusOperational, HTTPStatus: 200,
		Model: "gpt-5.4", LatencyMS: 820, FirstTokenMS: 310, Message: "ok",
		CheckedAt: checkedAt, NextCheckAt: checkedAt.Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordChannelMonitorHealth: %v", err)
	}
	checks, err := db.ListChannelMonitorChecks(ctx, accountID, now.Add(-time.Hour), 20)
	if err != nil || len(checks) != 1 || checks[0].LatencyMS != 820 {
		t.Fatalf("checks = %#v, err=%v", checks, err)
	}
	availability, err := db.GetChannelMonitorAvailability(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetChannelMonitorAvailability: %v", err)
	}
	if got := availability[accountID]; got.Total != 1 || got.Available != 1 {
		t.Fatalf("availability = %#v", got)
	}

	billingJSON := `{"billing_scope":"token","resolved_rate_multiplier":0.75,"effective_rate_multiplier":0.75}`
	if err := db.RecordChannelMonitorBilling(ctx, ChannelMonitorBillingResult{
		AccountID: accountID, Status: ChannelMonitorBillingStatusOK, Data: billingJSON,
		Message: "ok", HTTPStatus: 200, CheckedAt: checkedAt,
		NextCheckAt: checkedAt.Add(30 * time.Minute), FailureCount: 0,
	}); err != nil {
		t.Fatalf("RecordChannelMonitorBilling(ok): %v", err)
	}
	if err := db.RecordChannelMonitorBilling(ctx, ChannelMonitorBillingResult{
		AccountID: accountID, Status: ChannelMonitorBillingStatusFailed, PreserveData: true,
		Message: "temporary", HTTPStatus: 503, CheckedAt: checkedAt.Add(time.Minute),
		NextCheckAt: checkedAt.Add(time.Hour), FailureCount: 1,
	}); err != nil {
		t.Fatalf("RecordChannelMonitorBilling(failed): %v", err)
	}
	config, err = db.GetChannelMonitorConfig(ctx, accountID)
	if err != nil {
		t.Fatalf("GetChannelMonitorConfig: %v", err)
	}
	if config.BillingStatus != ChannelMonitorBillingStatusFailed || config.BillingData != billingJSON || !config.BillingSuccessAt.Valid {
		t.Fatalf("billing snapshot did not retain last success: %#v", config)
	}

	config, err = db.UpsertChannelMonitorConfig(ctx, accountID, false, 10, "gpt-5.4", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("disable monitor: %v", err)
	}
	if config.Enabled || config.NextCheckAt.Valid || config.BillingNextCheckAt.Valid {
		t.Fatalf("disabled config still scheduled: %#v", config)
	}
	enabled, err := db.ListEnabledChannelMonitors(ctx)
	if err != nil || len(enabled) != 0 {
		t.Fatalf("enabled configs after disable = %#v, err=%v", enabled, err)
	}

	if _, err := db.UpsertChannelMonitorConfig(ctx, accountID, true, 5, "gpt-5.4", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("re-enable monitor: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, accountID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	enabled, err = db.ListEnabledChannelMonitors(ctx)
	if err != nil || len(enabled) != 0 {
		t.Fatalf("soft-deleted account remained enabled = %#v, err=%v", enabled, err)
	}
	due, err = db.ListDueChannelMonitors(ctx, now.Add(4*time.Minute), 20)
	if err != nil || len(due) != 0 {
		t.Fatalf("soft-deleted account remained due = %#v, err=%v", due, err)
	}
	if err := db.RestoreAccount(ctx, accountID); err != nil {
		t.Fatalf("RestoreAccount: %v", err)
	}
	enabled, err = db.ListEnabledChannelMonitors(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].AccountID != accountID {
		t.Fatalf("restored account monitor not resumed = %#v, err=%v", enabled, err)
	}
}

func TestNormalizeChannelMonitorInterval(t *testing.T) {
	for _, tc := range []struct {
		input int
		want  int
	}{{0, 5}, {1, 1}, {60, 60}, {61, 5}} {
		if got := NormalizeChannelMonitorInterval(tc.input); got != tc.want {
			t.Fatalf("NormalizeChannelMonitorInterval(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestTruncateChannelMonitorMessagePreservesUTF8(t *testing.T) {
	message := strings.Repeat("渠", 2001)
	got := truncateChannelMonitorMessage(message)
	if !utf8.ValidString(got) || len(got) > 2000 {
		t.Fatalf("truncated message is invalid: bytes=%d valid=%t", len(got), utf8.ValidString(got))
	}
}

func TestChannelMonitorTimestampPlaceholderUsesPostgresCast(t *testing.T) {
	if got := (&DB{driver: "postgres"}).channelMonitorTimestampPlaceholder(7); got != "$7::TIMESTAMPTZ" {
		t.Fatalf("postgres placeholder = %q", got)
	}
	if got := (&DB{driver: "sqlite"}).channelMonitorTimestampPlaceholder(7); got != "$7" {
		t.Fatalf("sqlite placeholder = %q", got)
	}
	if got := (&DB{driver: "postgres"}).channelMonitorVarcharPlaceholder(1); got != "$1::VARCHAR" {
		t.Fatalf("postgres varchar placeholder = %q", got)
	}
	if got := (&DB{driver: "sqlite"}).channelMonitorVarcharPlaceholder(1); got != "$1" {
		t.Fatalf("sqlite varchar placeholder = %q", got)
	}
}

// This test is opt-in because it creates the full schema. CI or a local
// regression run can point it at an empty disposable PostgreSQL database.
func TestChannelMonitorPostgresLifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CODEX2API_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatalf("New(postgres): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "postgres-monitor", map[string]interface{}{
		"upstream_type": "openai_responses",
		"base_url":      "https://relay.example/v1",
		"api_key":       "sk-postgres-test",
		"models":        []string{"gpt-5.4"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.UpsertChannelMonitorConfig(ctx, accountID, true, 5, "gpt-5.4", now); err != nil {
		t.Fatalf("UpsertChannelMonitorConfig: %v", err)
	}
	if err := db.RecordChannelMonitorHealth(ctx, ChannelMonitorHealthResult{
		AccountID: accountID, Status: ChannelMonitorStatusOperational, HTTPStatus: 200,
		Model: "gpt-5.4", LatencyMS: 300, FirstTokenMS: 100, Message: "ok",
		CheckedAt: now, NextCheckAt: now.Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordChannelMonitorHealth: %v", err)
	}
	if err := db.RecordChannelMonitorBilling(ctx, ChannelMonitorBillingResult{
		AccountID: accountID, Status: ChannelMonitorBillingStatusOK,
		Data: `{"resolved_rate_multiplier":0.75}`, Message: "ok", HTTPStatus: 200,
		CheckedAt: now, NextCheckAt: now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordChannelMonitorBilling: %v", err)
	}
	config, err := db.GetChannelMonitorConfig(ctx, accountID)
	if err != nil {
		t.Fatalf("GetChannelMonitorConfig: %v", err)
	}
	if !config.NextCheckAt.Valid || !config.BillingNextCheckAt.Valid || config.Status != ChannelMonitorStatusOperational {
		t.Fatalf("postgres monitor config = %#v", config)
	}
}
