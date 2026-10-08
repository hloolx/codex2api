package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestExhaustedResetWindowDecision(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name                  string
		pct                   float64
		seconds, reset, after int64
		want                  bool
	}{
		{"weekly", 100, 604800, now.Add(time.Hour).Unix(), 0, true},
		{"five_hour", 100, 18000, now.Add(time.Hour).Unix(), 0, false},
		{"unknown_length", 100, 86400, now.Add(time.Hour).Unix(), 0, false},
		{"monthly", 101, 2592000, now.Add(time.Hour).Unix(), 0, true},
		{"below", 99.99, 604800, now.Add(time.Hour).Unix(), 0, false},
		{"expired", 100, 604800, now.Add(-time.Second).Unix(), 3600, false},
		{"unknown_window", 100, 0, now.Add(time.Hour).Unix(), 0, false},
		{"unknown_reset", 100, 604800, 0, 0, false},
		{"relative_reset", 100, 604800, 0, 3600, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := &proxy.WhamUsage{}
			u.RateLimit.PrimaryWindow = &proxy.WhamUsageWindow{UsedPercent: tt.pct, LimitWindowSeconds: tt.seconds, ResetAt: tt.reset, ResetAfterSeconds: tt.after}
			if got := resetUsageExhausted(u, now, now); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			u.RateLimit.SecondaryWindow = u.RateLimit.PrimaryWindow
			u.RateLimit.PrimaryWindow = nil
			if got := resetUsageExhausted(u, now, now); got != tt.want {
				t.Fatalf("secondary got %v want %v", got, tt.want)
			}
			if resetUsageExhausted(u, now.Add(-time.Minute), now) {
				t.Fatal("stale snapshot consumed")
			}
		})
	}
}

func TestExhaustedResetOnlyLongWindowTriggers(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).Unix()
	u := &proxy.WhamUsage{}
	u.RateLimit.PrimaryWindow = &proxy.WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: reset}
	u.RateLimit.SecondaryWindow = &proxy.WhamUsageWindow{UsedPercent: 80, LimitWindowSeconds: 604800, ResetAt: reset}
	if resetUsageExhausted(u, now, now) {
		t.Fatal("full 5h window with 7d below 100% must not consume a credit")
	}
	u.RateLimit.SecondaryWindow.UsedPercent = 100
	if !resetUsageExhausted(u, now, now) {
		t.Fatal("full 7d window must consume a credit")
	}
}

func TestExhaustedResetScan(t *testing.T) {
	for _, tt := range []struct {
		name            string
		enabled, expiry bool
		plan            string
		pct             float64
		count           int
		queryErr        bool
		want            int
	}{
		{"business_without_expiry", true, false, "self_serve_business_prolite", 100, 2, false, 1},
		{"both_policies_one_credit", true, true, "plus", 100, 2, false, 1},
		{"disabled", false, false, "plus", 100, 2, false, 0},
		{"no_credits", true, false, "plus", 100, 0, false, 0},
		{"live_below_cached_100", true, false, "plus", 99, 2, false, 0},
		{"query_failure", true, false, "plus", 100, 2, true, 0},
		{"expiry_not_blocked_by_usage_failure", true, true, "plus", 100, 2, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previous := proxy.CurrentRuntimeSettings()
			t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
			cfg := proxy.DefaultRuntimeSettings()
			cfg.AutoResetCreditsOnExhaustionEnabled = tt.enabled
			cfg.AutoResetCreditsEnabled = tt.expiry
			proxy.ApplyRuntimeSettings(cfg)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: 71, AccountID: "workspace-71", AccessToken: "fake", PlanType: tt.plan}
			account.SetUsageSnapshot(100, time.Now())
			store.AddAccount(account)
			now := time.Now()
			expires := now.Add(24 * time.Hour)
			if tt.expiry {
				expires = now.Add(30 * time.Minute)
			}
			credit := proxy.WhamResetCreditItem{ID: "coupon-1", Status: "available", ResetType: "codex_rate_limits", ExpiresAt: expires.Format(time.RFC3339)}
			var consumes, queries atomic.Int32
			h := &Handler{store: store,
				queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
					list := &proxy.WhamResetCreditsList{AvailableCount: tt.count}
					if tt.count > 0 {
						list.Credits = []proxy.WhamResetCreditItem{credit}
					}
					return list, nil, nil
				},
				queryResetUsage: func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
					queries.Add(1)
					if tt.queryErr {
						return nil, nil, errors.New("unavailable")
					}
					u := &proxy.WhamUsage{}
					u.RateLimit.SecondaryWindow = &proxy.WhamUsageWindow{UsedPercent: tt.pct, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}
					return u, nil, nil
				},
				consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, id string) (*proxy.WhamResetResult, *http.Response, error) {
					if id != stableAutoResetCreditRequestID(account, credit) {
						t.Errorf("not shared idempotency key: %s", id)
					}
					consumes.Add(1)
					return &proxy.WhamResetResult{WindowsReset: 1}, nil, nil
				},
				probeUsage: func(context.Context, *auth.Account) error { return nil },
			}
			t.Cleanup(h.WaitAutoResetCredits)
			stats := h.runAutoResetCreditsScan(context.Background(), now)
			if stats.Consumed != tt.want || int(consumes.Load()) != tt.want {
				t.Fatalf("stats %+v consumed=%d", stats, consumes.Load())
			}
			if tt.count == 0 && queries.Load() != 0 {
				t.Fatal("queried usage without credits")
			}
			// Covers overlap with manual/automatic redemption: the shared success cooldown
			// prevents another coupon from being used even with stale 100% responses.
			h.runAutoResetCreditsScan(context.Background(), now)
			if int(consumes.Load()) != tt.want {
				t.Fatal("consumed again during cooldown")
			}
		})
	}
}

