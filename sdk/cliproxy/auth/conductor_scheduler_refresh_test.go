package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type schedulerProviderTestExecutor struct {
	provider string
}

func (e schedulerProviderTestExecutor) Identifier() string { return e.provider }

func (e schedulerProviderTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e schedulerProviderTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e schedulerProviderTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type terminalRefreshExecutor struct {
	schedulerProviderTestExecutor
	calls     int
	seenToken string
}

func (e *terminalRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	if auth != nil && auth.Metadata != nil {
		e.seenToken, _ = auth.Metadata["refresh_token"].(string)
	}
	return nil, errors.New(`token refresh failed: status=401 content_type=application/json content_encoding=<empty> body_preview="{\"error\":\"invalid_grant\",\"error_description\":\"Refresh token has already been used\"}"`)
}

type captureStore struct {
	saved []*Auth
}

func (s *captureStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *captureStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth != nil {
		s.saved = append(s.saved, auth.Clone())
	}
	return "", nil
}

func (s *captureStore) Delete(context.Context, string) error { return nil }

func (s *captureStore) last() *Auth {
	if len(s.saved) == 0 {
		return nil
	}
	return s.saved[len(s.saved)-1]
}

func TestManagerRefreshAuth_DisablesRefreshAfterRefreshTokenReused(t *testing.T) {
	ctx := context.Background()
	store := &captureStore{}
	manager := NewManager(store, nil, nil)
	executor := &terminalRefreshExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "codex-refresh-reused",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token":            "old-refresh-token",
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.refreshAuth(ctx, auth.ID)
	if executor.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", executor.calls)
	}
	if executor.seenToken != "old-refresh-token" {
		t.Fatalf("refresh token used = %q, want original token", executor.seenToken)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if !updated.RefreshDisabled() {
		t.Fatal("RefreshDisabled() = false, want true after terminal reuse error")
	}
	if updated.Status != StatusError || updated.StatusMessage != "reauth_required" {
		t.Fatalf("status = %q/%q, want error/reauth_required", updated.Status, updated.StatusMessage)
	}
	if updated.LastError == nil || updated.LastError.Code != "reauth_required" || updated.LastError.Retryable {
		t.Fatalf("LastError = %+v, want non-retryable reauth_required", updated.LastError)
	}
	if strings.Contains(updated.LastError.Message, "old-refresh-token") {
		t.Fatalf("LastError message leaked refresh token: %q", updated.LastError.Message)
	}
	if got, _ := updated.Metadata["last_refresh_error"].(string); got == "" || strings.Contains(got, "old-refresh-token") {
		t.Fatalf("last_refresh_error = %q, want visible message without token", got)
	}
	if _, shouldSchedule := nextRefreshCheckAt(time.Now(), updated, time.Second); shouldSchedule {
		t.Fatal("nextRefreshCheckAt() scheduled terminal reauth auth, want unscheduled")
	}

	manager.refreshAuth(ctx, auth.ID)
	if executor.calls != 1 {
		t.Fatalf("refresh calls after second manager refresh = %d, want still 1", executor.calls)
	}
	saved := store.last()
	if saved == nil || !saved.RefreshDisabled() {
		t.Fatalf("persisted auth RefreshDisabled() = %v, want true", saved != nil && saved.RefreshDisabled())
	}
}

type invalidGrantRefreshExecutor struct {
	schedulerProviderTestExecutor
	calls int
}

func (e *invalidGrantRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	// Production incident error: Claude refused the refresh token with an OAuth
	// invalid_grant that is NOT a reuse phrasing, so it previously fell through
	// to the transient 5-minute retry path instead of reauth-required.
	return nil, errors.New(`token refresh failed: status=400 content_type=application/json body_preview="{\"error\":\"invalid_grant\",\"error_description\":\"Refresh token not found or invalid\"}"`)
}

// TestManagerRefreshAuth_DisablesRefreshAfterInvalidGrant covers T007: a Claude
// invalid_grant refresh failure must be persisted as terminal reauth-required
// and must stop the auto-refresh loop, instead of retrying the dead token every
// 5 minutes.
func TestManagerRefreshAuth_DisablesRefreshAfterInvalidGrant(t *testing.T) {
	ctx := context.Background()
	store := &captureStore{}
	manager := NewManager(store, nil, nil)
	executor := &invalidGrantRefreshExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "claude-invalid-grant",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_token":            "dead-refresh-token",
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.refreshAuth(ctx, auth.ID)
	if executor.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", executor.calls)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to remain registered")
	}
	if !updated.RefreshDisabled() {
		t.Fatal("RefreshDisabled() = false, want true after terminal invalid_grant")
	}
	if updated.Status != StatusError || updated.StatusMessage != "reauth_required" {
		t.Fatalf("status = %q/%q, want error/reauth_required", updated.Status, updated.StatusMessage)
	}
	if got, _ := updated.Metadata["refresh_error_code"].(string); got != "invalid_grant" {
		t.Fatalf("refresh_error_code = %q, want invalid_grant", got)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero (terminal, no retry)", updated.NextRefreshAfter)
	}
	if updated.LastError == nil || updated.LastError.Retryable {
		t.Fatalf("LastError = %+v, want non-retryable", updated.LastError)
	}
	if msg, _ := updated.Metadata["last_refresh_error"].(string); msg == "" ||
		strings.Contains(msg, "dead-refresh-token") || strings.Contains(strings.ToLower(msg), "body_preview") {
		t.Fatalf("last_refresh_error = %q, want sanitized message without token/raw body", msg)
	}

	// The terminal state is persisted and a second refresh tick does not retry.
	saved := store.last()
	if saved == nil || !saved.RefreshDisabled() {
		t.Fatalf("persisted auth RefreshDisabled() = %v, want true", saved != nil && saved.RefreshDisabled())
	}
	manager.refreshAuth(ctx, auth.ID)
	if executor.calls != 1 {
		t.Fatalf("refresh calls after second tick = %d, want still 1 (no retry of dead token)", executor.calls)
	}
}

