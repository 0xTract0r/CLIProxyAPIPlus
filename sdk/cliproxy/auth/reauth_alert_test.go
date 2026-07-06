package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// TestReauthAlertURL covers the #163 relative-path URL builder reused by both
// the conductor.go WARN alert and the management API's auth-file listing
// (buildAuthFileEntry). It must reuse the existing
// GET /v0/management/anthropic-auth-url?auth_name=<id> endpoint path exactly
// (no invented URL), URL-escape the id, and degrade to "" for an empty id
// instead of emitting a malformed/partial URL.
func TestReauthAlertURL(t *testing.T) {
	t.Run("builds the existing anthropic-auth-url endpoint path", func(t *testing.T) {
		got := reauthAlertURL("claude-account-1")
		want := "/v0/management/anthropic-auth-url?auth_name=claude-account-1"
		if got != want {
			t.Fatalf("reauthAlertURL(...) = %q, want %q", got, want)
		}
	})

	t.Run("URL-escapes ids with special characters", func(t *testing.T) {
		id := "label with spaces & stuff"
		got := reauthAlertURL(id)
		wantQuery := "auth_name=" + url.QueryEscape(id)
		if !strings.HasSuffix(got, wantQuery) {
			t.Fatalf("reauthAlertURL(%q) = %q, want suffix %q (escaped)", id, got, wantQuery)
		}
		if strings.Contains(got, " ") {
			t.Fatalf("reauthAlertURL(%q) = %q, want no raw spaces (must be escaped)", id, got)
		}
	})

	t.Run("empty id degrades to empty string, not a malformed URL", func(t *testing.T) {
		if got := reauthAlertURL(""); got != "" {
			t.Fatalf("reauthAlertURL(\"\") = %q, want \"\"", got)
		}
		if got := reauthAlertURL("   "); got != "" {
			t.Fatalf("reauthAlertURL(whitespace) = %q, want \"\"", got)
		}
	})

	t.Run("exported wrapper matches the unexported builder", func(t *testing.T) {
		if got, want := ReauthAlertURL("claude-x"), reauthAlertURL("claude-x"); got != want {
			t.Fatalf("ReauthAlertURL(...) = %q, want %q (must match reauthAlertURL)", got, want)
		}
	})
}

// terminalRefreshExecutorForAlertTest is a minimal executor that always
// returns a terminal invalid_grant refresh error, used to exercise the
// conductor.go reauth-alert WARN log without needing a real provider.
type terminalRefreshExecutorForAlertTest struct {
	schedulerProviderTestExecutor
	calls int
}

func (e *terminalRefreshExecutorForAlertTest) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	return nil, errors.New(`token refresh failed: status=400 body_preview="{\"error\":\"invalid_grant\",\"error_description\":\"Refresh token not found or invalid\"}"`)
}

// findAlertWarnEntry returns the first captured WARN-level "reauth required"
// alert entry, or nil if none was emitted.
func findAlertWarnEntry(entries []*log.Entry) *log.Entry {
	for _, entry := range entries {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "reauth required") {
			return entry
		}
	}
	return nil
}

// TestReauthAlert_FiresOnceOnLockTransition covers the #163 requirement that
// the semi-automatic alert fires exactly once at the untracked -> locked
// transition, not on every refresh retry. It reuses the same edge-triggered
// branch as the #164 diagnostic log (RefreshDisabled() short-circuits before
// refreshAuth() reaches the terminal-error branch on subsequent calls), so a
// second refreshAuth() call against an already-locked credential must not
// emit a second alert.
func TestReauthAlert_FiresOnceOnLockTransition(t *testing.T) {
	hook := attachTestAlertHook(t)

	ctx := context.Background()
	store := &captureStore{}
	manager := NewManager(store, nil, nil)
	executor := &terminalRefreshExecutorForAlertTest{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-alert-account",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_token":            "dead-refresh-token",
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// First refresh attempt: untracked -> locked transition. Must alert once
	// and include the reauth_url field pointing at the auth-scoped endpoint.
	manager.refreshAuth(ctx, auth.ID)

	warnEntries := countAlertWarnEntries(hook.AllEntries())
	if warnEntries != 1 {
		t.Fatalf("alert WARN entries after first lock = %d, want 1", warnEntries)
	}
	entry := findAlertWarnEntry(hook.AllEntries())
	if entry == nil {
		t.Fatal("expected a reauth-required WARN alert entry")
	}
	if got, _ := entry.Data["reauth_url"].(string); got != "/v0/management/anthropic-auth-url?auth_name=claude-alert-account" {
		t.Fatalf("alert reauth_url = %q, want the anthropic-auth-url endpoint for this auth", got)
	}
	if got, _ := entry.Data["auth_ref"].(string); got != auth.ID {
		t.Fatalf("alert auth_ref = %q, want %q", got, auth.ID)
	}
	if _, tokenLeaked := entry.Data["cred_fp"]; tokenLeaked {
		t.Fatal("alert must not carry a raw/derived token field; that belongs only to the #164 diagnostic Error log")
	}
	for key, value := range entry.Data {
		if strings.Contains(fmtString(value), "dead-refresh-token") {
			t.Fatalf("alert field %q leaked the raw refresh token: %v", key, value)
		}
	}

	hook.Reset()

	// Second refresh tick against the now-locked credential must not alert
	// again (edge-triggered, no spam per retry).
	manager.refreshAuth(ctx, auth.ID)
	if got := countAlertWarnEntries(hook.AllEntries()); got != 0 {
		t.Fatalf("alert WARN entries after already-locked retry = %d, want 0 (no repeat alert)", got)
	}
}

// TestReauthAlert_OperatorDisableDoesNotTriggerAlert covers the #163
// requirement that an operator's explicit refresh-disable (not the automatic
// terminal lock) never triggers the reauth alert: refreshAuth() short-circuits
// on RefreshDisabled() before it can ever reach the terminal-error branch that
// emits the alert, regardless of why RefreshDisabled() is true.
func TestReauthAlert_OperatorDisableDoesNotTriggerAlert(t *testing.T) {
	hook := attachTestAlertHook(t)

	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	executor := &terminalRefreshExecutorForAlertTest{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-operator-disabled",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_token": "some-refresh-token",
			"account_settings": map[string]any{
				"refresh_enabled": false,
			},
		},
	}
	if !auth.RefreshDisabled() {
		t.Fatal("fixture setup: expected operator-disabled auth to report RefreshDisabled() == true")
	}
	if IsReauthRequiredMetadata(auth.Metadata) {
		t.Fatal("fixture setup: operator disable must not set the automatic reauth_required markers")
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.refreshAuth(ctx, auth.ID)

	if executor.calls != 0 {
		t.Fatalf("executor.Refresh calls = %d, want 0 (operator-disabled auth must not even attempt refresh)", executor.calls)
	}
	if got := countAlertWarnEntries(hook.AllEntries()); got != 0 {
		t.Fatalf("alert WARN entries for operator-disabled auth = %d, want 0", got)
	}
}

func countAlertWarnEntries(entries []*log.Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "reauth required") {
			count++
		}
	}
	return count
}

func fmtString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// attachTestAlertHook attaches a logrus/hooks/test capture hook to the
// package-global standard logger (the one logEntryWithRequestID falls back to
// when the context carries no request id, which is the case for these
// synchronous manager.refreshAuth() calls) and detaches it again on test
// cleanup. This avoids copying the logrus.Logger value itself (which embeds a
// sync.Mutex and must never be copied after first use).
func attachTestAlertHook(t *testing.T) *test.Hook {
	t.Helper()
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	})
	return hook
}