func TestExhaustedResetDisableDuringScan(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	cfg := defaultBootstrapSettings()
	cfg.AutoResetCreditsOnExhaustionEnabled = true
	if err := db.UpdateSystemSettings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, cfg)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 72, AccessToken: "fake", PlanType: "self_serve_business_prolite"}
	store.AddAccount(account)
	now := time.Now()
	h := &Handler{db: db, store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 1, Credits: []proxy.WhamResetCreditItem{{ID: "one", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339)}}}, nil, nil
		},
		queryResetUsage: func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
			cfg.AutoResetCreditsOnExhaustionEnabled = false
			if err := db.UpdateSystemSettings(context.Background(), cfg); err != nil {
				return nil, nil, err
			}
			u := &proxy.WhamUsage{}
			u.RateLimit.PrimaryWindow = &proxy.WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}
			return u, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			t.Error("consumed after database disabled setting")
			return nil, nil, errors.New("unexpected consume")
		},
	}
	stats := h.runAutoResetCreditsScan(context.Background(), now)
	if stats.Candidates != 1 || stats.Consumed != 0 || stats.Failed != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// Exercise the real settings HTTP handlers, SQLite migration, partial updates,
// reload and runtime publication; the new switch is independent of expiry.
func TestExhaustedResetSettingsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	cfg := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, cfg)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(cfg)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(cfg.GlobalRPM), "admin-secret")
	router := gin.New()
	router.GET("/api/admin/settings", h.GetSettings)
	router.PUT("/api/admin/settings", h.UpdateSettings)
	request := func(method, body string, canceled bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if canceled {
			ctx, cancel := context.WithCancel(req.Context())
			cancel()
			req = req.WithContext(ctx)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	check := func(want bool) {
		t.Helper()
		rec := request(http.MethodGet, "", false)
		var response settingsResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
			t.Fatalf("GET %d %s", rec.Code, rec.Body.String())
		}
		if response.AutoResetCreditsOnExhaustionEnabled != want || response.AutoResetCreditsEnabled {
			t.Fatalf("GET exhaustion=%v expiry=%v", response.AutoResetCreditsOnExhaustionEnabled, response.AutoResetCreditsEnabled)
		}
		saved, err := db.GetSystemSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if saved.AutoResetCreditsOnExhaustionEnabled != want || proxy.CurrentRuntimeSettings().AutoResetCreditsOnExhaustionEnabled != want {
			t.Fatal("database/runtime mismatch")
		}
	}
	check(false)
	rec := request(http.MethodPut, `{"auto_reset_credits_on_exhaustion_enabled":true}`, true)
	if rec.Code != 500 {
		t.Fatalf("failed save status %d", rec.Code)
	}
	check(false)
	rec = request(http.MethodPut, `{"auto_reset_credits_on_exhaustion_enabled":true}`, false)
	if rec.Code != 200 {
		t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
	}
	check(true)
	select {
	case <-h.autoResetCreditsWake:
	default:
		t.Fatal("enable did not wake scanner")
	}
	// Simulate another instance having an older in-memory value.
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	rec = request(http.MethodPut, `{"site_name":"Exhaustion test"}`, false)
	if rec.Code != 200 {
		t.Fatalf("partial PUT %d %s", rec.Code, rec.Body.String())
	}
	check(true)
	saved, _ := db.GetSystemSettings(context.Background())
	proxy.ApplyRuntimeSettingsFromSystem(saved)
	check(true)
	rec = request(http.MethodPut, `{"auto_reset_credits_on_exhaustion_enabled":false}`, false)
	if rec.Code != 200 {
		t.Fatalf("disable PUT %d %s", rec.Code, rec.Body.String())
	}
	check(false)
}

