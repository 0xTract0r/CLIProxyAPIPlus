package auth

import (
	"context"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestFileTokenStore_Save_AutoQuarantinedRoundTrips is a real (no
// WithSkipPersist involved -- store.Save/List are called directly) disk
// round-trip covering the fork's auto-quarantine restart-persistence gap:
// a terminally quarantined credential must still read back as quarantined
// after a fresh FileTokenStore instance re-lists the same directory,
// simulating a CPA process restart. The Metadata fields set here mirror
// exactly what markAutoQuarantine (conductor_auto_quarantine.go) writes.
func TestFileTokenStore_Save_AutoQuarantinedRoundTrips(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	quarantinedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:               "quarantined.json",
		Provider:         "claude",
		FileName:         "quarantined.json",
		AutoQuarantined:  true,
		QuarantineReason: "terminal_auth_failure",
		QuarantinedAt:    quarantinedAt,
		Status:           cliproxyauth.StatusQuarantined,
		Unavailable:      true,
		Metadata: map[string]any{
			"type":              "claude",
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

	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after restart round-trip, want true")
	}
	if got.Status != cliproxyauth.StatusQuarantined {
		t.Fatalf("Status = %q after restart round-trip, want %q", got.Status, cliproxyauth.StatusQuarantined)
	}
	if !got.Unavailable {
		t.Fatalf("Unavailable = false after restart round-trip, want true")
	}
	if got.QuarantineReason != "terminal_auth_failure" {
		t.Fatalf("QuarantineReason = %q after restart round-trip, want %q", got.QuarantineReason, "terminal_auth_failure")
	}
	if !got.QuarantinedAt.Equal(quarantinedAt) {
		t.Fatalf("QuarantinedAt = %v after restart round-trip, want %v", got.QuarantinedAt, quarantinedAt)
	}
}

// TestFileTokenStore_Save_ClearAutoQuarantineRemovesMetadataRoundTrip covers
// the release side: after clearAutoQuarantine's struct-field reset and
// Metadata key deletion (mirrored here the same way
// TestFileTokenStore_Save_AutoQuarantinedRoundTrips mirrors
// markAutoQuarantine's write), a second Save()+fresh-List() round trip must
// show the credential as no longer quarantined and the persisted Metadata
// keys actually gone (not merely false), so a restart cannot resurrect a
// lock that was legitimately lifted by a completed reauth or an operator
// re-enable.
func TestFileTokenStore_Save_ClearAutoQuarantineRemovesMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	quarantinedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:               "recovered.json",
		Provider:         "claude",
		FileName:         "recovered.json",
		AutoQuarantined:  true,
		QuarantineReason: "terminal_auth_failure",
		QuarantinedAt:    quarantinedAt,
		Status:           cliproxyauth.StatusQuarantined,
		Unavailable:      true,
		Metadata: map[string]any{
			"type":              "claude",
			"auto_quarantined":  true,
			"quarantine_reason": "terminal_auth_failure",
			"quarantined_at":    quarantinedAt.Format(time.RFC3339),
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() (quarantined) error: %v", err)
	}

	// Simulate clearAutoQuarantine's effect on both the struct fields and
	// the mirrored Metadata keys (see clearAutoQuarantine /
	// clearAutoQuarantineMetadata in conductor_auto_quarantine.go), then
	// Save again -- this is what saveTokenRecord / applyAuthDisabledState do
	// via Auth.ClearAutoQuarantine() followed by Manager.Update/Save.
	cleared := auth.Clone()
	cleared.AutoQuarantined = false
	cleared.QuarantineReason = ""
	cleared.QuarantinedAt = time.Time{}
	cleared.Status = cliproxyauth.StatusActive
	cleared.Unavailable = false
	delete(cleared.Metadata, "auto_quarantined")
	delete(cleared.Metadata, "quarantine_reason")
	delete(cleared.Metadata, "quarantined_at")

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

	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after clear+restart round-trip, want false")
	}
	if got.Status == cliproxyauth.StatusQuarantined {
		t.Fatalf("Status = %q after clear+restart round-trip, want anything but quarantined", got.Status)
	}
	if _, ok := got.Metadata["auto_quarantined"]; ok {
		t.Fatalf("Metadata[auto_quarantined] = %#v after clear round-trip, want key absent", got.Metadata["auto_quarantined"])
	}
	if _, ok := got.Metadata["quarantine_reason"]; ok {
		t.Fatalf("Metadata[quarantine_reason] = %#v after clear round-trip, want key absent", got.Metadata["quarantine_reason"])
	}
	if _, ok := got.Metadata["quarantined_at"]; ok {
		t.Fatalf("Metadata[quarantined_at] = %#v after clear round-trip, want key absent", got.Metadata["quarantined_at"])
	}
}

// TestFileTokenStore_Save_TransientStateDoesNotPersistAsQuarantine is the
// terminal-vs-transient guard: a credential merely cooling down from a
// transient failure (429/5xx) -- which never sets the "auto_quarantined"
// Metadata key, since evaluateAutoQuarantineLocked's in-memory streak
// bookkeeping is intentionally never persisted -- must never be misread as
// auto-quarantined after a restart round trip, even though it shares the
// same Unavailable/NextRetryAfter-shaped runtime state while live.
func TestFileTokenStore_Save_TransientStateDoesNotPersistAsQuarantine(t *testing.T) {
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
			// Deliberately no "auto_quarantined"/"quarantine_reason"/
			// "quarantined_at" keys: only a terminal quarantine ever writes
			// them (see setAutoQuarantineMetadata).
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

	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after restart round-trip for a transient-cooldown-only auth, want false")
	}
	if got.Status == cliproxyauth.StatusQuarantined {
		t.Fatalf("Status = %q after restart round-trip, want anything but quarantined", got.Status)
	}
	if got.Unavailable {
		t.Fatalf("Unavailable = true after restart round-trip for a transient-cooldown-only auth, want false (only the terminal quarantine lock is restored)")
	}
}
