package auth

import (
	"strings"
	"testing"
	"time"
)

// validProvisionedDeviceID is a well-formed 64-hex claude_device_id override,
// standing in for a real container provisioning binding.
var validProvisionedDeviceID = strings.Repeat("a", 64)

func claudeAuthWithOverride(deviceID string) *Auth {
	auth := &Auth{ID: "claude-acct", Provider: "claude", Status: StatusActive}
	if deviceID != "" {
		auth.Attributes = map[string]string{ClaudeDeviceIDAttributeKey: deviceID}
	}
	return auth
}

// claudeAuthEnrolledWithOverride behaves like claudeAuthWithOverride but
// additionally marks the account farm-enrolled (AuthFarmEnrolled,
// farm_enrolled.go). Enrollment is the account-level precondition the gate
// now requires in addition to the binding check: an account must be BOTH
// enrolled AND unbound to be fail-closed.
func claudeAuthEnrolledWithOverride(deviceID string) *Auth {
	auth := claudeAuthWithOverride(deviceID)
	auth.Metadata = map[string]any{FarmEnrolledMetadataKey: true}
	return auth
}

// TestForkRequireProvisioned_FlagOffIsNoop is the critical no-op guard: with
// FARM_REQUIRE_PROVISIONED unset, an unprovisioned Claude account (no real
// device_id override, only the synthetic fallback) must NOT be blocked, so
// existing serving is byte-identical to today's behaviour.
func TestForkRequireProvisioned_FlagOffIsNoop(t *testing.T) {
	// Force the flag off explicitly so a polluted process env cannot mask this.
	t.Setenv(FarmRequireProvisionedEnvVar, "")

	auth := claudeAuthWithOverride("")
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true with flag off, want false (strict no-op)")
	}
	blocked, reason, next := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true with flag off, want false (no-op)")
	}
	if reason != blockReasonNone {
		t.Fatalf("reason = %v, want %v", reason, blockReasonNone)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero", next)
	}
}

// TestForkRequireProvisioned_FlagOnBlocksUnprovisionedEnrolledClaude confirms
// the fail-closed behaviour: with the flag armed AND the account explicitly
// farm-enrolled, a Claude account that carries only a synthetic device_id (no
// valid override binding) is skipped entirely with the distinct
// blockReasonUnprovisioned and no retry time.
func TestForkRequireProvisioned_FlagOnBlocksUnprovisionedEnrolledClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthEnrolledWithOverride("")
	now := time.Now()

	if !forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = false, want true (enrolled+unprovisioned account must fail closed)")
	}
	for _, model := range []string{"", "claude-sonnet-4-5"} {
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked {
			t.Fatalf("model=%q: blocked = false, want true (enrolled+unprovisioned must be skipped)", model)
		}
		if reason != blockReasonUnprovisioned {
			t.Fatalf("model=%q: reason = %v, want %v (distinct from disabled/cooldown)", model, reason, blockReasonUnprovisioned)
		}
		if !next.IsZero() {
			t.Fatalf("model=%q: next = %v, want zero (no time-based recovery; provisioning is an external event)", model, next)
		}
	}
}

// TestForkRequireProvisioned_FlagOnAllowsUnenrolledUnprovisionedClaude is the
// single most important regression guard for the account-scoped gate: with
// the global flag armed, a Claude account that was NEVER farm-enrolled — which
// today means every pre-existing account, including every production-stable
// account — must be passed through unconditionally even though it also
// carries no real device_id override. Arming the global flag must be a
// complete no-op for the entire non-enrolled account population.
func TestForkRequireProvisioned_FlagOnAllowsUnenrolledUnprovisionedClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthWithOverride("") // unenrolled: no Metadata[farm_enrolled] at all
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an unenrolled account, want false (gate must be a no-op for non-enrolled accounts)")
	}
	blocked, reason, next := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true for an unenrolled account, want false (old/production-stable accounts must stay immune)")
	}
	if reason != blockReasonNone {
		t.Fatalf("reason = %v, want %v", reason, blockReasonNone)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero", next)
	}
}

// TestForkRequireProvisioned_FlagOnAllowsExplicitlyUnenrolledClaude covers the
// explicit-false variant of unenrolled (Metadata present but farm_enrolled ==
// false), which must behave identically to the metadata-absent case above.
func TestForkRequireProvisioned_FlagOnAllowsExplicitlyUnenrolledClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthWithOverride("")
	auth.Metadata = map[string]any{FarmEnrolledMetadataKey: false}
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an explicitly-unenrolled account, want false")
	}
	blocked, _, _ := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true for an explicitly-unenrolled account, want false")
	}
}

// TestForkRequireProvisioned_FlagOnAllowsProvisionedClaude confirms the recovery
// side: an enrolled Claude account WITH a valid claude_device_id override
// binding passes the gate and is servable even when the flag is armed.
func TestForkRequireProvisioned_FlagOnAllowsProvisionedClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthEnrolledWithOverride(validProvisionedDeviceID)
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an enrolled+provisioned account, want false")
	}
	blocked, reason, _ := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true for an enrolled+provisioned account, want false")
	}
	if reason != blockReasonNone {
		t.Fatalf("reason = %v, want %v", reason, blockReasonNone)
	}
}

