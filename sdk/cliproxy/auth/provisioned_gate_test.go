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

// TestForkRequireProvisioned_FlagOnBlocksUnprovisionedClaude confirms the
// fail-closed behaviour: with the flag armed, a Claude account that carries only
// a synthetic device_id (no valid override binding) is skipped entirely with the
// distinct blockReasonUnprovisioned and no retry time.
func TestForkRequireProvisioned_FlagOnBlocksUnprovisionedClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthWithOverride("")
	now := time.Now()

	if !forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = false, want true (unprovisioned account must fail closed)")
	}
	for _, model := range []string{"", "claude-sonnet-4-5"} {
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked {
			t.Fatalf("model=%q: blocked = false, want true (unprovisioned must be skipped)", model)
		}
		if reason != blockReasonUnprovisioned {
			t.Fatalf("model=%q: reason = %v, want %v (distinct from disabled/cooldown)", model, reason, blockReasonUnprovisioned)
		}
		if !next.IsZero() {
			t.Fatalf("model=%q: next = %v, want zero (no time-based recovery; provisioning is an external event)", model, next)
		}
	}
}

// TestForkRequireProvisioned_FlagOnAllowsProvisionedClaude confirms the recovery
// side: a Claude account WITH a valid claude_device_id override binding passes
// the gate and is servable even when the flag is armed.
func TestForkRequireProvisioned_FlagOnAllowsProvisionedClaude(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthWithOverride(validProvisionedDeviceID)
	now := time.Now()

	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for a provisioned account, want false")
	}
	blocked, reason, _ := isAuthBlockedForModel(auth, "", now)
	if blocked {
		t.Fatalf("blocked = true for a provisioned account, want false")
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
// operator-disabled account that also happens to be unprovisioned still reports
// its terminal blockReasonDisabled, so the new reason never clobbers the
// existing terminal locks (unprovisioned != dead).
func TestForkRequireProvisioned_DisabledTakesPrecedence(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")

	auth := claudeAuthWithOverride("")
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

// TestClaudeDeviceIDSource_FarmBoundMatchesGate is the cross-consistency guard:
// farm_bound==true must be exactly the set of Claude accounts the armed gate
// would allow (i.e. NOT blocked). This is the property the three-end contract
// relies on and the reason the classifier reuses authHasProvisionedDeviceBinding.
func TestClaudeDeviceIDSource_FarmBoundMatchesGate(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")
	cases := []*Auth{
		claudeAuthWithMetadataOverride(validProvisionedDeviceID), // bound
		&Auth{ID: "a", Provider: "claude", Status: StatusActive}, // synthetic
		claudeAuthWithMetadataOverride(""),                       // cleared -> synthetic
		claudeAuthWithMetadataOverride("not-valid"),              // drift
	}
	for i, auth := range cases {
		_, bound := ClaudeDeviceIDSource(auth)
		blocked := forkRequireProvisionedBlocked(auth)
		if bound == blocked {
			t.Fatalf("case %d: farmBound=%v but gateBlocked=%v; farm_bound must equal NOT-blocked", i, bound, blocked)
		}
	}
}
