package egressipv6

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIPv6SSEPreflightPreservesEveryByte(t *testing.T) {
	cases := []struct {
		name, body, reason string
		safe               bool
	}{
		{"early error", "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n", "http_503", true},
		{"after output", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n", "", false},
		{"billable failure", "data: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"output_tokens\":12},\"error\":{\"code\":\"server_error\"}}}\n\n", "http_503", false},
		{"large event", "data: " + strings.Repeat("x", 128*1024) + "\n\n", "", false},
		{"incomplete metadata", "data: {\"type\":\"response.created\"}\n\n", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(tt.body))}
			reason, safe := inspectSSEPrefix(resp, true)
			if reason != tt.reason || safe != tt.safe {
				t.Fatalf("%s %v", reason, safe)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != tt.body {
				t.Fatalf("prefix corruption: got %d want %d err=%v", len(got), len(tt.body), err)
			}
		})
	}
}

func TestIPv6HTTPRetryLimitsAndTerminalFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		calls  int
	}{
		{"bounded", 503, `{}`, 3}, {"quota", 429, `{"error":{"code":"insufficient_quota"}}`, 1}, {"auth", 401, `{}`, 1}, {"invalid", 400, `{}`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, id := httpFixture(t)
			calls := 0
			c := WrapClient(m, id, &http.Client{}, func(string) http.RoundTripper {
				return roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: tt.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
				})
			}, true)
			req, _ := http.NewRequest("POST", "https://example.test/responses", strings.NewReader("body"))
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if calls != tt.calls {
				t.Fatalf("got %d calls, want %d", calls, tt.calls)
			}
		})
	}
}
