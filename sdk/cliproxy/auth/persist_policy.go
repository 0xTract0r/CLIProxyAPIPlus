package auth

import "context"

type skipPersistContextKey struct{}
type deferAPIKeyModelAliasRebuildContextKey struct{}
type refreshResultWriteContextKey struct{}

// WithRefreshResultWrite marks an Update whose token-owned fields come straight
// from a successful upstream refresh. Such a write carries the newest token the
// provider issued (a rotation has already invalidated the previous refresh
// token upstream), so the stale write-back guard must not roll it back based on
// local timestamps, which become untrustworthy after a host clock jump.
// Concurrent same-token refreshes are coalesced by the provider singleflight and
// a refresh with an already rotated token fails upstream, so neither reaches
// this write. Only the refresh success paths may set this marker.
func WithRefreshResultWrite(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, refreshResultWriteContextKey{}, true)
}

// withoutRefreshResultWrite clears the refresh-result marker so it only applies
// to the Update that carries the refresh result, never to nested writes made by
// lifecycle hooks (e.g. plugin model registration calling Update again).
func withoutRefreshResultWrite(ctx context.Context) context.Context {
	if !isRefreshResultWrite(ctx) {
		return ctx
	}
	return context.WithValue(ctx, refreshResultWriteContextKey{}, false)
}

func isRefreshResultWrite(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(refreshResultWriteContextKey{}).(bool)
	return ok && enabled
}

// WithSkipPersist returns a derived context that disables persistence for Manager Update/Register calls.
// It is intended for code paths that are reacting to file watcher events, where the file on disk is
// already the source of truth and persisting again would create a write-back loop.
func WithSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(skipPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithDeferredAPIKeyModelAliasRebuild returns a derived context that defers API-key model alias table rebuilds.
// Callers that use this for a batch of Register/Update/Remove operations must call RefreshAPIKeyModelAlias once.
func WithDeferredAPIKeyModelAliasRebuild(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deferAPIKeyModelAliasRebuildContextKey{}, true)
}

func shouldDeferAPIKeyModelAliasRebuild(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(deferAPIKeyModelAliasRebuildContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}
