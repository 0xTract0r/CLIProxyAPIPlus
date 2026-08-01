package store

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPostgresRestore_ReauthRequiredRestoresTerminalLock covers the
// postgres-backed store's side of the reauth_required restart-persistence gap.
// PostgresStore.List runs, in order, the disabled restore, then
// applyQuarantineStateFromMetadata, then
// cliproxyauth.ApplyReauthRequiredStateFromMetadata (see List). This test
// exercises that same composition (no live Postgres connection required) so the
// terminal reauth lock is restored to StatusError/Unavailable on load instead
// of silently reloading as a fresh StatusActive credential.
func TestPostgresRestore_ReauthRequiredRestoresTerminalLock(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "a", Status: cliproxyauth.StatusActive}
	metadata := map[string]any{
		"type":               "claude",
		"reauth_required":    true,
		"refresh_status":     "reauth_required",
		"refresh_error_code": "invalid_grant",
	}
	auth.Metadata = metadata

	// Mirror PostgresStore.List's restore tail.
	applyQuarantineStateFromMetadata(auth, metadata)
	cliproxyauth.ApplyReauthRequiredStateFromMetadata(auth)

	if auth.Status != cliproxyauth.StatusError {
		t.Fatalf("Status = %q, want %q", auth.Status, cliproxyauth.StatusError)
	}
	if auth.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q, want %q", auth.StatusMessage, "reauth_required")
	}
	if !auth.Unavailable {
		t.Fatalf("Unavailable = false, want true")
	}
}

// TestPostgresRestore_QuarantineWinsOverReauth confirms the store's restore
// order preserves the priority quarantine > reauth: a row carrying both locks
// keeps StatusQuarantined for display (both still force it off the rotation).
func TestPostgresRestore_QuarantineWinsOverReauth(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "a", Status: cliproxyauth.StatusActive}
	metadata := map[string]any{
		"type":              "claude",
		"auto_quarantined":  true,
		"quarantine_reason": "terminal_auth_failure",
		"reauth_required":   true,
		"refresh_status":    "reauth_required",
	}
	auth.Metadata = metadata

	applyQuarantineStateFromMetadata(auth, metadata)
	cliproxyauth.ApplyReauthRequiredStateFromMetadata(auth)

	if !auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false, want true")
	}
	if auth.Status != cliproxyauth.StatusQuarantined {
		t.Fatalf("Status = %q, want %q (quarantine takes display precedence)", auth.Status, cliproxyauth.StatusQuarantined)
	}
	if !auth.Unavailable {
		t.Fatalf("Unavailable = false, want true")
	}
}
