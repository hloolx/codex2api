package proxy

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexRoutePayloadRulesRespectDetectorAndServiceTier(t *testing.T) {
	withPayloadRules(t, `{"override":[{"models":["gpt-*"],"params":{"instructions":"rewritten","service_tier":"flex"}}]}`)
	for _, detector := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "detector"}[detector], func(t *testing.T) {
			ctx := context.Background()
			if detector {
				ctx = WithCodexDetectorRequest(ctx)
			}
			decision := newCodexRouteDecision(ctx, "gpt-5.6-sol", "gpt-5.6-sol", database.APIKeyLimits{CodexRoutePolicy: database.CodexRouteNativeOnly}, 1)
			ctx = context.WithValue(ctx, codexRouteKey{}, decision)
			account := &auth.Account{DBID: 993991, AccessToken: "test-token", AccountID: "workspace"}
			var sent []byte
			native := func(_ context.Context, _ *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header) (*http.Response, error) {
				sent = append([]byte(nil), body...)
				return routeTestResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"), nil
			}
			resp, err := executeCodexRoute(ctx, account, []byte(payloadTestBody), "", "", "", nil, nil, false, native)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			want := "rewritten"
			if detector {
				want = "official prompt"
			}
			if got := gjson.GetBytes(sent, "instructions").String(); got != want {
				t.Fatalf("instructions = %q, want %q", got, want)
			}
			if got := gjson.GetBytes(sent, "service_tier").String(); got == "flex" {
				t.Fatal("unsupported payload-rule tier reached the native executor")
			}
		})
	}
}
