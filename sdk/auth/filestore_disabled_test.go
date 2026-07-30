package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testTokenStorage struct {
	meta map[string]any
}

func (s *testTokenStorage) SetMetadata(meta map[string]any) { s.meta = meta }

func (s *testTokenStorage) SaveTokenToFile(authFilePath string) error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, raw, 0o600)
}

func TestFileTokenStore_Save_DisabledPersistsFlagForTokenStorage(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled.json")

	if err := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	storage := &testTokenStorage{}

	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
}

// TestFileTokenStore_Save_DisabledStatusRoundTrips is a real (no
// WithSkipPersist involved -- store.Save/List are called directly) disk
// round-trip covering the regression where readAuthFiles's disabled/
// auto-quarantined switch's `case disabled:` branch was left with no
// status assignment, so a disabled credential silently read back as
// StatusActive after a process restart even though its persisted
// Metadata["disabled"] was still true. The test above only asserts the
// on-disk metadata; it never asserts the List()-restored Auth.Status,
// which is what the selector/conductor actually gate on (see
// selector.go's isAuthBlockedForModel and conductor_selection.go).
func TestFileTokenStore_Save_DisabledStatusRoundTrips(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled-status.json")

	// Save() deliberately no-ops for a Disabled auth whose file does not yet
	// exist (see the os.IsNotExist guard in Save()), mirroring the real
	// applyAuthDisabledState flow where an operator disables an
	// already-persisted credential rather than creating a brand new one.
	if err := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	auth := &cliproxyauth.Auth{
		ID:       "disabled-status.json",
		Provider: "claude",
		FileName: "disabled-status.json",
		Disabled: true,
		Status:   cliproxyauth.StatusDisabled,
		Metadata: map[string]any{
			"type":     "claude",
			"disabled": true,
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// A brand new store instance re-reading the same directory simulates a
	// process restart: nothing from the in-memory Auth struct survives,
	// only whatever was actually written to disk.
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

	if !got.Disabled {
		t.Fatalf("Disabled = false after restart round-trip, want true")
	}
	if got.Status != cliproxyauth.StatusDisabled {
		t.Fatalf("Status = %q after restart round-trip, want %q (selector/conductor gate on Status, not just the Disabled flag)", got.Status, cliproxyauth.StatusDisabled)
	}
}

// TestFileTokenStore_Save_DisabledAndQuarantinedStatusRoundTrips covers the
// precedence guard documented in readAuthFiles: an account that is both
// operator-disabled and auto-quarantined must still read back with
// Status == StatusDisabled (disabled wins for display), while
// AutoQuarantined/Unavailable remain true so the selector's independent
// OR-check still blocks on the quarantine reason too.
func TestFileTokenStore_Save_DisabledAndQuarantinedStatusRoundTrips(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled-and-quarantined.json")

	// See the comment in TestFileTokenStore_Save_DisabledStatusRoundTrips:
	// Save() no-ops for a Disabled auth whose file does not already exist.
	if err := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	quarantinedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:               "disabled-and-quarantined.json",
		Provider:         "claude",
		FileName:         "disabled-and-quarantined.json",
		Disabled:         true,
		AutoQuarantined:  true,
		QuarantineReason: "terminal_auth_failure",
		QuarantinedAt:    quarantinedAt,
		Status:           cliproxyauth.StatusDisabled,
		Unavailable:      true,
		Metadata: map[string]any{
			"type":              "claude",
			"disabled":          true,
			"auto_quarantined":  true,
			"quarantine_reason": "terminal_auth_failure",
			"quarantined_at":    quarantinedAt.Format(time.RFC3339),
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

	if !got.Disabled {
		t.Fatalf("Disabled = false after restart round-trip, want true")
	}
	if got.Status != cliproxyauth.StatusDisabled {
		t.Fatalf("Status = %q after restart round-trip, want %q (disabled must take display precedence over quarantined)", got.Status, cliproxyauth.StatusDisabled)
	}
	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after restart round-trip, want true (quarantine reason must still be tracked independently of Status)")
	}
	if !got.Unavailable {
		t.Fatalf("Unavailable = false after restart round-trip, want true")
	}
}
