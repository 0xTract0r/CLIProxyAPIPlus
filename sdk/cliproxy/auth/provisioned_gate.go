// Farm supply-atomicity fail-closed gate ("供给原子性 fail-closed 门", P2-A1)
// lives here: a gated, account-level selection block that refuses to serve a
// Claude account which has NOT been bound to a real container device_id (i.e.
// it carries only the per-account synthetic device_id derived by
// helps.SyntheticDeviceID, never a real claude_device_id override). Without
// this gate, an account that finished OAuth but was never provisioned with a
// container binding would still be picked and would then serve traffic under a
// fabricated synthetic device_id ("authenticated but no container, running bare
// with a synthetic device_id").
//
// This file only carries the GATE PREDICATE and its env gating. The fail-closed
// guarantee itself reuses the pre-existing selection block primitive in
// selector.go (isAuthBlockedForModel): the same chokepoint that skips
// disabled/quarantined/reauth-required accounts is extended with one isomorphic
// account-level branch that returns blockReasonUnprovisioned. Every local
// selection path (legacy availableAuthsForRouteModel and the built-in scheduler
// fast path) already funnels through isAuthBlockedForModel, so wiring the gate
// there covers all of them without scattering edits.
//
// Semantics are deliberately DISTINCT from disabled/quarantine: an unprovisioned
// account is NOT dead. It is simply not yet bound to a container, so it gets its
// own block reason (blockReasonUnprovisioned) and self-clears the moment a valid
// claude_device_id override is persisted (an auth Update re-runs the scheduler
// upsert / a fresh candidate scan re-evaluates the gate).
//
// It is a strict no-op unless FARM_REQUIRE_PROVISIONED is set, which keeps every
// existing serving byte identical to today's selection behaviour when the flag
// is off, and it never touches non-Claude providers (which have no
// claude_device_id concept).
package auth

import (
	"os"
	"strings"
)

// FarmRequireProvisionedEnvVar is the environment variable that arms the
// supply-atomicity fail-closed gate. It is intentionally env-driven (like
// FARM_PIN_ENABLED and the management/GITLAB toggles) so enabling the primitive
// requires no config-schema change and stays fully decoupled from non-farm
// request handling.
const FarmRequireProvisionedEnvVar = "FARM_REQUIRE_PROVISIONED"

// blockReasonUnprovisioned is a fork-only block reason, distinct from the
// upstream iota-defined reasons in selector.go (blockReasonNone/Cooldown/
// Disabled/Other). It is given an explicit high value so it never collides with
// the upstream iota block even if upstream appends new reasons there. It marks
// an account skipped by the supply-atomicity fail-closed gate; callers that
// switch on blockReason fall through to their default (non-ready, no
// auto-promote) branch, which is exactly the desired fail-closed handling.
const blockReasonUnprovisioned blockReason = 1 << 16

// farmRequireProvisionedEnabled reports whether the supply-atomicity fail-closed
// gate is armed. Mirrors farmPinEnvEnabled's truthy parsing so the two farm
// toggles behave identically.
func farmRequireProvisionedEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(FarmRequireProvisionedEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// authHasProvisionedDeviceBinding reports whether auth carries a valid,
// operator/container-supplied claude_device_id override, which is the marker of
// a real container provisioning binding. It is the exact same predicate that
// helps.explicitClaudeDeviceID uses to decide "explicit override present ==
// provisioned" (a valid ClaudeDeviceIDAttributeKey in Auth.Attributes), reused
// here rather than reimplemented. When it returns false the account only has the
// synthetic derived device_id, i.e. it is not provisioned.
func authHasProvisionedDeviceBinding(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	value := strings.TrimSpace(auth.Attributes[ClaudeDeviceIDAttributeKey])
	return IsValidClaudeDeviceID(value)
}

// forkRequireProvisionedBlocked reports whether the supply-atomicity fail-closed
// gate should skip auth during selection. It is a strict no-op (returns false)
// unless FARM_REQUIRE_PROVISIONED is armed, so non-farm deployments keep
// byte-identical selection behaviour. The gate is scoped to Claude accounts only
// (other providers have no claude_device_id container binding and must not be
// fail-closed by it). A Claude account is blocked only when it lacks a real
// device_id provisioning binding.
func forkRequireProvisionedBlocked(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if !farmRequireProvisionedEnabled() {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "claude" {
		return false
	}
	return !authHasProvisionedDeviceBinding(auth)
}
