package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestReadChannelMonitorSSERequiresTerminalAndExtractsText(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"go"}`,
		"",
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		"",
	}, "\n")
	result := readChannelMonitorSSE(strings.NewReader(stream), time.Now().Add(-100*time.Millisecond))
	if result.Failure != "" || !result.Terminal || result.Text != "go" || result.FirstTokenMS <= 0 {
		t.Fatalf("SSE result = %#v", result)
	}
}

func TestReadChannelMonitorJSONFindsMessageAfterReasoning(t *testing.T) {
	body := []byte(`{
		"status":"completed",
		"output":[
			{"type":"reasoning","summary":[]},
			{"type":"message","content":[{"type":"output_text","text":"go"}]}
		]
	}`)
	result := readChannelMonitorJSON(body)
	if result.Failure != "" || !result.Terminal || result.Text != "go" || result.FirstTokenMS != 0 {
		t.Fatalf("JSON result = %#v", result)
	}
}

func TestChannelMonitorAnswerMatchesExpectedWord(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"go", true},
		{"`go`", true},
		{"GO!", true},
		{"going", false},
		{"The answer is go.", false},
		{"no number", false},
	} {
		if got := channelMonitorAnswerMatches(tc.text, "go"); got != tc.want {
			t.Fatalf("channelMonitorAnswerMatches(%q) = %t, want %t", tc.text, got, tc.want)
		}
	}
}

func TestBuildChannelMonitorPayloadUsesNonStreamingResponses(t *testing.T) {
	payload, expected := buildChannelMonitorPayload("gpt-monitor", 7)
	if expected == "" || gjson.GetBytes(payload, "model").String() != "gpt-monitor" ||
		gjson.GetBytes(payload, "stream").Bool() || gjson.GetBytes(payload, "store").Bool() {
		t.Fatalf("payload = %s, expected=%q", payload, expected)
	}
	if got := gjson.GetBytes(payload, "max_output_tokens").Int(); got != 64 {
		t.Fatalf("max_output_tokens = %d, want 64", got)
	}
}

func TestParseChannelMonitorBillingIsCompatibilityOriented(t *testing.T) {
	data, err := parseChannelMonitorBilling([]byte(`{
		"object":"sub2api.key_billing",
		"schema_version":1,
		"billing_scope":"token",
		"group_rate_multiplier":0.5,
		"user_rate_multiplier":0.7,
		"resolved_rate_multiplier":0.75,
		"peak_rate_enabled":true,
		"peak_start":"09:00",
		"peak_end":"11:00",
		"peak_rate_multiplier":1.5,
		"effective_rate_multiplier":0.75,
		"timezone":"Europe/Berlin"
	}`), time.Now())
	if err != nil {
		t.Fatalf("parseChannelMonitorBilling: %v", err)
	}
	// Deliberately do not require resolved == user or effective == resolved*peak.
	// Monitoring displays an upstream declaration and should tolerate variants.
	if data["resolved_rate_multiplier"] != 0.75 || data["group_rate_multiplier"] != 0.5 || data["peak_rate_enabled"] != true {
		t.Fatalf("billing data = %#v", data)
	}
}

func TestParseChannelMonitorBillingRejectsUnsafeCoreValues(t *testing.T) {
	for _, body := range []string{
		`{"billing_scope":"request","resolved_rate_multiplier":1}`,
		`{"billing_scope":"token"}`,
		`{"billing_scope":"token","resolved_rate_multiplier":-1}`,
	} {
		if _, err := parseChannelMonitorBilling([]byte(body), time.Now()); err == nil {
			t.Fatalf("parseChannelMonitorBilling(%s) unexpectedly succeeded", body)
		}
	}
}

func TestNextChannelMonitorBillingAtBackoff(t *testing.T) {
	now := time.Now().UTC()
	ok := nextChannelMonitorBillingAt(database.ChannelMonitorBillingStatusOK, now, 0, 0, 1)
	unsupported := nextChannelMonitorBillingAt(database.ChannelMonitorBillingStatusUnsupported, now, 1, 0, 1)
	failed := nextChannelMonitorBillingAt(database.ChannelMonitorBillingStatusFailed, now, 3, 0, 1)
	if ok.Sub(now) < channelMonitorBillingInterval || unsupported.Sub(now) < channelMonitorUnsupportedBilling || failed.Sub(now) < 2*time.Hour {
		t.Fatalf("unexpected delays: ok=%s unsupported=%s failed=%s", ok.Sub(now), unsupported.Sub(now), failed.Sub(now))
	}
}
