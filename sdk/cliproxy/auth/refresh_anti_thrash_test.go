package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// antiThrashTestExecutor records every Refresh call and rewrites the expiry to
// simulate an upstream provider whose access_token TTL is significantly shorter
// than the configured refresh lead. The TTL is what historically triggered the
// "refresh, rewrite auth file, repeat every minute" thrash loop seen in
// production.
type antiThrashTestExecutor struct {
	provider     string
	ttl          time.Duration
	refreshCalls atomic.Int32
}

func (e *antiThrashTestExecutor) Identifier() string { return e.provider }

func (e *antiThrashTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *antiThrashTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *antiThrashTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	if auth == nil {
		return nil, nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	// Simulate upstream re-issuing a short-lived token whose TTL stays inside
	// the provider's RefreshLead window so the legacy code path would mark the
	// freshly refreshed auth as immediately due-for-refresh.
	auth.Metadata["expired"] = time.Now().Add(e.ttl).Format(time.RFC3339)
	return auth, nil
}

func (e *antiThrashTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *antiThrashTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

// TestRefreshAuth_DoesNotThrashWhenLeadExceedsTTL exercises the full
// refreshAuth path with a synthetic executor that mimics the production
// scenario: provider RefreshLead is much larger than the issued access_token
// TTL. The legacy code path would schedule another refresh inside
// refreshIneffectiveBackoff (30s), which when combined with refreshPendingBackoff
// produced the observed ~1-2 minute rewrite loop. With the fix, NextRefreshAfter
// must move forward by at least token-half-life (here 30 min).
func TestRefreshAuth_DoesNotThrashWhenLeadExceedsTTL(t *testing.T) {
	// Make the provider lead deliberately larger than the simulated TTL so
	// shouldRefresh returns true again after a successful refresh, which is
	// exactly the condition that drove the thrash.
	leadDur := 4 * time.Hour
	provider := "anti-thrash-claude"
	setRefreshLeadFactory(t, provider, func() *time.Duration {
		d := leadDur
		return &d
	})

	exec := &antiThrashTestExecutor{provider: provider, ttl: time.Hour}
	manager := NewManager(NoopStore{}, nil, nil)
	manager.RegisterExecutor(exec)

	ctx := context.Background()
	authID := "anti-thrash-1"
	now := time.Now()
	if _, err := manager.Register(ctx, &Auth{
		ID:       authID,
		Provider: provider,
		Metadata: map[string]any{
			"refresh_token": "rt-1",
			"expired":       now.Add(time.Hour).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() err = %v", err)
	}

	// First refresh: must succeed and must NOT schedule another refresh
	// inside the legacy 30s ineffective backoff window.
	manager.refreshAuth(ctx, authID)
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("first pass refresh calls = %d, want 1", got)
	}

	manager.mu.RLock()
	after := manager.auths[authID]
	manager.mu.RUnlock()
	if after == nil {
		t.Fatalf("auth missing after refresh")
	}

	gap := time.Until(after.NextRefreshAfter)
	if gap <= refreshIneffectiveBackoff {
		t.Fatalf("NextRefreshAfter gap = %s, want > %s (anti-thrash backoff active)", gap, refreshIneffectiveBackoff)
	}
	// Lower bound: at least one half of the simulated 1h TTL minus a small
	// margin for the wall clock between Register and refreshAuth.
	if gap < 25*time.Minute {
		t.Fatalf("NextRefreshAfter gap = %s, want at least ~30min (token half-life)", gap)
	}
}

// NoopStore is a Store implementation that performs no persistence. It exists
// so refreshAuth can run without touching the filesystem and without requiring
// a configured backing store during unit tests.
type NoopStore struct{}

func (NoopStore) Save(ctx context.Context, auth *Auth) (string, error) { return "", nil }
func (NoopStore) List(ctx context.Context) ([]*Auth, error)            { return nil, nil }
func (NoopStore) Delete(ctx context.Context, id string) error          { return nil }
