package store

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestApplyQuarantineStateFromMetadata_RestoresTerminalLock covers the
// postgres-backed store's side of the fork's auto-quarantine
// restart-persistence gap (see markAutoQuarantine in
// sdk/cliproxy/auth/conductor_auto_quarantine.go): a row whose persisted
// metadata carries the "auto_quarantined" lock must restore
// AutoQuarantined/Status/Unavailable/QuarantineReason/QuarantinedAt on the
// runtime Auth struct, exactly like PostgresStore.List does for every
// database row. This is a pure-function test (no live Postgres connection
// required) so the restore logic is covered even without TEST_DATABASE_URL.
func TestApplyQuarantineStateFromMetadata_RestoresTerminalLock(t *testing.T) {
	quarantinedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{ID: "a", Status: cliproxyauth.StatusActive}
	metadata := map[string]any{
		"type":              "claude",
		"auto_quarantined":  true,
		"quarantine_reason": "terminal_auth_failure",
		"quarantined_at":    quarantinedAt.Format(time.RFC3339),
	}

	applyQuarantineStateFromMetadata(auth, metadata)

	if !auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false, want true")
	}
	if auth.Status != cliproxyauth.StatusQuarantined {
		t.Fatalf("Status = %q, want %q", auth.Status, cliproxyauth.StatusQuarantined)
	}
	if !auth.Unavailable {
		t.Fatalf("Unavailable = false, want true")
	}
	if auth.QuarantineReason != "terminal_auth_failure" {
		t.Fatalf("QuarantineReason = %q, want %q", auth.QuarantineReason, "terminal_auth_failure")
	}
	if !auth.QuarantinedAt.Equal(quarantinedAt) {
		t.Fatalf("QuarantinedAt = %v, want %v", auth.QuarantinedAt, quarantinedAt)
	}
}

// TestApplyQuarantineStateFromMetadata_NoOpForTransientOrHealthyRecords is
// the terminal-vs-transient guard for the postgres-backed store: metadata
// without the "auto_quarantined" key (a healthy record, or one merely
// cooling down from a transient 429/5xx whose streak bookkeeping is
// intentionally never persisted -- see Auth.terminalAuthFailureStreak) must
// never be misclassified as auto-quarantined.
func TestApplyQuarantineStateFromMetadata_NoOpForTransientOrHealthyRecords(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{"no key at all", map[string]any{"type": "claude"}},
		{"explicit false", map[string]any{"type": "claude", "auto_quarantined": false}},
		{"wrong type", map[string]any{"type": "claude", "auto_quarantined": "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{ID: "flaky", Status: cliproxyauth.StatusError, Unavailable: true}
			applyQuarantineStateFromMetadata(auth, tc.metadata)
			if auth.AutoQuarantined {
				t.Fatalf("AutoQuarantined = true, want false")
			}
			if auth.Status != cliproxyauth.StatusError {
				t.Fatalf("Status = %q, want unchanged %q", auth.Status, cliproxyauth.StatusError)
			}
		})
	}
}

// TestApplyQuarantineStateFromMetadata_DisabledWinsStatusButAutoQuarantinedStillSet
// covers the case where an operator disables an already-quarantined
// credential (see applyAuthDisabledState in the management API, which does
// not clear an existing quarantine on disable): Status must stay
// StatusDisabled for display, but AutoQuarantined itself must still be
// restored to true so isAuthBlockedForModel's OR-check blocks on either
// reason independently once the operator later re-enables it without also
// completing a fresh reauth.
func TestApplyQuarantineStateFromMetadata_DisabledWinsStatusButAutoQuarantinedStillSet(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "disabled-and-quarantined", Disabled: true, Status: cliproxyauth.StatusDisabled}
	metadata := map[string]any{
		"type":             "claude",
		"disabled":         true,
		"auto_quarantined": true,
	}

	applyQuarantineStateFromMetadata(auth, metadata)

	if !auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false, want true")
	}
	if auth.Status != cliproxyauth.StatusDisabled {
		t.Fatalf("Status = %q, want %q (disabled takes display precedence)", auth.Status, cliproxyauth.StatusDisabled)
	}
}
