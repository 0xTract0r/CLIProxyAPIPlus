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
// FARM_REQUIRE_PROVISIONED explicitly disarmed, an unprovisioned Claude account
// (no real device_id override, only the synthetic fallback) must NOT be
// blocked, so existing serving is byte-identical to today's behaviour.
func TestForkRequireProvisioned_FlagOffIsNoop(t *testing.T) {
	// PG-1: FARM_REQUIRE_PROVISIONED now defaults to ARMED, so an empty value no
	// longer means off (os.Getenv cannot distinguish unset from empty). Use an
	// explicit, recognized falsey token to force the flag off.
	t.Setenv(FarmRequireProvisionedEnvVar, "0")

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

// TestForkRequireProvisioned_FlagUnsetDefaultsArmed is the PG-1 fail-safe
// default guard: with FARM_REQUIRE_PROVISIONED forced to "" (== unset, since
// os.Getenv cannot tell the two apart) rather than an explicit falsey token, a
// deployment that forgot to set the env var must still fail closed — an
// enrolled-but-unprovisioned Claude account is blocked. Immune populations
// (unenrolled, non-Claude) still pass through unconditionally even under this
// default-armed state, proving the fail-safe flip did not widen the gate's
// account-level scoping.
func TestForkRequireProvisioned_FlagUnsetDefaultsArmed(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "") // simulate "deploy forgot to set it"

	if !farmRequireProvisionedEnabled() {
		t.Fatalf("farmRequireProvisionedEnabled = false with env unset, want true (PG-1 fail-safe default)")
	}

	enrolledUnprovisioned := claudeAuthEnrolledWithOverride("")
	if !forkRequireProvisionedBlocked(enrolledUnprovisioned) {
		t.Fatalf("forkRequireProvisionedBlocked = false for enrolled+unprovisioned with env unset, want true (fail-safe default must block)")
	}
	blocked, reason, next := isAuthBlockedForModel(enrolledUnprovisioned, "", time.Now())
	if !blocked || reason != blockReasonUnprovisioned || !next.IsZero() {
		t.Fatalf("isAuthBlockedForModel = (%v,%v,%v) with env unset, want (true,%v,zero)", blocked, reason, next, blockReasonUnprovisioned)
	}

	unenrolled := claudeAuthWithOverride("")
	if forkRequireProvisionedBlocked(unenrolled) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an unenrolled account with env unset, want false (immune even under fail-safe default)")
	}

	nonClaude := &Auth{ID: "codex-acct", Provider: "codex", Status: StatusActive, Metadata: map[string]any{FarmEnrolledMetadataKey: true}}
	if forkRequireProvisionedBlocked(nonClaude) {
		t.Fatalf("forkRequireProvisionedBlocked = true for a non-Claude account with env unset, want false (Claude-scoped even under fail-safe default)")
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

// TestFarmRequireProvisionedEnabled_Parsing confirms the PG-1 fail-safe
// denylist parsing: everything stays armed EXCEPT a recognized falsey token
// (case-insensitive, whitespace-trimmed). This is the mirror image of the
// sibling FARM_PIN_ENABLED / FARM_REQUIRE_CONTAINER_ALIVE allowlist toggles —
// unset/empty and any unrecognized value must stay armed here, never fall
// through to disarmed the way they would under an allowlist.
func TestFarmRequireProvisionedEnabled_Parsing(t *testing.T) {
	armed := []string{"", "1", "true", "TRUE", "Yes", "on", " on ", "garbage"}
	for _, v := range armed {
		t.Run("armed/"+v, func(t *testing.T) {
			t.Setenv(FarmRequireProvisionedEnvVar, v)
			if !farmRequireProvisionedEnabled() {
				t.Fatalf("farmRequireProvisionedEnabled = false for %q, want true (fail-safe default)", v)
			}
		})
	}
	disarmed := []string{"0", "false", "FALSE", "off", "OFF", "no", "No", " off "}
	for _, v := range disarmed {
		t.Run("disarmed/"+v, func(t *testing.T) {
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
