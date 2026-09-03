package auth

import (
	"context"
	"testing"
)

func TestSessionIDFromContext_MissingReturnsEmpty(t *testing.T) {
	if got := SessionIDFromContext(context.Background()); got != "" {
		t.Fatalf("SessionIDFromContext(background) = %q, want empty", got)
	}
	// A genuinely nil context.Context (as opposed to context.Background())
	// must be handled defensively, not panic.
	var nilCtx context.Context
	if got := SessionIDFromContext(nilCtx); got != "" {
		t.Fatalf("SessionIDFromContext(nil) = %q, want empty", got)
	}
}

func TestWithSessionID_RoundTrip(t *testing.T) {
	ctx := WithSessionID(context.Background(), "claude:abcd-1234")
	if got := SessionIDFromContext(ctx); got != "claude:abcd-1234" {
		t.Fatalf("SessionIDFromContext = %q, want %q", got, "claude:abcd-1234")
	}
}

func TestWithSessionID_EmptyIsNoop(t *testing.T) {
	base := WithSessionID(context.Background(), "claude:preserved")
	// An empty id must not clobber a value already stored on ctx.
	ctx := WithSessionID(base, "")
	if got := SessionIDFromContext(ctx); got != "claude:preserved" {
		t.Fatalf("SessionIDFromContext after empty WithSessionID = %q, want unchanged %q", got, "claude:preserved")
	}
	// And on a bare context, an empty id must not synthesize a stored value.
	if got := SessionIDFromContext(WithSessionID(context.Background(), "   ")); got != "" {
		t.Fatalf("SessionIDFromContext after whitespace-only WithSessionID = %q, want empty", got)
	}
}

func TestWithSessionID_TrimsWhitespace(t *testing.T) {
	ctx := WithSessionID(context.Background(), "  session:with-padding  ")
	if got := SessionIDFromContext(ctx); got != "session:with-padding" {
		t.Fatalf("SessionIDFromContext = %q, want trimmed %q", got, "session:with-padding")
	}
}

// TestWithSessionID_CarriesExtractSessionIDPrimaryOnly ties the context
// helpers to the real level-8 message-hash fallback in ExtractSessionID
// (selector.go, extractMessageHashIDs). ExtractSessionID returns only the
// primary id -- WithSessionID's single-string signature makes it structurally
// impossible to also stash the fallback id alongside it, so a caller that
// wires ExtractSessionID's result into ctx via WithSessionID can never
// register both the primary and fallback hash for the same request (the
// "primary/fallback 不重复计数" contract the P6 session aggregation relies
// on -- see internal/usage.SessionAggregateForAuthIndex).
func TestWithSessionID_CarriesExtractSessionIDPrimaryOnly(t *testing.T) {
	// A second-turn Claude-style payload: system + first user message + first
	// assistant reply present, so extractMessageHashIDs computes BOTH a
	// primary (full hash, includes the assistant reply) and a fallback (short
	// hash, system+user only) id for this single request.
	payload := []byte(`{"messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"hello there"},` +
		`{"role":"assistant","content":"hi, how can I help?"},` +
		`{"role":"user","content":"what is 2+2?"}` +
		`]}`)

	primaryID := ExtractSessionID(nil, payload, nil)
	if primaryID == "" {
		t.Fatal("ExtractSessionID returned empty for a well-formed multi-turn payload")
	}

	// Confirm the fallback id genuinely exists and differs from primary (the
	// scenario this test guards against), using the package-internal
	// extractSessionIDs directly -- this is intentionally white-box since the
	// point is verifying that a real, distinct fallback id exists for this
	// payload that WithSessionID must not also carry.
	gotPrimary, fallbackID := extractSessionIDs(nil, payload, nil)
	if gotPrimary != primaryID {
		t.Fatalf("extractSessionIDs primary = %q, want ExtractSessionID's %q", gotPrimary, primaryID)
	}
	if fallbackID == "" || fallbackID == primaryID {
		t.Fatalf("fallbackID = %q, want a non-empty id distinct from primary %q (test payload must exercise the two-hash branch)", fallbackID, primaryID)
	}

	ctx := WithSessionID(context.Background(), primaryID)
	if got := SessionIDFromContext(ctx); got != primaryID {
		t.Fatalf("SessionIDFromContext = %q, want primary %q", got, primaryID)
	}
	// The fallback id must never appear as the stored session id: WithSessionID
	// only ever accepts (and SessionIDFromContext only ever returns) a single
	// string, so calling it once with ExtractSessionID's result already
	// guarantees this -- assert it explicitly so a future refactor that widens
	// the signature to accept a fallback id trips this test.
	if got := SessionIDFromContext(ctx); got == fallbackID {
		t.Fatalf("SessionIDFromContext returned the fallback id %q instead of the primary id %q", fallbackID, primaryID)
	}
}
