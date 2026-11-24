package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManager_Issue6415_Original401DoesNotReplayAndBlocksRejectedGrant(t *testing.T) {
	m, executor, primary, _, model := newUnauthorizedRefreshFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run := func() error {
		opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: primary.ID}}
		_, err := m.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, opts)
		return err
	}
	if err := run(); statusCodeFromError(err) != http.StatusUnauthorized {
		t.Fatalf("original provider 401 lost: %v", err)
	}
	current, ok := m.GetByID(primary.ID)
	if !ok || current == nil {
		t.Fatal("selected grant disappeared")
	}
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); !blocked {
		t.Fatal("rejected grant remains selectable")
	}

	results := make(chan error, 2)
	for range 2 {
		go func() { results <- run() }()
	}
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("independent pinned request used a rejected grant")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if calls := executor.ExecuteCalls(); len(calls) != 1 || calls[0] != primary.ID {
		t.Fatalf("provider calls = %v, want one selected call", calls)
	}
	if n := executor.RefreshCalls(); n != 0 {
		t.Fatalf("request started %d automatic refreshes, want 0", n)
	}
}

// TestManager_Issue6415_NonTerminalRefreshFailure_MarksCredentialUnavailable reproduces
// Issue #6415 symptom 2:
// When refresh fails with a non-terminal error (e.g. transient 503) for a token with
// unknown expiry, the rejected token must not be retained as active.
func TestManager_Issue6415_NonTerminalRefreshFailure_MarksCredentialUnavailable(t *testing.T) {
	m, executor, primary, _, _ := newUnauthorizedRefreshFixture(t, false)

	executor.mu.Lock()
	executor.refreshErr = errors.New("upstream 503 Service Unavailable")
	executor.mu.Unlock()

	// Direct call to refreshAuthForRequest with the rejected access token.
	_, errRefresh := m.refreshAuthForRequest(context.Background(), primary.ID, authAccessToken(primary))
	if errRefresh == nil {
		t.Fatal("expected refresh error")
	}

	updatedPrimary, ok := m.GetByID(primary.ID)
	if !ok || updatedPrimary == nil {
		t.Fatal("primary auth missing")
	}
	if !updatedPrimary.Unavailable {
		t.Fatalf("primary.Unavailable = false, want true after rejected token refresh failed")
	}
	if updatedPrimary.Status != StatusError {
		t.Fatalf("primary.Status = %q, want StatusError", updatedPrimary.Status)
	}
}

// TestManager_Issue6415_RefreshSuccess_PreservesActiveQuotaCooldown reproduces
// Issue #6415 related symptom:
// A successful 401 refresh must not wipe an independent, still-active quota cooldown on that model.
func TestManager_Issue6415_RefreshSuccess_PreservesActiveQuotaCooldown(t *testing.T) {
	m, _, primary, _, model := newUnauthorizedRefreshFixture(t, false)

	// Set active quota cooldown on primary for model.
	now := time.Now()
	quotaRecover := now.Add(30 * time.Minute)
	primaryAuth, ok := m.GetByID(primary.ID)
	if !ok || primaryAuth == nil {
		t.Fatal("primary auth missing")
	}
	primaryAuth.ModelStates = map[string]*ModelState{
		model: {
			Status:         StatusError,
			StatusMessage:  "unauthorized",
			Unavailable:    true,
			NextRetryAfter: quotaRecover,
			Quota: QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: quotaRecover,
			},
			LastError: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		},
	}
	if _, errUpdate := m.Update(context.Background(), primaryAuth); errUpdate != nil {
		t.Fatalf("update primary auth: %v", errUpdate)
	}

	// Trigger 401 refresh on primary.
	_, errRefresh := m.ForceRefreshAuth(context.Background(), primary.ID)
	if errRefresh != nil {
		t.Fatalf("ForceRefreshAuth error = %v", errRefresh)
	}

	updatedPrimary, ok := m.GetByID(primary.ID)
	if !ok || updatedPrimary == nil {
		t.Fatal("primary auth missing after refresh")
	}
	ms := updatedPrimary.ModelStates[model]
	if ms == nil {
		t.Fatalf("model state for %q missing after refresh", model)
	}
	if !ms.Quota.Exceeded {
		t.Fatalf("model state Quota.Exceeded = false, want true (quota cooldown was discarded)")
	}
	if !ms.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("model state Quota.NextRecoverAt = %v, want %v", ms.Quota.NextRecoverAt, quotaRecover)
	}
}

