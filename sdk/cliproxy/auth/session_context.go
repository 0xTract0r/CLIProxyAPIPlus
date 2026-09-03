package auth

import (
	"context"
	"strings"
)

// sessionIDContextKey is the context key used to carry the request's session
// identifier (as computed by ExtractSessionID, see selector.go) from the
// request entry point through to usage reporting. It mirrors the
// WithServiceTier/WithReasoningEffort style already used by
// sdk/cliproxy/usage.Record's context helpers, and the
// internal/runtime/executor/helps.APIKeyFromContext style used for the
// usage-sink-facing API key.
type sessionIDContextKey struct{}

// WithSessionID stores id (the session identifier resolved via
// ExtractSessionID) on ctx for downstream usage-sink consumption
// (see internal/usage.RequestStatistics.Record and
// SessionAggregateForAuthIndex). An empty id is a no-op: it deliberately
// leaves any previously-stored value on ctx untouched rather than clobbering
// it with an empty string, and it never fabricates a placeholder id for a
// request ExtractSessionID could not classify into any session -- matching
// the "unknown is not a number" contract already used elsewhere in this
// package (see buildAdaptiveSchedulingView's doc comment).
func WithSessionID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDContextKey{}, id)
}

// SessionIDFromContext returns the session identifier stored on ctx by
// WithSessionID, or "" if none was set. An empty return is a legitimate,
// expected value -- it means the originating request could not be classified
// into any session (see ExtractSessionID's fallback chain) -- and callers
// must not coerce it into a synthetic/shared bucket.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(sessionIDContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}
