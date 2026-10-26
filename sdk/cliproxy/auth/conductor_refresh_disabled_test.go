package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type mockOAuthErrorExecutor struct {
	id           string
	refreshCalls atomic.Int32
	errToReturn  error
}

func (e *mockOAuthErrorExecutor) Identifier() string { return e.id }

func (e *mockOAuthErrorExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *mockOAuthErrorExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *mockOAuthErrorExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	if e.errToReturn != nil {
		return nil, e.errToReturn
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "new-valid-token"
	return auth, nil
}

func (e *mockOAuthErrorExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *mockOAuthErrorExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type oauthStatusError struct {
	code int
	msg  string
}

func (e oauthStatusError) Error() string   { return fmt.Sprintf("status %d: %s", e.code, e.msg) }
func (e oauthStatusError) StatusCode() int { return e.code }

func TestRefreshAuthForRequest_NormalDisabledAuth_RefreshesTokenSuccessfully(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "test-provider"}
	manager.RegisterExecutor(executor)

	// Normal disabled auth without invalid_grant should refresh tokens normally (expected behavior)
	auth := &Auth{
		ID:       "normal-disabled-auth",
		Provider: "test-provider",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, "")
	if errRefresh != nil {
		t.Fatalf("expected successful refresh for normal disabled auth, got error: %v", errRefresh)
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatalf("executor.Refresh called %d times, want 1 for normal disabled auth", executor.refreshCalls.Load())
	}
	if refreshed.Metadata["access_token"] != "new-valid-token" {
		t.Fatalf("refreshed token = %v, want new-valid-token", refreshed.Metadata["access_token"])
	}
}

func TestRefreshAuthForRequest_DisabledAuth_InvalidGrant_NeverRetries(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	invalidGrantErr := oauthStatusError{
		code: http.StatusBadRequest,
		msg:  `{"error": "invalid_grant", "error_description": "Bad Request"}`,
	}
	executor := &mockOAuthErrorExecutor{
		id:          "test-provider",
		errToReturn: invalidGrantErr,
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "disabled-invalid-grant",
		Provider: "test-provider",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	// 1st call encounters invalid_grant
	_, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, "")
	if errRefresh == nil {
		t.Fatalf("expected refresh error, got nil")
	}

	manager.mu.RLock()
	current := manager.auths[auth.ID]
	manager.mu.RUnlock()

	if current == nil {
		t.Fatalf("auth not found in manager")
	}
	// Disabled + invalid_grant must never schedule retry
	// Terminal refresh state is visible independently of the operator's disabled flag.
	if !current.Disabled || current.Status != StatusError || !current.RefreshDisabled() {
		t.Fatalf("disabled credential lost its terminal lock: disabled=%t status=%v", current.Disabled, current.Status)
	}
	if !current.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero time (never refresh)", current.NextRefreshAfter)
	}

	// 2nd call should be blocked immediately without calling executor
	callsBefore := executor.refreshCalls.Load()
	lockedAgain, errSecond := manager.refreshAuthForRequest(ctx, auth.ID, "")
	if errSecond != nil || lockedAgain == nil || !lockedAgain.RefreshDisabled() || !lockedAgain.Disabled {
		t.Fatal("terminal short-circuit must return the unchanged locked disabled credential")
	}
	if executor.refreshCalls.Load() != callsBefore {
		t.Fatalf("executor was called again for disabled+invalid_grant, calls=%d", executor.refreshCalls.Load())
	}
}