// TestManager_Issue6415_ExecuteRetrySuccess_PreservesActiveQuotaCooldown tests
// the full Execute path (401 -> refresh -> retry success) and ensures both sibling
// models and the retried model with active quota cooldown keep their quota state.
func TestManager_Issue6415_ExecuteRetrySuccess_PreservesActiveQuotaCooldown(t *testing.T) {
	m, executor, primary, _, model := newUnauthorizedRefreshFixture(t, false)

	siblingModel := "gpt-5-mini"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(primary.ID, "codex", []*registry.ModelInfo{{ID: model}, {ID: siblingModel}})
	t.Cleanup(func() {
		reg.UnregisterClient(primary.ID)
	})

	now := time.Now()
	quotaRecover := now.Add(45 * time.Minute)
	primaryAuth, ok := m.GetByID(primary.ID)
	if !ok || primaryAuth == nil {
		t.Fatal("primary auth missing")
	}
	primaryAuth.ModelStates = map[string]*ModelState{
		siblingModel: {
			Status:         StatusError,
			StatusMessage:  "quota_exceeded",
			Unavailable:    true,
			NextRetryAfter: quotaRecover,
			Quota: QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: quotaRecover,
			},
			LastError: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "429 rate limited"},
		},
	}
	if _, errUpdate := m.Update(context.Background(), primaryAuth); errUpdate != nil {
		t.Fatalf("update primary auth: %v", errUpdate)
	}

	// Execute model: triggers 401 on primary, refreshes token, retries, and succeeds.
	resp, errExec := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute error = %v, want success after refresh retry", errExec)
	}
	if got := string(resp.Payload); got != primary.ID+":fresh-access-token" {
		t.Fatalf("payload = %q, want fresh primary response", got)
	}

	if got := executor.RefreshCalls(); got != 1 {
		t.Fatalf("Refresh calls = %d, want 1", got)
	}

	updatedPrimary, ok := m.GetByID(primary.ID)
	if !ok || updatedPrimary == nil {
		t.Fatal("primary auth missing after execution")
	}

	// Verify sibling model still preserves its active quota cooldown.
	siblingState := updatedPrimary.ModelStates[siblingModel]
	if siblingState == nil {
		t.Fatalf("sibling model state %q missing", siblingModel)
	}
	if !siblingState.Quota.Exceeded {
		t.Fatalf("sibling model Quota.Exceeded = false, want true after successful refresh of primary")
	}
	if !siblingState.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("sibling model Quota.NextRecoverAt = %v, want %v", siblingState.Quota.NextRecoverAt, quotaRecover)
	}
}

// TestManager_Issue6415_MarkRejectedAccessToken_AdvancesGeneration verifies
// that markRejectedAccessToken increments Generation so stale in-flight results
// cannot overwrite the rejected credential status in the scheduler.
func TestManager_Issue6415_MarkRejectedAccessToken_AdvancesGeneration(t *testing.T) {
	m, _, primary, _, _ := newUnauthorizedRefreshFixture(t, false)

	initialPrimary, ok := m.GetByID(primary.ID)
	if !ok || initialPrimary == nil {
		t.Fatal("primary auth missing")
	}
	initialGen := initialPrimary.Generation

	m.markRejectedAccessToken(primary.ID, authAccessToken(initialPrimary))

	afterPrimary, ok := m.GetByID(primary.ID)
	if !ok || afterPrimary == nil {
		t.Fatal("primary auth missing after rejection")
	}
	if afterPrimary.Generation <= initialGen {
		t.Fatalf("Generation = %d, want > %d after markRejectedAccessToken", afterPrimary.Generation, initialGen)
	}
	if afterPrimary.RejectedAccessToken != authAccessToken(initialPrimary) {
		t.Fatalf("RejectedAccessToken = %q, want %q", afterPrimary.RejectedAccessToken, authAccessToken(initialPrimary))
	}
}

