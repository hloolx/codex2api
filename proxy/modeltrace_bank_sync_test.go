package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/sjson"
)

func TestFetchModelTraceBankPinsRevision(t *testing.T) {
	const revision = "d4131b30243dfa05e70180b5eedde742103f1d73"
	var bankPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/commits/main":
			if r.Header.Get("Accept") != "application/vnd.github.sha" {
				t.Errorf("commit Accept = %q", r.Header.Get("Accept"))
			}
			_, _ = w.Write([]byte(revision + "\n"))
		default:
			bankPath = r.URL.Path
			_, _ = w.Write(modelTraceBankJSON)
		}
	}))
	defer upstream.Close()
	defer SetModelTraceSourceURLsForTest(upstream.URL+"/commits/main", upstream.URL+"/%s/unified_bank.json")()

	got, err := FetchLatestModelTraceRevision(context.Background(), "")
	if err != nil || got != revision {
		t.Fatalf("FetchLatestModelTraceRevision() = %q, %v", got, err)
	}
	body, err := FetchModelTraceBank(context.Background(), got, "")
	if err != nil {
		t.Fatalf("FetchModelTraceBank() error = %v", err)
	}
	if bankPath != "/"+revision+"/unified_bank.json" {
		t.Fatalf("bank fetched from %q, want revision-pinned path", bankPath)
	}
	detector, err := ParseModelTraceBankOverride(body, got)
	if err != nil {
		t.Fatalf("ParseModelTraceBankOverride() error = %v", err)
	}
	if info := detector.Info(); info.Origin != ModelTraceBankOriginOverride || info.Revision != revision || len(info.Models) != 17 || detector.BuiltAt().IsZero() {
		t.Fatalf("override info = %+v builtAt=%v", info, detector.BuiltAt())
	}
}

func TestFetchLatestModelTraceRevisionRejectsGarbage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer upstream.Close()
	defer SetModelTraceSourceURLsForTest(upstream.URL, upstream.URL+"/%s")()

	if revision, err := FetchLatestModelTraceRevision(context.Background(), ""); err == nil {
		t.Fatalf("FetchLatestModelTraceRevision() = %q, want error", revision)
	}
	if _, err := FetchModelTraceBank(context.Background(), "../../etc/passwd", ""); err == nil {
		t.Fatal("FetchModelTraceBank() accepted a non-SHA revision")
	}
}

func TestParseModelTraceBankOverrideRejectsOtherScoringMethods(t *testing.T) {
	mutations := map[string]func([]byte) ([]byte, error){
		"method name": func(body []byte) ([]byte, error) { return sjson.SetBytes(body, "method.name", "Something new") },
		"value range": func(body []byte) ([]byte, error) { return sjson.SetBytes(body, "method.range.1", 400) },
		"ordered feature": func(body []byte) ([]byte, error) {
			return sjson.SetBytes(body, "robust.ordered_blocks.feature", "eight position blocks")
		},
		"weight mismatch": func(body []byte) ([]byte, error) { return sjson.SetBytes(body, "method.ordered_block_weight", 0.5) },
	}
	for name, mutate := range mutations {
		body, err := mutate(append([]byte(nil), modelTraceBankJSON...))
		if err != nil {
			t.Fatalf("%s: mutate: %v", name, err)
		}
		if _, err := ParseModelTraceBankOverride(body, "rev"); err == nil || !strings.Contains(err.Error(), "scoring method") {
			t.Fatalf("%s: ParseModelTraceBankOverride() error = %v, want scoring method rejection", name, err)
		}
	}
	if _, err := ParseModelTraceBankOverride(modelTraceBankJSON, " "); err == nil {
		t.Fatal("ParseModelTraceBankOverride() accepted an empty revision")
	}
}
