package egressipv6

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestIPv6FailureClassification(t *testing.T) {
	for _, tt := range []struct {
		status   int
		body     string
		retry5xx bool
		want     string
	}{
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, true, "http_429"},
		{429, `{"error":{"code":"usage_limit_reached"}}`, true, ""},
		{429, `{"error":{"type":"insufficient_quota"}}`, true, ""},
		{503, `{"error":{"code":"server_error"}}`, true, "http_503"},
		{503, `{}`, false, ""}, {401, `{}`, true, ""}, {400, `{}`, true, ""}, {403, `{}`, true, ""},
	} {
		if got := FailureReason(tt.status, []byte(tt.body), tt.retry5xx); got != tt.want {
			t.Errorf("status=%d body=%s got=%q want=%q", tt.status, tt.body, got, tt.want)
		}
	}
	if got := FrameFailure([]byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`), true); got != "http_503" {
		t.Fatal(got)
	}
}
func TestIPv6SourceValidation(t *testing.T) {
	local := []string{"2604::1"}
	valid, err := ValidateSources([]string{"2604:0::1", "2604::1"}, local)
	if err != nil || len(valid) != 1 {
		t.Fatalf("%v %v", valid, err)
	}
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:192.0.2.1", "fe80::1", "fd00::1", "2604::2"} {
		if _, err := ValidateSources([]string{ip}, local); err == nil {
			t.Errorf("accepted %s", ip)
		}
	}
	d, err := NewDialer("2604::1")
	if err != nil {
		t.Fatal(err)
	}
	if d.LocalAddr.(*net.TCPAddr).IP.String() != "2604::1" {
		t.Fatal(d.LocalAddr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func httpFixture(t *testing.T) (*Manager, int64) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "fixture", map[string]interface{}{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := db.IPv6EgressConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	if err = db.SaveIPv6EgressConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	m := New(db)
	m.LocalIPs = func() ([]string, error) { return []string{"2604::1", "2604::2", "2604::3"}, nil }
	return m, id
}
func TestIPv6HTTPRetriesNewSourceAndStopsOnSuccess(t *testing.T) {
	m, id := httpFixture(t)
	seen := []string{}
	c := WrapClient(m, id, &http.Client{}, func(ip string) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen = append(seen, ip)
			body, _ := io.ReadAll(req.Body)
			if string(body) != "original" {
				t.Fatalf("lost request: %s", body)
			}
			status := 200
			if len(seen) == 1 {
				status = 429
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		})
	}, true)
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", strings.NewReader("original"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(seen) != 2 || seen[0] == seen[1] || resp.StatusCode != 200 {
		t.Fatalf("%v status=%d", seen, resp.StatusCode)
	}
}
func TestIPv6OAuthNeverReplays(t *testing.T) {
	m, id := httpFixture(t)
	calls := 0
	c := WrapClient(m, id, &http.Client{}, func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})
	}, false)
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/token", strings.NewReader("token"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatal("replayed OAuth", calls)
	}
}
