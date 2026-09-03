package auth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const testValidDeviceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestMarkCredentialUnauthorizedWritesAuthoritativeReauthLock proves a
// probe-confirmed credential-unauthorized escalates into the SAME authoritative
// reauth-required lock a terminal refresh failure writes: red status, sanitized
// message, refresh disabled, and IsReauthRequiredMetadata true — never a leaked
// raw body.
func TestMarkCredentialUnauthorizedWritesAuthoritativeReauthLock(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "claude-1", Provider: "claude"}

	auth.MarkCredentialUnauthorized(now)

	if auth.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError", auth.Status)
	}
	if auth.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q, want reauth_required", auth.StatusMessage)
	}
	if !auth.RefreshDisabled() {
		t.Fatal("RefreshDisabled() = false, want true (revoked token must not be re-hammered)")
	}
	if !IsReauthRequiredMetadata(auth.Metadata) {
		t.Fatal("IsReauthRequiredMetadata = false, want true")
	}
	if got, _ := auth.Metadata["refresh_error_code"].(string); got != CredentialUnauthorizedReauthCode {
		t.Fatalf("refresh_error_code = %q, want %q", got, CredentialUnauthorizedReauthCode)
	}
	if auth.LastError == nil || auth.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want a 401 error", auth.LastError)
	}
	if msg, _ := auth.Metadata["last_refresh_error"].(string); strings.Contains(strings.ToLower(msg), "token=") || strings.Contains(msg, "Bearer") {
		t.Fatalf("last_refresh_error appears to leak token material: %q", msg)
	}
}

func TestAuthEverBoundToContainer(t *testing.T) {
	cases := []struct {
		name string
		auth *Auth
		want bool
	}{
		{
			name: "container_synced valid binding is ever-bound",
			auth: &Auth{Provider: "claude", Attributes: map[string]string{ClaudeDeviceIDAttributeKey: testValidDeviceID}},
			want: true,
		},
		{
			name: "drift residual invalid binding is ever-bound",
			auth: &Auth{Provider: "claude", Metadata: map[string]any{ClaudeDeviceIDMetadataKey: "not-a-valid-device-id"}},
			want: true,
		},
		{
			name: "synthetic (never bound) is NOT ever-bound (leak boundary)",
			auth: &Auth{Provider: "claude"},
			want: false,
		},
		{
			name: "non-claude is NOT ever-bound",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{ClaudeDeviceIDAttributeKey: testValidDeviceID}},
			want: false,
		},
		{
			name: "nil auth is NOT ever-bound",
			auth: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthEverBoundToContainer(tc.auth); got != tc.want {
				t.Fatalf("AuthEverBoundToContainer = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFarmHealthBlindDistinguishesBoundVsUnbound is the B1 gate-decouple
// invariant: an EVER-BOUND farm account blocked by the armed container-alive
// gate is health-blind (probeable / must-alert), a NEVER-BOUND account is NOT
// (probing it would leak the synthetic device_id — the gate is not relaxed).
func TestFarmHealthBlindDistinguishesBoundVsUnbound(t *testing.T) {
	// Arm the container-liveness sub-gate; leave provisioning gate at its armed
	// default. A stale (missing) farm_container_alive_at makes an alive-armed,
	// bound account fail-closed.
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")

	everBoundBlocked := &Auth{
		Provider:   "claude",
		Metadata:   map[string]any{FarmEnrolledMetadataKey: true, ClaudeDeviceIDMetadataKey: testValidDeviceID},
		Attributes: map[string]string{ClaudeDeviceIDAttributeKey: testValidDeviceID}, // bound, but no fresh heartbeat
	}
	if !FarmHealthBlind(everBoundBlocked) {
		t.Fatal("ever-bound farm account with stale container heartbeat should be health-blind")
	}

	neverBound := &Auth{
		Provider: "claude",
		Metadata: map[string]any{FarmEnrolledMetadataKey: true}, // enrolled, never bound (synthetic)
	}
	if FarmHealthBlind(neverBound) {
		t.Fatal("never-bound farm account must NOT be health-blind (probing it would leak the synthetic device_id)")
	}

	notEnrolled := &Auth{
		Provider:   "claude",
		Metadata:   map[string]any{ClaudeDeviceIDMetadataKey: testValidDeviceID},
		Attributes: map[string]string{ClaudeDeviceIDAttributeKey: testValidDeviceID},
	}
	if FarmHealthBlind(notEnrolled) {
		t.Fatal("non-enrolled account must never be health-blind")
	}
}

// TestFarmHealthBlindFalseWhenGateDisarmed proves the signal is scoped to a real
// block: with both farm sub-gates disarmed nothing is skipped, so nothing is blind.
func TestFarmHealthBlindFalseWhenGateDisarmed(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "0")
	t.Setenv(FarmRequireContainerAliveEnvVar, "0")

	auth := &Auth{
		Provider:   "claude",
		Metadata:   map[string]any{FarmEnrolledMetadataKey: true, ClaudeDeviceIDMetadataKey: testValidDeviceID},
		Attributes: map[string]string{ClaudeDeviceIDAttributeKey: testValidDeviceID},
	}
	if FarmHealthBlind(auth) {
		t.Fatal("with both farm sub-gates disarmed no account is skipped, so none is health-blind")
	}
}

// TestClearStaleReauthLockRecoversCredentialUnauthorized proves the recovery
// contract: the automatic lock MarkCredentialUnauthorized writes is releasable
// (the successful-probe / manual-reauth path), and IsReauthRequiredMetadata is
// the discriminator used to distinguish it from an operator's explicit
// refresh-disable.
func TestClearStaleReauthLockRecoversCredentialUnauthorized(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{ID: "claude-1", Provider: "claude"}
	auth.MarkCredentialUnauthorized(now)
	if !IsReauthRequiredMetadata(auth.Metadata) {
		t.Fatal("precondition: lock must be set")
	}
	// The management-layer clear (clearStaleReauthLockOnSave) removes exactly these
	// keys on a successful probe/reauth; mirror the metadata-key contract here.
	for _, key := range []string{"reauth_required", "refresh_status", "refresh_error_code", "refresh_disabled_reason", "refresh_disabled"} {
		delete(auth.Metadata, key)
	}
	if IsReauthRequiredMetadata(auth.Metadata) {
		t.Fatal("after clearing the lock keys IsReauthRequiredMetadata must be false")
	}
	if auth.RefreshDisabled() {
		t.Fatal("after clearing the lock keys RefreshDisabled must be false (account can refresh again)")
	}
}