type unauthorizedRefreshTestExecutor struct {
	schedulerProviderTestExecutor
}

func (e unauthorizedRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return nil, errors.New("token refresh failed with status 401: invalid_grant")
}

func TestManager_RefreshAuthUnauthorizedFailureStopsAutoRefreshRetry(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(unauthorizedRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
	})

	auth := &Auth{
		ID:       "unauthorized-refresh",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "x@example.com",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after refresh", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected unauthorized refresh failure to be recorded")
	}
	if got := updated.LastError.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("LastError.StatusCode() = %d, want %d", got, http.StatusUnauthorized)
	}
	if updated.LastError.Code != "unauthorized" {
		t.Fatalf("LastError.Code = %q, want unauthorized", updated.LastError.Code)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero for unauthorized refresh failure", updated.NextRefreshAfter)
	}
	now := time.Now()
	if manager.shouldRefresh(updated, now) {
		t.Fatal("expected unauthorized auth to stop refresh attempts")
	}
	if _, shouldSchedule := nextRefreshCheckAt(now, updated, time.Second); shouldSchedule {
		t.Fatal("expected unauthorized auth to be removed from the auto-refresh schedule")
	}
}

func TestManager_RefreshSchedulerEntry_RebuildsSupportedModelSetAfterModelRegistration(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name  string
		prime func(*Manager, *Auth) error
	}{
		{
			name: "register",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				return errRegister
			},
		},
		{
			name: "update",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				if errRegister != nil {
					return errRegister
				}
				updated := auth.Clone()
				updated.Metadata = map[string]any{"updated": true}
				_, errUpdate := manager.Update(ctx, updated)
				return errUpdate
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auth := &Auth{
				ID:       "refresh-entry-" + testCase.name,
				Provider: "gemini",
			}
			if errPrime := testCase.prime(manager, auth); errPrime != nil {
				t.Fatalf("prime auth %s: %v", testCase.name, errPrime)
			}

			registerSchedulerModels(t, "gemini", "scheduler-refresh-model", auth.ID)

			got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			var authErr *Error
			if !errors.As(errPick, &authErr) || authErr == nil {
				t.Fatalf("pickSingle() before refresh error = %v, want auth_not_found", errPick)
			}
			if authErr.Code != "auth_not_found" {
				t.Fatalf("pickSingle() before refresh code = %q, want %q", authErr.Code, "auth_not_found")
			}
			if got != nil {
				t.Fatalf("pickSingle() before refresh auth = %v, want nil", got)
			}

			manager.RefreshSchedulerEntry(auth.ID)

			got, errPick = manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickSingle() after refresh error = %v", errPick)
			}
			if got == nil || got.ID != auth.ID {
				t.Fatalf("pickSingle() after refresh auth = %v, want %q", got, auth.ID)
			}
		})
	}
}

func TestManager_PickNext_RebuildsSchedulerAfterModelCooldownError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "gemini"})

	registerSchedulerModels(t, "gemini", "scheduler-cooldown-rebuild-model", "cooldown-stale-old")

	oldAuth := &Auth{
		ID:       "cooldown-stale-old",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, oldAuth); errRegister != nil {
		t.Fatalf("register old auth: %v", errRegister)
	}

	planQuotaRetry := 30 * time.Minute
	manager.MarkResult(ctx, Result{
		AuthID:     oldAuth.ID,
		Provider:   "gemini",
		Model:      "scheduler-cooldown-rebuild-model",
		Success:    false,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
		RetryAfter: &planQuotaRetry,
	})

	newAuth := &Auth{
		ID:       "cooldown-stale-new",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, newAuth); errRegister != nil {
		t.Fatalf("register new auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(newAuth.ID, "gemini", []*registry.ModelInfo{{ID: "scheduler-cooldown-rebuild-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(newAuth.ID)
	})

	got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() before sync error = %v, want modelCooldownError", errPick)
	}
	if got != nil {
		t.Fatalf("pickSingle() before sync auth = %v, want nil", got)
	}

	got, executor, errPick := manager.pickNext(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if executor == nil {
		t.Fatal("pickNext() executor = nil")
	}
	if got == nil || got.ID != newAuth.ID {
		t.Fatalf("pickNext() auth = %v, want %q", got, newAuth.ID)
	}
}