func TestExhaustedResetLiveUsageHTTP(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	cfg := proxy.DefaultRuntimeSettings()
	cfg.AutoResetCreditsOnExhaustionEnabled = true
	proxy.ApplyRuntimeSettings(cfg)
	now := time.Now()
	var usageCalls, consumes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/wham/usage" {
			t.Errorf("unexpected upstream %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Chatgpt-Account-Id") != "fixture-workspace" {
			t.Error("missing usage auth/workspace")
		}
		pct := 100
		if usageCalls.Load() > 1 {
			pct = 0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type":                "self_serve_business_prolite",
			"rate_limit":               map[string]any{"primary_window": map[string]any{"used_percent": pct, "limit_window_seconds": 604800, "reset_at": now.Add(time.Hour).Unix()}},
			"rate_limit_reset_credits": map[string]any{"available_count": 2, "applicable_available_count": 2},
		})
	}))
	defer upstream.Close()
	restore := proxy.SetWhamUsageURLForTest(upstream.URL + "/wham/usage")
	defer restore()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 74, AccessToken: "fixture-token", AccountID: "fixture-workspace", PlanType: "self_serve_business_prolite"}
	store.AddAccount(account)
	h := &Handler{store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{{ID: "coupon-live", Status: "available", ResetType: "codex_rate_limits", ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339)}}}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes.Add(1)
			return &proxy.WhamResetResult{WindowsReset: 1}, nil, nil
		},
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	t.Cleanup(h.WaitAutoResetCredits)
	first := h.runAutoResetCreditsScan(context.Background(), now)
	if first.Consumed != 1 || first.Failed != 0 {
		t.Fatalf("first scan %+v", first)
	}
	// Simulate the next scheduled scan after cooldown. The live 0% response must
	// prevent consuming another credit despite the persisted local 100% badge.
	h.resetCreditLastSuccess.Delete(resetCreditLockKey(account))
	account.SetUsageSnapshot(100, now)
	second := h.runAutoResetCreditsScan(context.Background(), now.Add(5*time.Minute))
	if second.Consumed != 0 || second.Failed != 0 || usageCalls.Load() != 2 || consumes.Load() != 1 {
		t.Fatalf("second scan %+v usage=%d consumes=%d", second, usageCalls.Load(), consumes.Load())
	}
}

func TestExhaustedResetUsage401RefreshAndUnavailableCredits(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 75, AccessToken: "old", PlanType: "plus"}
	store.AddAccount(account)
	now := time.Now()
	var calls, refreshes int
	h := &Handler{store: store,
		queryResetUsage: func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
			calls++
			if calls == 1 {
				return nil, &http.Response{StatusCode: 401, Body: http.NoBody}, errors.New("unauthorized")
			}
			u := &proxy.WhamUsage{}
			// Available does not always mean applicable (credits-only billing can refuse reset).
			_ = json.Unmarshal([]byte(`{"rate_limit_reset_credits":{"available_count":1,"applicable_available_count":0}}`), u)
			u.RateLimit.PrimaryWindow = &proxy.WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}
			return u, nil, nil
		},
		refreshAccount: func(context.Context, int64) error { refreshes++; return nil },
	}
	u, err := h.queryResetUsageWithRefresh(context.Background(), account)
	if err != nil || calls != 2 || refreshes != 1 {
		t.Fatalf("usage calls=%d refreshes=%d err=%v", calls, refreshes, err)
	}
	if resetUsageExhausted(u, now, now) {
		t.Fatal("consumed when upstream reported no applicable credits")
	}
}

func TestAutoResetCreditsSkipsDisabledAccounts(t *testing.T) {
	for _, tt := range []struct {
		name          string
		flag          func(*auth.Account) *int32
		disableMidRun bool
		wantQueried   bool
	}{
		{"dispatch_paused", func(a *auth.Account) *int32 { return &a.DispatchPaused }, false, false},
		{"unauthorized_disabled", func(a *auth.Account) *int32 { return &a.Disabled }, false, false},
		{"paused_during_scan", func(a *auth.Account) *int32 { return &a.DispatchPaused }, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previous := proxy.CurrentRuntimeSettings()
			t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
			cfg := proxy.DefaultRuntimeSettings()
			cfg.AutoResetCreditsEnabled = true
			cfg.AutoResetCreditsBeforeExpiryMin = 60
			cfg.AutoResetCreditsOnExhaustionEnabled = true
			proxy.ApplyRuntimeSettings(cfg)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: 73, AccountID: "workspace-73", AccessToken: "fake", PlanType: "plus"}
			if !tt.disableMidRun {
				atomic.StoreInt32(tt.flag(account), 1)
			}
			store.AddAccount(account)
			now := time.Now()
			var queried atomic.Bool
			h := &Handler{store: store,
				queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
					queried.Store(true)
					if tt.disableMidRun {
						atomic.StoreInt32(tt.flag(account), 1)
					}
					return &proxy.WhamResetCreditsList{AvailableCount: 1, Credits: []proxy.WhamResetCreditItem{{ID: "one", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339)}}}, nil, nil
				},
				queryResetUsage: func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
					u := &proxy.WhamUsage{}
					u.RateLimit.SecondaryWindow = &proxy.WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}
					return u, nil, nil
				},
				consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
					t.Error("consumed a reset credit on a disabled account")
					return nil, nil, errors.New("unexpected consume")
				},
			}
			t.Cleanup(h.WaitAutoResetCredits)
			stats := h.runAutoResetCreditsScan(context.Background(), now)
			if stats.Consumed != 0 || stats.Failed != 0 || queried.Load() != tt.wantQueried {
				t.Fatalf("stats %+v queried=%v", stats, queried.Load())
			}
		})
	}
}
