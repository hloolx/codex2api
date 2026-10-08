package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/egressipv6"
)

func TestIPv6TransportPoolsPreservePurposeIsolation(t *testing.T) {
	const id int64 = 987654321
	owned := map[http.RoundTripper]bool{}
	t.Cleanup(func() {
		clientPool.Range(func(key, value any) bool {
			entry := value.(*poolEntry)
			if owned[entry.client.Transport] {
				if clientPool.CompareAndDelete(key, entry) {
					releaseEvictedClient(entry.client)
				}
			}
			return true
		})
	})
	get := func(accountID int64, ip, mode, scope string) http.RoundTripper {
		rt := sourceIPv6Transport(accountID, ip, mode, scope)
		owned[rt] = true
		return rt
	}
	responses := get(id, "2604::1", codexTransportModeStandard, "responses")
	if same := get(id, "2604::1", codexTransportModeStandard, "responses"); same != responses {
		t.Fatal("lost same-purpose connection reuse")
	}
	for _, scope := range []string{"maintenance:codex", "maintenance:subscription", "oauth-refresh", "basispoints"} {
		if get(id, "2604::1", codexTransportModeStandard, scope) == responses {
			t.Fatalf("shared business connection with %s", scope)
		}
	}
	if get(id, "2604::2", codexTransportModeStandard, "responses") == responses || get(id+1, "2604::1", codexTransportModeStandard, "responses") == responses {
		t.Fatal("lost account/source isolation")
	}
	browser := get(id, "2604::1", codexTransportModeUTLSChrome, "oauth-session")
	transport, ok := browser.(*utlsRoundTripper)
	if !ok {
		t.Fatal("lost session browser TLS profile")
	}
	dialer, ok := transport.dialer.(*egressipv6.Dialer)
	if !ok || dialer.LocalAddr.String() != "[2604::1]:0" {
		t.Fatalf("wrong source dialer: %T", transport.dialer)
	}
	if _, ok := responses.(*http.Transport); !ok {
		t.Fatal("lost standard TLS profile")
	}
}
