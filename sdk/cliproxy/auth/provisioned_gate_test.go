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
