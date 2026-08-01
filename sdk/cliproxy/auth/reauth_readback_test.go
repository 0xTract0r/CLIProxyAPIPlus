package auth

import (
	"testing"
	"time"
)

// reauthRequiredMetadata returns a metadata map mirroring exactly what
// markRefreshReauthRequiredWithReason (types.go) persists when a refresh token
// is rejected as terminally invalid, so the read-back tests exercise the same
// on-disk shape the production write path produces.
func reauthRequiredMetadata(code string) map[string]any {
	return map[string]any{
		"type":                    "claude",
		"refresh_disabled":        true,
		"refresh_status":          "reauth_required",
		"refresh_error_code":      code,
		"refresh_disabled_reason": "reauth_required",
		"reauth_required":         true,
		"refresh_disabled_at":     time.Now().UTC().Format(time.RFC3339),
		"last_refresh_error":      reauthMessageForCode(code),
	}
}

// TestApplyReauthRequiredStateFromMetadata_RestoresTerminalLock is the core
// read-back guard: a record whose persisted metadata carries the terminal
// reauth_required lock must be promoted from StatusActive back to the
// error/unavailable state on load, so a dead refresh token never reloads as a
// fresh, routable credential.
func TestApplyReauthRequiredStateFromMetadata_RestoresTerminalLock(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a", Status: StatusActive, Metadata: reauthRequiredMetadata("invalid_grant")}

	ApplyReauthRequiredStateFromMetadata(auth)

	if auth.Status != StatusError {
		t.Fatalf("Status = %q, want %q", auth.Status, StatusError)
	}
	if auth.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q, want %q", auth.StatusMessage, "reauth_required")
	}
	if !auth.Unavailable {
		t.Fatalf("Unavailable = false, want true")
	}
	if auth.LastError == nil || auth.LastError.Code != "reauth_required" {
		t.Fatalf("LastError = %+v, want code reauth_required", auth.LastError)
	}
}

// TestApplyReauthRequiredStateFromMetadata_NoOpForHealthyOrTransient is the
// terminal-vs-transient guard: a healthy record, or one merely cooling down
// from a transient failure (which never writes the reauth_required lock keys),
// must never be misclassified as reauth-required.
func TestApplyReauthRequiredStateFromMetadata_NoOpForHealthyOrTransient(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{"no keys at all", map[string]any{"type": "claude"}},
		{"explicit reauth false", map[string]any{"type": "claude", "reauth_required": false}},
		{"operator refresh disabled only", map[string]any{"type": "claude", "refresh_disabled": true}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			auth := &Auth{ID: "flaky", Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(30 * time.Minute), Metadata: tc.metadata}
			ApplyReauthRequiredStateFromMetadata(auth)
			if auth.StatusMessage == "reauth_required" {
				t.Fatalf("StatusMessage = %q, want unchanged (not reauth_required)", auth.StatusMessage)
			}
			if auth.LastError != nil && auth.LastError.Code == "reauth_required" {
				t.Fatalf("LastError = %+v, want no reauth_required error injected", auth.LastError)
			}
		})
	}
}

// TestApplyReauthRequiredStateFromMetadata_StrongerLocksWin verifies the
// priority ordering disabled > auto_quarantined > reauth_required: when a
// stronger terminal lock already owns the display Status, the reauth restore
// must not overwrite it (both stronger locks already force the record off the
// rotation, so serving stays blocked either way).
func TestApplyReauthRequiredStateFromMetadata_StrongerLocksWin(t *testing.T) {
	t.Parallel()

	t.Run("disabled wins status", func(t *testing.T) {
		t.Parallel()
		auth := &Auth{ID: "d", Disabled: true, Status: StatusDisabled, Metadata: reauthRequiredMetadata("invalid_grant")}
		ApplyReauthRequiredStateFromMetadata(auth)
		if auth.Status != StatusDisabled {
			t.Fatalf("Status = %q, want %q (disabled takes display precedence)", auth.Status, StatusDisabled)
		}
	})

	t.Run("auto_quarantined wins status", func(t *testing.T) {
		t.Parallel()
		auth := &Auth{ID: "q", AutoQuarantined: true, Unavailable: true, Status: StatusQuarantined, Metadata: reauthRequiredMetadata("invalid_grant")}
		ApplyReauthRequiredStateFromMetadata(auth)
		if auth.Status != StatusQuarantined {
			t.Fatalf("Status = %q, want %q (quarantine takes display precedence)", auth.Status, StatusQuarantined)
		}
	})
}

// TestApplyReauthRequiredStateFromMetadata_SelfClearsWhenLockRemoved documents
// the recovery path: once a completed re-auth removes the lock keys, a fresh
// load leaves the record active with no reverse dead-lock.
func TestApplyReauthRequiredStateFromMetadata_SelfClearsWhenLockRemoved(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "recovered", Status: StatusActive, Metadata: map[string]any{"type": "claude"}}
	ApplyReauthRequiredStateFromMetadata(auth)
	if auth.Status != StatusActive || auth.Unavailable {
		t.Fatalf("Status/Unavailable = %q/%v, want active/false after lock removed", auth.Status, auth.Unavailable)
	}
}
