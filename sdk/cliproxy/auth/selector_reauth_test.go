package auth

import (
	"testing"
	"time"
)

// TestIsAuthBlockedForModel_ReauthRequiredMetadataBlocks is the routing guard
// that closes the false-green gap: a credential carrying the terminal
// reauth_required metadata lock must be skipped entirely (blockReasonDisabled),
// exactly like Disabled/AutoQuarantined -- NOT treated as a transient cooldown.
// Critically it must block even when Unavailable is true but NextRetryAfter is
// zero, since the transient-cooldown branch (Unavailable && NextRetryAfter >
// now) would otherwise let the dead-token account back onto the rotation.
func TestIsAuthBlockedForModel_ReauthRequiredMetadataBlocks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cases := []struct {
		name string
		auth *Auth
	}{
		{
			name: "zero NextRetryAfter, Unavailable false",
			auth: &Auth{ID: "reauth", Status: StatusError, Metadata: map[string]any{"type": "claude", "reauth_required": true}},
		},
		{
			name: "Unavailable true, zero NextRetryAfter",
			auth: &Auth{ID: "reauth", Status: StatusError, Unavailable: true, Metadata: map[string]any{"type": "claude", "refresh_status": "reauth_required"}},
		},
		{
			name: "watcher reset Status back to active but metadata still locked",
			auth: &Auth{ID: "reauth", Status: StatusActive, Metadata: map[string]any{"type": "claude", "reauth_required": true}},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blocked, reason, next := isAuthBlockedForModel(tc.auth, "", now)
			if !blocked {
				t.Fatalf("blocked = false, want true (reauth_required must be skipped)")
			}
			if reason != blockReasonDisabled {
				t.Fatalf("reason = %v, want %v (skipped entirely, not a cooldown)", reason, blockReasonDisabled)
			}
			if !next.IsZero() {
				t.Fatalf("next = %v, want zero (terminal lock has no retry time)", next)
			}
		})
	}
}

// TestIsAuthBlockedForModel_ReauthClearedUnblocks confirms the recovery side:
// once the reauth lock metadata is removed (completed re-auth), the same
// selector gate no longer blocks the credential, so serving resumes without a
// restart.
func TestIsAuthBlockedForModel_ReauthClearedUnblocks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "recovered", Status: StatusActive, Metadata: map[string]any{"type": "claude"}}

	blocked, reason, _ := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true, want false after reauth lock cleared")
	}
	if reason != blockReasonNone {
		t.Fatalf("reason = %v, want %v", reason, blockReasonNone)
	}
}