// TestForkRequireProvisioned_FlagOnIgnoresNonClaude confirms the gate is scoped
// to Claude accounts only: a Codex account has no claude_device_id container
// binding concept and must never be fail-closed by this gate, even with the flag
// armed.
func TestForkRequireProvisioned_FlagOnIgnoresNonClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := &Auth{ID: "codex-acct", Provider: "codex", Status: StatusActive}
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for a non-Claude account, want false (gate is Claude-scoped)")
	}
	blocked, _, _ := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true for a non-Claude account, want false")
	}
}

// TestForkRequireProvisioned_DisabledTakesPrecedence confirms the gate is
// isomorphic to and ordered after the disabled/quarantine/reauth locks: an
// operator-disabled account that also happens to be enrolled+unprovisioned
// (i.e. the gate WOULD otherwise block it) still reports its terminal
// blockReasonDisabled, so the new reason never clobbers the existing terminal
// locks (unprovisioned != dead).
func TestForkRequireProvisioned_DisabledTakesPrecedence(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthEnrolledWithOverride("")
	auth.Disabled = true
	now := time.Now()

	blocked, reason, _ := isAuthBlockedForModel(auth, "", now)
	if !blocked {
		t.Fatalf("blocked = false, want true")
	}
	if reason != blockReasonDisabled {
		t.Fatalf("reason = %v, want %v (disabled lock must take precedence over unprovisioned)", reason, blockReasonDisabled)
	}
}

// TestFarmRequireProvisionedEnabled_Parsing confirms the env truthy parsing
// matches the sibling FARM_PIN_ENABLED toggle so the two farm switches behave
// identically.
func TestFarmRequireProvisionedEnabled_Parsing(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "Yes", "on", " on "}
	for _, v := range truthy {
		t.Run("on/"+v, func(t *testing.T) {
			t.Setenv(FarmRequireProvisionedEnvVar, v)
			if !farmRequireProvisionedEnabled() {
				t.Fatalf("farmRequireProvisionedEnabled = false for %q, want true", v)
			}
		})
	}
	falsy := []string{"", "0", "false", "off", "no", "garbage"}
	for _, v := range falsy {
		t.Run("off/"+v, func(t *testing.T) {
			t.Setenv(FarmRequireProvisionedEnvVar, v)
			if farmRequireProvisionedEnabled() {
				t.Fatalf("farmRequireProvisionedEnabled = true for %q, want false", v)
			}
		})
	}
}

// claudeAuthWithMetadataOverride builds a Claude auth whose PERSISTED metadata
// carries a claude_device_id value, then hydrates the attribute mirror exactly
// like the live pipeline (ApplyRuntimeFieldsFromMetadata) so classification sees
// the same state the gate does.
func claudeAuthWithMetadataOverride(deviceID string) *Auth {
	auth := &Auth{ID: "claude-acct", Provider: "claude", Status: StatusActive}
	auth.Metadata = map[string]any{ClaudeDeviceIDMetadataKey: deviceID}
	ApplyRuntimeFieldsFromMetadata(auth)
	return auth
}

// claudeAuthWithMetadataOverrideEnrolled behaves like
// claudeAuthWithMetadataOverride but additionally marks the account
// farm-enrolled, matching AuthFarmEnrolled's persisted-metadata contract.
func claudeAuthWithMetadataOverrideEnrolled(deviceID string) *Auth {
	auth := claudeAuthWithMetadataOverride(deviceID)
	auth.Metadata[FarmEnrolledMetadataKey] = true
	return auth
}

// TestClaudeDeviceIDSource_ContainerSynced: a valid persisted override marks a
// real container binding -> container_synced + farm_bound true.
func TestClaudeDeviceIDSource_ContainerSynced(t *testing.T) {
	auth := claudeAuthWithMetadataOverride(validProvisionedDeviceID)
	source, bound := ClaudeDeviceIDSource(auth)
	if source != DeviceIDSourceContainerSynced {
		t.Fatalf("source = %q, want %q", source, DeviceIDSourceContainerSynced)
	}
	if !bound {
		t.Fatalf("farmBound = false, want true for a container-synced account")
	}
}

// TestClaudeDeviceIDSource_SyntheticNoOverride: no override at all -> synthetic,
// not farm-bound.
func TestClaudeDeviceIDSource_SyntheticNoOverride(t *testing.T) {
	auth := &Auth{ID: "claude-acct", Provider: "claude", Status: StatusActive}
	source, bound := ClaudeDeviceIDSource(auth)
	if source != DeviceIDSourceSynthetic {
		t.Fatalf("source = %q, want %q", source, DeviceIDSourceSynthetic)
	}
	if bound {
		t.Fatalf("farmBound = true, want false for an unbound synthetic account")
	}
}

