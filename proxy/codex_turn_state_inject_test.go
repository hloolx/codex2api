package proxy

import (
	"context"
	"testing"
)

func turnStateTraceContext() (context.Context, *upstreamTraceAudit) {
	audit := &upstreamTraceAudit{requestID: "req-1"}
	return context.WithValue(context.Background(), upstreamTraceContextKey{}, audit), audit
}

func TestObserveCodexTurnStateFrame(t *testing.T) {
	ctx, audit := turnStateTraceContext()
	audit.current = &upstreamTraceAttempt{accountID: 1}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.output_text.delta","delta":"turn-state is a phrase"}`))
	if audit.current.upstreamTurnState != "" {
		t.Fatalf("content frame must not be treated as turn state: %q", audit.current.upstreamTurnState)
	}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.metadata","headers":{"X-Codex-Turn-State":"frame-state"}}`))
	if audit.current.upstreamTurnState != "frame-state" {
		t.Fatalf("metadata frame turn state = %q, want frame-state", audit.current.upstreamTurnState)
	}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.metadata","client_metadata":{"x-codex-turn-state":"later-state"}}`))
	if audit.current.upstreamTurnState != "later-state" {
		t.Fatalf("later frame must win: %q", audit.current.upstreamTurnState)
	}
	if got := codexTurnStateFromFrame([]byte(`{"headers":{"x-codex-turn-state":"bad\nvalue"}}`)); got != "" {
		t.Fatalf("control characters must be rejected: %q", got)
	}
}
