package database

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

func TestIPv6IndependentClaimsRotationAndCooldown(t *testing.T) {
	testIPv6Claims(t, newModelQualityDB(t), 1, 2)
}

func testIPv6Claims(t *testing.T, db *DB, first, second int64) {
	t.Helper()
	ctx := context.Background()
	cfg, err := db.IPv6EgressConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.CooldownSeconds = 600
	if err = db.SaveIPv6EgressConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ips := []string{"2604::1", "2604::2", "2604::3"}
	a, _, err := db.ClaimIPv6Egress(ctx, first, ips, "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := db.ClaimIPv6Egress(ctx, second, ips, "", "", 1000)
	if err != nil || a.IP == b.IP {
		t.Fatalf("independent claims: %+v %+v %v", a, b, err)
	}
	var wg sync.WaitGroup
	results := make(chan IPv6Binding, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, _, err := db.ClaimIPv6Egress(ctx, first, ips, a.IP, "http_429", 1001)
			if err != nil {
				t.Error(err)
			}
			results <- next
		}()
	}
	wg.Wait()
	close(results)
	for next := range results {
		if next.IP != ips[2] || next.Rotations != 1 {
			t.Fatalf("stale error rotated twice: %+v", next)
		}
	}
	_, _, err = db.ClaimIPv6Egress(ctx, first, ips, ips[2], "http_503", 1002)
	if !errors.Is(err, ErrIPv6PoolExhausted) {
		t.Fatalf("expected exhausted: %v", err)
	}
	_, _, err = db.ClaimIPv6Egress(ctx, first, ips, "", "", 1100)
	if !errors.Is(err, ErrIPv6PoolExhausted) {
		t.Fatalf("reused cooling address: %v", err)
	}
	next, _, err := db.ClaimIPv6Egress(ctx, first, ips, "", "", 1601)
	if err != nil || next.IP != ips[0] {
		t.Fatalf("cooldown recovery: %+v %v", next, err)
	}
	cfg.Revision++
	cfg.Enabled = false
	if err = db.SaveIPv6EgressConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, disabled, err := db.ClaimIPv6Egress(ctx, first, ips, "", "", 1700)
	if err != nil || disabled.Enabled {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
	if err = db.SaveIPv6EgressConfig(ctx, cfg); !errors.Is(err, ErrIPv6ConfigConflict) {
		t.Fatalf("missing config fence: %v", err)
	}
}

func TestIPv6ClaimHonorsAllowlistAndRestart(t *testing.T) {
	db := newModelQualityDB(t)
	ctx := context.Background()
	cfg, err := db.IPv6EgressConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.SourceIPs = []string{"2604::2"}
	if err = db.SaveIPv6EgressConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ips := []string{"2604::1", "2604::2"}
	a, _, err := db.ClaimIPv6Egress(ctx, 1, ips, "", "", 1000)
	if err != nil || a.IP != ips[1] {
		t.Fatalf("allowlist: %+v %v", a, err)
	}
	again, _, err := db.ClaimIPv6Egress(ctx, 1, ips, "", "", 1001)
	if err != nil || again != a {
		t.Fatalf("sticky persisted: %+v %v", again, err)
	}
	_, _, err = db.ClaimIPv6Egress(ctx, 2, ips, "", "", 1001)
	if !errors.Is(err, ErrIPv6PoolExhausted) {
		t.Fatalf("shared an address: %v", err)
	}
}

func TestPostgresIPv6Egress(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, err := db.InsertAccountWithCredentials(context.Background(), "ipv6-first", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.InsertAccountWithCredentials(context.Background(), "ipv6-second", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	testIPv6Claims(t, db, first, second)
}
