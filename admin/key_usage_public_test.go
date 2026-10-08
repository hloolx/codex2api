package admin

import (
	"strings"
	"testing"
)

func TestParsePublicAPIKeyUsageLogFilter(t *testing.T) {
	filter, err := parsePublicAPIKeyUsageLogFilter(" gpt-5.5 ", "/v1/responses", "ERROR", "Stream", "Claude")
	if err != nil {
		t.Fatalf("parse valid filter: %v", err)
	}
	if filter.Model != "gpt-5.5" || filter.Endpoint != "/v1/responses" || filter.Status != "error" || filter.Stream != "stream" || filter.Channel != "claude" {
		t.Fatalf("filter = %+v", filter)
	}

	if _, err := parsePublicAPIKeyUsageLogFilter("", "", "", "", ""); err != nil {
		t.Fatalf("empty filter should be valid: %v", err)
	}
	invalid := []struct{ model, endpoint, status, stream, channel string }{
		{status: "200"},
		{stream: "ws"},
		{channel: "openai"},
		{model: strings.Repeat("m", publicAPIKeyUsageMaxFilterLen+1)},
	}
	for _, tc := range invalid {
		if _, err := parsePublicAPIKeyUsageLogFilter(tc.model, tc.endpoint, tc.status, tc.stream, tc.channel); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}
