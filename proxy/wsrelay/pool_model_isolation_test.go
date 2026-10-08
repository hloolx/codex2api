package wsrelay

import "testing"

func TestReusablePoolBaseKeyWithModelIsolatesTemplateIdentity(t *testing.T) {
	if got := reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4"); got != "cache-key|m:gpt-5.4" {
		t.Fatalf("got %q", got)
	}
	if got := reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4-mini"); got != "cache-key|m:gpt-5.4-mini" {
		t.Fatalf("got %q", got)
	}
	if reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4") == reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4-mini") {
		t.Fatal("different models must not share reusable base key")
	}
	if got := reusablePoolBaseKeyWithModel("cache-key", ""); got != "cache-key" {
		t.Fatalf("empty model should preserve base key, got %q", got)
	}
	if got := reusablePoolBaseKeyWithModel("", "gpt-5.4"); got != "" {
		t.Fatalf("empty base key stays empty, got %q", got)
	}
	// Matching identity preserves reuse.
	a := reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4")
	b := reusablePoolBaseKeyWithModel("cache-key", "gpt-5.4")
	if a != b {
		t.Fatalf("same identity must match: %q vs %q", a, b)
	}
}
