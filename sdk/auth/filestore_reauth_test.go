package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func TestSelectedClaudeFileOwnerWhitespace(t *testing.T) {
	for _, fixture := range []struct {
		name, old, account, organization string
		rejectedAccounts                 []string
	}{
		{
			name:         "padded UUIDs",
			old:          `{"type":"claude","account_uuid":"  ab12cdef-1234-4abc-8def-abcdefabcdef  ","organization_uuid":"  ed34abcd-1234-4def-8abc-abcdefabcdef\t","access_token":"old","note":"keep"}`,
			account:      " AB12CDEF-1234-4ABC-8DEF-ABCDEFABCDEF ",
			organization: "ED34ABCD-1234-4DEF-8ABC-ABCDEFABCDEF",
		},
		{
			name:             "opaque account and blank organization",
			old:              `{"type":"claude","account_uuid":" Opaque-Owner\t","organization_uuid":" \t ","access_token":"old","note":"keep"}`,
			account:          "Opaque-Owner",
			organization:     "new-org",
			rejectedAccounts: []string{" opaque-owner ", "   "},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "fixture.json"
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(fixture.old), 0600); err != nil {
				t.Fatal(err)
			}
			store := NewFileTokenStore()
			store.SetBaseDir(dir)
			file, err := ReadAccountSettingsFile(dir, path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithSelectedClaudeLoginFile(context.Background(), file, func() (func(), bool) {
				return func() {}, true
			})
			save := func(account string) error {
				_, err := store.Save(ctx, &cliproxyauth.Auth{
					ID: name, FileName: name, Provider: "claude",
					Storage: &claude.ClaudeTokenStorage{
						AccountUUID: account, OrganizationUUID: fixture.organization,
						AccessToken: "updated-access", RefreshToken: "updated-refresh",
					},
				})
				return err
			}
			for _, account := range fixture.rejectedAccounts {
				if err := save(account); !errors.Is(err, cliproxyauth.ErrFileRemovalBusy) {
					t.Fatalf("unexpected result for an unmatched account: %v", err)
				}
				raw, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, []byte(fixture.old)) {
					t.Fatalf("rejected update changed the file: %v", err)
				}
			}
			if !file.MatchesClaudeLoginOwner("", fixture.account, fixture.organization) {
				t.Fatal("preflight rejected the equivalent saved account")
			}
			if err := save(fixture.account); err != nil {
				t.Fatalf("CAS rejected the equivalent saved account: %v", err)
			}
			raw, err := os.ReadFile(path)
			var persisted struct {
				Access string `json:"access_token"`
				Note   string `json:"note"`
			}
			if err != nil || json.Unmarshal(raw, &persisted) != nil || persisted.Access != "updated-access" || persisted.Note != "keep" {
				t.Fatal("CAS did not write the new grant while preserving the note")
			}
		})
	}
	if matchesClaudeLoginOwner([]byte(`{"type":"claude","account_uuid":" "}`), "", " ", "") {
		t.Fatal("a blank saved account cannot establish ownership")
	}
}