// TestManager_Issue6415_UpdateWithoutCredentialChange_PreservesRejectedAccessToken
// verifies that hot-reloads or Manager.Update with identical credentials does not
// wipe out the runtime RejectedAccessToken marker.
func TestManager_Issue6415_UpdateWithoutCredentialChange_PreservesRejectedAccessToken(t *testing.T) {
	m, _, primary, _, _ := newUnauthorizedRefreshFixture(t, false)

	token := authAccessToken(primary)
	m.markRejectedAccessToken(primary.ID, token)

	// Simulate config reload or metadata update (e.g. note or weight change, but same credentials).
	reloadAuth := primary.Clone()
	reloadAuth.RejectedAccessToken = "" // Serialized auth from file has empty runtime field
	reloadAuth.Metadata["note"] = "reloaded from file"

	updated, errUpdate := m.Update(context.Background(), reloadAuth)
	if errUpdate != nil {
		t.Fatalf("Update error = %v", errUpdate)
	}
	if updated.RejectedAccessToken != token {
		t.Fatalf("RejectedAccessToken after reload = %q, want %q", updated.RejectedAccessToken, token)
	}

	fetched, ok := m.GetByID(primary.ID)
	if !ok || fetched == nil {
		t.Fatal("primary auth missing")
	}
	if fetched.RejectedAccessToken != token {
		t.Fatalf("fetched RejectedAccessToken = %q, want %q", fetched.RejectedAccessToken, token)
	}
}

// TestManager_Issue6415_RefreshSuccessWithUnchangedToken_ClearsRejectedAccessToken
// verifies that when Refresh succeeds but returns the exact same token string,
// the RejectedAccessToken marker is cleared and not erroneously restored.
func TestManager_Issue6415_RefreshSuccessWithUnchangedToken_ClearsRejectedAccessToken(t *testing.T) {
	m, executor, primary, _, model := newUnauthorizedRefreshFixture(t, false)

	token := authAccessToken(primary)
	m.markRejectedAccessToken(primary.ID, token)

	// Executor Refresh returns the same token string (e.g. session refreshed without rotating key).
	executor.mu.Lock()
	executor.refreshTokens[primary.ID] = token
	executor.mu.Unlock()

	refreshed, errRefresh := m.ForceRefreshAuth(context.Background(), primary.ID)
	if errRefresh != nil {
		t.Fatalf("ForceRefreshAuth error = %v", errRefresh)
	}
	if refreshed.RejectedAccessToken != "" {
		t.Fatalf("refreshed.RejectedAccessToken = %q, want empty after successful refresh", refreshed.RejectedAccessToken)
	}

	fetched, ok := m.GetByID(primary.ID)
	if !ok || fetched == nil {
		t.Fatal("primary auth missing")
	}
	if fetched.RejectedAccessToken != "" {
		t.Fatalf("fetched.RejectedAccessToken = %q, want empty after successful refresh", fetched.RejectedAccessToken)
	}

	blocked, _, _ := isAuthBlockedForModel(fetched, model, time.Now())
	if blocked {
		t.Fatalf("primary is blocked after successful refresh with unchanged token")
	}
}

// TestManager_Issue6415_RefreshCanceled_ReschedulesRefresh verifies that if
// a refresh is canceled via context, a retry is scheduled so the rejected credential
// does not remain indefinitely unselectable without a retry attempt.
func TestManager_Issue6415_RefreshCanceled_ReschedulesRefresh(t *testing.T) {
	m, executor, primary, _, _ := newUnauthorizedRefreshFixture(t, false)

	executor.mu.Lock()
	executor.refreshErr = context.Canceled
	executor.mu.Unlock()

	_, err := m.refreshAuthForRequest(context.Background(), primary.ID, authAccessToken(primary))
	if err == nil {
		t.Fatal("expected canceled error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	fetched, ok := m.GetByID(primary.ID)
	if !ok || fetched == nil {
		t.Fatal("primary auth missing")
	}
	if fetched.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter should be scheduled after refresh cancellation, got zero")
	}
}
