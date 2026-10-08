package proxy

import "testing"

func TestCodexWindowsBuildMatchesStore(t *testing.T) {
	store := []int64{26, 928, 1915, 0}
	cases := []struct {
		name     string
		build    []int64
		fallback bool
		want     bool
	}{
		{"versioned package same minor", []int64{26, 928, 30112}, false, true},
		{"versioned package older minor", []int64{26, 924, 22138}, false, false},
		{"fallback package lagging behind store", []int64{26, 924, 22138}, true, true},
		{"fallback package same minor", []int64{26, 928, 30112}, true, true},
		{"fallback package newer than store", []int64{26, 930, 1}, true, false},
		{"fallback package newer major", []int64{27, 1, 1}, true, false},
	}
	for _, tc := range cases {
		if got := codexWindowsBuildMatchesStore(tc.build, store, tc.fallback); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCodexWindowsBuildMemo(t *testing.T) {
	var memo codexWindowsBuildMemo
	memo.put("", "26.924.22138")
	if _, ok := memo.get(""); ok {
		t.Fatal("packages without ETag must never be served from cache")
	}
	memo.put("url|etag-a|10", "26.924.22138")
	if v, ok := memo.get("url|etag-a|10"); !ok || v != "26.924.22138" {
		t.Fatalf("cache hit = %q %v", v, ok)
	}
	if _, ok := memo.get("url|etag-b|10"); ok {
		t.Fatal("a changed ETag must miss the cache")
	}
}
