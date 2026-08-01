package auth

import (
	"context"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// reauthRequiredMetadata mirrors exactly what markRefreshReauthRequiredWithReason
// (sdk/cliproxy/auth/types.go) persists when a refresh token is rejected as
// terminally invalid, so this disk round-trip exercises the real on-disk shape.
func reauthRequiredMetadata(code string) map[string]any {
	return map[string]any{
		"type":                    "claude",
		"refresh_disabled":        true,
		"refresh_status":          "reauth_required",
		"refresh_error_code":      code,
		"refresh_disabled_reason": "reauth_required",
		"reauth_required":         true,
		"refresh_disabled_at":     time.Now().UTC().Format(time.RFC3339),
		"last_refresh_error":      "refresh token is no longer valid; sign in again to reconnect this account",
	}
}

// TestFileTokenStore_Save_ReauthRequiredRoundTrips is a real (no
// WithSkipPersist -- store.Save/List are called directly) disk round-trip
// covering the reauth_required restart-persistence gap: a credential whose
// refresh token is terminally dead must still read back as abnormal
// (StatusError / Unavailable=true) after a fresh FileTokenStore instance
// re-lists the same directory, simulating a CPA process restart. Before the
// fix the cold load rebuilt it as a fresh StatusActive credential (false
// green) that kept getting routed and 401-ing.
func TestFileTokenStore_Save_ReauthRequiredRoundTrips(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	auth := &cliproxyauth.Auth{
		ID:            "reauth.json",
		Provider:      "claude",
		FileName:      "reauth.json",
		Status:        cliproxyauth.StatusError,
		StatusMessage: "reauth_required",
		Unavailable:   true,
		Metadata:      reauthRequiredMetadata("invalid_grant"),
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// A brand new store instance re-reading the same directory simulates a
	// process restart: nothing from the in-memory Auth struct survives, only
	// whatever was actually written to disk.
	reloaded := NewFileTokenStore()
	reloaded.SetBaseDir(baseDir)
	auths, err := reloaded.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("List() len = %d, want 1", len(auths))
	}
	got := auths[0]

	if got.Status != cliproxyauth.StatusError {
		t.Fatalf("Status = %q after restart round-trip, want %q", got.Status, cliproxyauth.StatusError)
	}
	if got.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q after restart round-trip, want %q", got.StatusMessage, "reauth_required")
	}
	if !got.Unavailable {
		t.Fatalf("Unavailable = false after restart round-trip, want true")
	}
}

// TestFileTokenStore_Save_ClearedReauthRoundTrips covers the recovery side:
// after a completed re-auth removes the lock keys, a second Save()+fresh-List()
// must show the credential as active again (not stuck abnormal), proving the
// read-back is self-clearing and never introduces a reverse dead-lock.
func TestFileTokenStore_Save_ClearedReauthRoundTrips(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	auth := &cliproxyauth.Auth{
		ID:            "reauth.json",
		Provider:      "claude",
		FileName:      "reauth.json",
		Status:        cliproxyauth.StatusError,
		StatusMessage: "reauth_required",
		Unavailable:   true,
		Metadata:      reauthRequiredMetadata("invalid_grant"),
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() (reauth) error: %v", err)
	}

	// Simulate clearAuthReauthRequiredLock's effect: drop the lock keys and add
	// fresh token material, then Save again -- this is what a completed re-auth
	// / OAuth callback does before store.Save.
	cleared := auth.Clone()
	cleared.Status = cliproxyauth.StatusActive
	cleared.StatusMessage = ""
	cleared.Unavailable = false
	for _, k := range []string{"reauth_required", "refresh_status", "refresh_error_code", "refresh_disabled_reason", "refresh_disabled", "refresh_disabled_at", "last_refresh_error"} {
		delete(cleared.Metadata, k)
	}
	cleared.Metadata["access_token"] = "fresh-token"

	if _, err := store.Save(ctx, cleared); err != nil {
		t.Fatalf("Save() (cleared) error: %v", err)
	}

	reloaded := NewFileTokenStore()
	reloaded.SetBaseDir(baseDir)
	auths, err := reloaded.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("List() len = %d, want 1", len(auths))
	}
	got := auths[0]

	if got.Status != cliproxyauth.StatusActive {
		t.Fatalf("Status = %q after clear+restart round-trip, want %q", got.Status, cliproxyauth.StatusActive)
	}
	if got.Unavailable {
		t.Fatalf("Unavailable = true after clear+restart round-trip, want false")
	}
	if got.StatusMessage == "reauth_required" {
		t.Fatalf("StatusMessage = %q after clear+restart round-trip, want cleared", got.StatusMessage)
	}
}

// TestFileTokenStore_Save_TransientStateDoesNotPersistAsReauth is the
// terminal-vs-transient guard: a credential merely cooling down from a
// transient failure -- which never writes the reauth_required lock keys --
// must never be misread as reauth-required after a restart round trip, even
// though it shares the same StatusError/Unavailable runtime shape while live.
func TestFileTokenStore_Save_TransientStateDoesNotPersistAsReauth(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	auth := &cliproxyauth.Auth{
		ID:             "flaky.json",
		Provider:       "claude",
		FileName:       "flaky.json",
		Status:         cliproxyauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(30 * time.Minute),
		Metadata: map[string]any{
			"type": "claude",
			// Deliberately no reauth_required / refresh_status lock keys.
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	reloaded := NewFileTokenStore()
	reloaded.SetBaseDir(baseDir)
	auths, err := reloaded.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("List() len = %d, want 1", len(auths))
	}
	got := auths[0]

	if got.StatusMessage == "reauth_required" {
		t.Fatalf("StatusMessage = %q after restart round-trip, want not reauth_required (transient cooldown only)", got.StatusMessage)
	}
	if got.Status == cliproxyauth.StatusError && got.Unavailable {
		t.Fatalf("transient-cooldown-only auth restored as StatusError+Unavailable; only the terminal reauth lock should do that")
	}
}
