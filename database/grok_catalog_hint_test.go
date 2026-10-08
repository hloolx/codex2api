package database

import (
	"context"
	"testing"
	"time"
)

func TestGrokCatalogHintRoundTripsAndConcurrentObservations(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "hint", "xai", "grok", map[string]any{"upstream_type": "grok", "api_key": "test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	origin := "https://api.x.ai/v1"
	items := []GrokModelCatalogItem{{ModelID: "grok-4.7"}}
	replace := func(at time.Time, httpETag, hint, requestHint string) {
		t.Helper()
		snap := GrokModelCatalogSnapshot{AccountID: id, Origin: origin, CredentialGeneration: 1, AuthKind: "api_key", Status: "ok", HTTPETag: httpETag, ETagHint: hint, RequestETagHint: requestHint, ObservedAt: at, ExpiresAt: at.Add(5 * time.Minute)}
		if hint != "" {
			snap.ETagHintObservedAt = at
		}
		if ok, e := db.ReplaceGrokModelCatalog(ctx, snap, items); e != nil || !ok {
			t.Fatalf("replace %v %v", ok, e)
		}
	}
	read := func() *GrokModelCatalogSnapshot {
		t.Helper()
		s, _, e := db.GetGrokModelCatalog(ctx, id, origin)
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	hint := func(value string, at time.Time) {
		t.Helper()
		_, e := db.UpdateGrokModelsETagHint(ctx, id, origin, 1, value, at)
		if e != nil {
			t.Fatal(e)
		}
	}
	replace(now, "\"one\"", "hint-a", "")
	if _, err = db.UpsertGrokModelCapability(ctx, GrokModelCapability{AccountID: id, ModelID: "grok-4.7", Origin: origin, Protocol: "responses", CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Missing x-models-etag must retain the known opaque hint.
	replace(now.Add(time.Minute), "W/\"one\"", "", "hint-a")
	if s := read(); s.ETagHint != "hint-a" || !s.ExpiresAt.Equal(now.Add(6*time.Minute)) {
		t.Fatalf("missing hint: %+v", s)
	}
	caps, e := db.GetGrokModelCapabilities(ctx, id)
	if e != nil || len(caps) != 1 {
		t.Fatalf("representation change deleted capability: %+v %v", caps, e)
	}
	hint("hint-a", now.Add(2*time.Minute))
	if !read().ExpiresAt.Equal(now.Add(6 * time.Minute)) {
		t.Fatal("repeated hint invalidated")
	}
	hint("hint-b", now.Add(3*time.Minute))
	if !read().ExpiresAt.Equal(now.Add(3 * time.Minute)) {
		t.Fatal("real change did not invalidate")
	}
	// /models request began before the new hint arrived: it cannot swallow it.
	replace(now.Add(2*time.Minute+30*time.Second), "\"one\"", "", "hint-a")
	if s := read(); s.ETagHint != "hint-b" || s.ExpiresAt.After(now.Add(3*time.Minute)) {
		t.Fatalf("concurrent hint lost: %+v", s)
	}
	replace(now.Add(4*time.Minute), "\"one\"", "", "hint-b")
	// A delayed old observation cannot roll the hint backwards.
	hint("hint-a", now.Add(time.Minute))
	if read().ETagHint != "hint-b" {
		t.Fatal("out of order hint overwrote latest")
	}
	// A repeated hint during a refresh of naturally expired data is harmless.
	hint("hint-b", now.Add(12*time.Minute))
	replace(now.Add(11*time.Minute), "\"one\"", "", "hint-b")
	if !read().ExpiresAt.Equal(now.Add(16 * time.Minute)) {
		t.Fatal("concurrent repeat kept catalog expired")
	}
	ok, e := db.TouchGrokModelCatalogNotModified(ctx, id, origin, 1, now.Add(13*time.Minute), now.Add(18*time.Minute))
	if e != nil || !ok || !read().ExpiresAt.Equal(now.Add(18*time.Minute)) {
		t.Fatalf("304: %v %v", ok, e)
	}
	caps, e = db.GetGrokModelCapabilities(ctx, id)
	if e != nil || len(caps) != 1 {
		t.Fatal("refresh deleted unchanged capability")
	}
}