func TestRefreshAuthForRequest_EnabledAuth_InvalidGrant_RequiresReauthentication(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "test-provider", errToReturn: oauthStatusError{
		code: http.StatusBadRequest,
		msg:  `{"error":"invalid_grant","error_description":"Bad Request"}`,
	}}
	manager.RegisterExecutor(executor)
	auth := &Auth{ID: "enabled-auth-invalid-grant", Provider: "test-provider", ProxyURL: "direct", Status: StatusActive,
		Metadata: map[string]any{"access_token": "expired-token", "refresh_token": "refresh-1", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.refreshAuthForRequest(ctx, auth.ID, ""); err == nil {
		t.Fatal("expected invalid_grant")
	}
	locked, _ := manager.GetByID(auth.ID)
	if locked.Status != StatusError || locked.Disabled || !locked.RefreshDisabled() || !locked.NextRefreshAfter.IsZero() {
		t.Fatal("invalid_grant must require reauthentication without changing operator disabled state")
	}
	// A dead token must not enter the upstream exponential retry loop.
	for i := 0; i < 2; i++ {
		again, err := manager.ForceRefreshAuth(ctx, auth.ID)
		if err != nil || again == nil || !again.RefreshDisabled() || again.Metadata["access_token"] != "expired-token" {
			t.Fatal("terminal short-circuit must preserve the locked token")
		}
	}
	stillLocked, _ := manager.GetByID(auth.ID)
	if executor.refreshCalls.Load() != 1 || stillLocked.RefreshFailures != 0 {
		t.Fatalf("terminal token entered retry loop: calls=%d retries=%d", executor.refreshCalls.Load(), stillLocked.RefreshFailures)
	}
	replacement := &Auth{ID: auth.ID, Provider: auth.Provider, ProxyURL: "direct", Status: StatusActive,
		Metadata: map[string]any{"access_token": "reauth-access", "refresh_token": "refresh-2", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	MergeExistingAuthMetadata(replacement, locked.Metadata)
	registered, err := manager.Register(WithAuthCreationIntent(ctx), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if registered.RefreshDisabled() || registered.RegistrationEpoch <= locked.RegistrationEpoch || registered.CredentialVersion <= locked.CredentialVersion {
		t.Fatal("public reauthentication did not replace locked credential generation")
	}
	executor.errToReturn = nil
	refreshed, err := manager.ForceRefreshAuth(ctx, auth.ID)
	if err != nil || refreshed.Metadata["access_token"] != "new-valid-token" || executor.refreshCalls.Load() != 2 {
		t.Fatalf("new credential failed refresh: err=%v calls=%d", err, executor.refreshCalls.Load())
	}
}

// Preserve the upstream delay algorithm independently of the fork's terminal
// invalid_grant policy: the same revoked refresh token never reaches this loop.
func TestInvalidGrantBackoffDuration(t *testing.T) {
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if got := invalidGrantBackoffDuration(i + 1); got != want {
			t.Fatalf("attempt %d backoff=%v want=%v", i+1, got, want)
		}
	}
}

func TestManager_AutoRefreshLoop_DisabledAuthWithInvalidGrantNeverRefreshes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "test-provider"}
	manager.RegisterExecutor(executor)

	now := time.Now()
	expiredAt := now.Add(-time.Hour).Format(time.RFC3339)
	auth := &Auth{
		ID:       "disabled-invalid-grant-loop",
		Provider: "test-provider",
		Disabled: true,
		Status:   StatusDisabled,
		LastError: &Error{
			HTTPStatus: 400,
			Message:    `{"error": "invalid_grant", "error_description": "Bad Request"}`,
		},
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
			"expires_at":    expiredAt,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	manager.StartAutoRefresh(ctx, 10*time.Millisecond)
	defer manager.StopAutoRefresh()

	// Wait 100ms to allow refresh loop cycles
	time.Sleep(100 * time.Millisecond)

	if calls := executor.refreshCalls.Load(); calls != 0 {
		t.Fatalf("executor.Refresh called %d times for disabled+invalid_grant auth, want 0", calls)
	}
}

func TestRefreshAuthForRequest_RawFmtError_RecognizesInvalidGrant(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	// Error created via fmt.Errorf without implementing StatusCode()
	rawErr := fmt.Errorf("oauth token refresh failed: invalid_grant: account checkpoint required")
	executor := &mockOAuthErrorExecutor{
		id:          "test-provider",
		errToReturn: rawErr,
	}
	manager.RegisterExecutor(executor)

	now := time.Now()
	expiredAt := now.Add(-time.Hour).Format(time.RFC3339)
	auth := &Auth{
		ID:       "raw-fmt-invalid-grant",
		Provider: "test-provider",
		Disabled: false,
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
			"expires_at":    expiredAt,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	_, _ = manager.refreshAuthForRequest(ctx, auth.ID, "")
	manager.mu.RLock()
	current := manager.auths[auth.ID]
	manager.mu.RUnlock()

	if !current.RefreshDisabled() || current.Metadata["refresh_error_code"] != "invalid_grant" || !current.NextRefreshAfter.IsZero() {
		t.Fatal("raw fmt invalid_grant was not recognized as terminal")
	}
	again, err := manager.ForceRefreshAuth(ctx, auth.ID)
	if err != nil || again == nil || !again.RefreshDisabled() {
		t.Fatal("raw terminal error must preserve the locked credential")
	}
	if executor.refreshCalls.Load() != 1 || current.RefreshFailures != 0 {
		t.Fatalf("raw terminal error was retried: calls=%d retries=%d", executor.refreshCalls.Load(), current.RefreshFailures)
	}
}