// TestClaudeDeviceIDSource_SyntheticClearedOverride: an explicitly emptied
// override is an intentional synthetic fallback, NOT drift.
func TestClaudeDeviceIDSource_SyntheticClearedOverride(t *testing.T) {
	auth := claudeAuthWithMetadataOverride("")
	source, bound := ClaudeDeviceIDSource(auth)
	if source != DeviceIDSourceSynthetic {
		t.Fatalf("source = %q, want %q (empty override is intentional synthetic, not drift)", source, DeviceIDSourceSynthetic)
	}
	if bound {
		t.Fatalf("farmBound = true, want false")
	}
}

// TestClaudeDeviceIDSource_Drift: a residual, non-empty-but-invalid persisted
// override marks historical drift -> drift, not farm-bound.
func TestClaudeDeviceIDSource_Drift(t *testing.T) {
	auth := claudeAuthWithMetadataOverride("not-a-valid-64-hex-device-id")
	// Sanity: hydration must have refused to mirror the invalid value.
	if _, ok := auth.Attributes[ClaudeDeviceIDAttributeKey]; ok {
		t.Fatalf("invalid override was mirrored into attributes; drift precondition broken")
	}
	source, bound := ClaudeDeviceIDSource(auth)
	if source != DeviceIDSourceDrift {
		t.Fatalf("source = %q, want %q", source, DeviceIDSourceDrift)
	}
	if bound {
		t.Fatalf("farmBound = true, want false for a drifted account")
	}
}

// TestClaudeDeviceIDSource_UnknownNonClaudeAndNil: the classifier is Claude-only.
func TestClaudeDeviceIDSource_UnknownNonClaudeAndNil(t *testing.T) {
	codex := claudeAuthWithMetadataOverride(validProvisionedDeviceID)
	codex.Provider = "codex"
	if source, bound := ClaudeDeviceIDSource(codex); source != DeviceIDSourceUnknown || bound {
		t.Fatalf("non-Claude: got (%q, %v), want (%q, false)", source, bound, DeviceIDSourceUnknown)
	}
	if source, bound := ClaudeDeviceIDSource(nil); source != DeviceIDSourceUnknown || bound {
		t.Fatalf("nil: got (%q, %v), want (%q, false)", source, bound, DeviceIDSourceUnknown)
	}
}

// TestClaudeDeviceIDSource_FarmBoundMatchesGate is the cross-consistency guard
// between the (enrollment-independent) farm_bound classification and the
// (now account-scoped) gate:
//
//  1. Universal, regardless of enrollment: the gate must never fail-close an
//     account that farm_bound reports as bound — a container-synced account
//     is always servable.
//  2. For ENROLLED accounts specifically, the invariant is exact: farm_bound
//     == true must be precisely the set the armed gate allows (NOT blocked).
//     This is the property the three-end management contract relies on for
//     opted-in accounts, and the reason the classifier reuses
//     authHasProvisionedDeviceBinding.
//  3. For UNENROLLED accounts, the gate is always a no-op (never blocks)
//     regardless of what farm_bound reports — this is the account-scoping
//     guarantee this gate was changed to provide.
func TestClaudeDeviceIDSource_FarmBoundMatchesGate(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")
	cases := []struct {
		enrolled bool
		auth     *Auth
	}{
		{enrolled: true, auth: claudeAuthWithMetadataOverrideEnrolled(validProvisionedDeviceID)},                                                  // bound
		{enrolled: true, auth: &Auth{ID: "a", Provider: "claude", Status: StatusActive, Metadata: map[string]any{FarmEnrolledMetadataKey: true}}}, // synthetic
		{enrolled: true, auth: claudeAuthWithMetadataOverrideEnrolled("")},                                                                        // cleared -> synthetic
		{enrolled: true, auth: claudeAuthWithMetadataOverrideEnrolled("not-valid")},                                                               // drift
		{enrolled: false, auth: claudeAuthWithMetadataOverride(validProvisionedDeviceID)},                                                         // bound, but never enrolled
		{enrolled: false, auth: &Auth{ID: "b", Provider: "claude", Status: StatusActive}},                                                         // synthetic, never enrolled (the old-account case)
		{enrolled: false, auth: claudeAuthWithMetadataOverride("")},                                                                               // cleared -> synthetic, never enrolled
		{enrolled: false, auth: claudeAuthWithMetadataOverride("not-valid")},                                                                      // drift, never enrolled
	}
	for i, tc := range cases {
		_, bound := ClaudeDeviceIDSource(tc.auth)
		blocked := forkRequireProvisionedBlocked(tc.auth)
		if bound && blocked {
			t.Fatalf("case %d: farm_bound account was blocked by the gate; a bound account must never be fail-closed", i)
		}
		if !tc.enrolled && blocked {
			t.Fatalf("case %d: unenrolled account was blocked by the gate; the gate must be a no-op for non-enrolled accounts", i)
		}
		if tc.enrolled && bound == blocked {
			t.Fatalf("case %d (enrolled): farmBound=%v but gateBlocked=%v; for enrolled accounts farm_bound must equal NOT-blocked", i, bound, blocked)
		}
	}
}
