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
//
// Even when armed, the gate only applies per-account: only accounts explicitly
// marked farm-enrolled (AuthFarmEnrolled, see farm_enrolled.go) can ever be
// blocked by it. Every account that has never been enrolled into the farm —
// which today means every pre-existing account, including production-stable
// ones — is passed through unconditionally, so arming the global flag can
// never fail-close an account the operator did not opt into the farm.
package auth

import (
	"os"
	"strings"
	"time"
)

// FarmRequireProvisionedEnvVar is the environment variable that arms the
// supply-atomicity fail-closed gate. It is intentionally env-driven (like
// FARM_PIN_ENABLED and the management/GITLAB toggles) so enabling the primitive
// requires no config-schema change and stays fully decoupled from non-farm
// request handling.
const FarmRequireProvisionedEnvVar = "FARM_REQUIRE_PROVISIONED"

// FarmRequireContainerAliveEnvVar arms the container-liveness sub-gate: a
// second, independently-armable fail-closed predicate layered onto the same
// selection chokepoint. When armed, a farm-enrolled Claude account is skipped
// unless its bound container refreshed a liveness heartbeat
// (FarmContainerAliveAtAttributeKey) within FarmContainerAliveStaleThreshold.
// It is env-driven and truthy-parsed exactly like FarmRequireProvisionedEnvVar
// so the two farm sub-gates arm the same way, require no config-schema change,
// and stay decoupled from non-farm request handling. Default off => strict
// no-op (see forkRequireProvisionedBlocked's byte-identical early return).
const FarmRequireContainerAliveEnvVar = "FARM_REQUIRE_CONTAINER_ALIVE"

// FarmContainerAliveStaleThreshold is the freshness window for a container
// liveness heartbeat. authContainerRecentlyAlive treats a heartbeat older than
// this — or missing/unparseable — as not-alive, so the armed liveness sub-gate
// fail-closes the account. It is deliberately MUCH tighter than the farm
// keepalive interval (5 minutes here, not the 120-minute keepalive) so a
// dead/stopped container stops being selected within minutes rather than hours.
const FarmContainerAliveStaleThreshold = 5 * time.Minute

// blockReasonUnprovisioned is a fork-only block reason, distinct from the
// upstream iota-defined reasons in selector.go (blockReasonNone/Cooldown/
// Disabled/Other). It is given an explicit high value so it never collides with
// the upstream iota block even if upstream appends new reasons there. It marks
// an account skipped by the supply-atomicity fail-closed gate; callers that
// switch on blockReason fall through to their default (non-ready, no
// auto-promote) branch, which is exactly the desired fail-closed handling.
const blockReasonUnprovisioned blockReason = 1 << 16

// blockReasonContainerNotAlive is the reserved fork-only block reason for the
// container-liveness sub-gate. It is given the next distinct high bit (1<<17) so
// it never collides with blockReasonUnprovisioned (1<<16) or the upstream
// iota-defined reasons even if upstream appends more. It marks an account
// skipped because its bound container's liveness heartbeat is stale/missing.
//
// NOTE on wiring: forkRequireProvisionedBlocked collapses both farm sub-gates
// (provisioning + liveness) into a single bool, and the selector chokepoint
// (isAuthBlockedForModel) maps every such skip to blockReasonUnprovisioned
// today. This constant reserves the distinct reason value for a later slice that
// threads the specific sub-gate reason through the selector; wiring it into the
// selector is intentionally out of scope for this change (this slice only
// adds/arms the predicate). It is referenced by the collision-guard unit test.
const blockReasonContainerNotAlive blockReason = 1 << 17

// provisionedGateNow returns the current wall-clock time used by the
// container-liveness freshness comparison. It is a package-level indirection
// (rather than a direct time.Now() call inside forkRequireProvisionedBlocked)
// solely so unit tests can inject a deterministic clock when exercising the
// liveness branch; production code leaves it as time.Now.
var provisionedGateNow = time.Now

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

// farmRequireContainerAliveEnabled reports whether the container-liveness
// sub-gate is armed. Mirrors farmRequireProvisionedEnabled's truthy parsing so
// both farm sub-gates behave identically.
func farmRequireContainerAliveEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(FarmRequireContainerAliveEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// authContainerRecentlyAlive reports whether auth carries a container liveness
// heartbeat (auth.Attributes[FarmContainerAliveAtAttributeKey], an RFC3339 UTC
// timestamp the farm orchestrator refreshes while the bound container is alive)
// that is still fresh relative to now. It reads the hydrated Attributes mirror
// (populated by ApplyRuntimeFieldsFromMetadata / the management PATCH sync), NOT
// Metadata, so it stays consistent with the other runtime-hydrated fields the
// gate relies on. An empty, missing, or unparseable value — or a timestamp
// older than FarmContainerAliveStaleThreshold — returns false, which is exactly
// the fail-closed condition the armed liveness sub-gate skips on. A future
// timestamp (benign clock skew) is treated as fresh, since now.Sub(t) is then
// negative and negative <= threshold holds.
func authContainerRecentlyAlive(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	value := strings.TrimSpace(auth.Attributes[FarmContainerAliveAtAttributeKey])
	if value == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return false
	}
	return now.Sub(t) <= FarmContainerAliveStaleThreshold
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

// forkRequireProvisionedBlocked reports whether the farm fail-closed gate should
// skip auth during selection. It now composes TWO independently-armable
// sub-gates that share the exact same scoping:
//
//   - the supply-atomicity sub-gate (FARM_REQUIRE_PROVISIONED): blocks a
//     farm-enrolled Claude account that lacks a real device_id provisioning
//     binding (authHasProvisionedDeviceBinding).
//   - the container-liveness sub-gate (FARM_REQUIRE_CONTAINER_ALIVE): blocks a
//     farm-enrolled Claude account whose bound container's liveness heartbeat is
//     stale/missing (authContainerRecentlyAlive).
//
// It is a strict no-op (returns false, byte-identical selection behaviour)
// unless at least one of the two flags is armed. The shared scoping is
// unchanged: it applies to Claude accounts only (other providers have no
// claude_device_id / container concept and must never be fail-closed by it),
// and only to explicitly farm-enrolled accounts (AuthFarmEnrolled,
// farm_enrolled.go / telemetry-device-farm TR1) — every pre-existing Claude
// account, including every production-stable one, was never marked
// farm_enrolled and stays immune even while a flag is armed. A Claude account is
// blocked when it is farm-enrolled AND fails any armed sub-gate's predicate.
func forkRequireProvisionedBlocked(auth *Auth) bool {
	provisionArmed := farmRequireProvisionedEnabled()
	aliveArmed := farmRequireContainerAliveEnabled()
	if !provisionArmed && !aliveArmed {
		// Neither sub-gate armed: strict no-op, byte-identical to pre-gate
		// selection behaviour. Kept as the very first check so the flag-off path
		// never even inspects auth.
		return false
	}
	if auth == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "claude" {
		return false
	}
	if !AuthFarmEnrolled(auth) {
		return false
	}
	if provisionArmed && !authHasProvisionedDeviceBinding(auth) {
		return true
	}
	if aliveArmed && !authContainerRecentlyAlive(auth, provisionedGateNow()) {
		return true
	}
	return false
}

// RequireProvisionedBlocked is the exported wrapper around
// forkRequireProvisionedBlocked so callers outside this package — the management
// quota poller (quota_snapshots.go), the api-call precheck (api_tools.go) and the
// Claude refresh executor (claude_executor_auth.go) — can reuse the EXACT same
// supply-atomicity fail-closed predicate instead of re-deriving a looser one. It
// inherits every gating property verbatim: strict no-op unless
// FARM_REQUIRE_PROVISIONED is armed, Claude-only, and scoped to explicitly
// farm-enrolled accounts (pre-existing/production accounts stay immune).
func RequireProvisionedBlocked(auth *Auth) bool {
	return forkRequireProvisionedBlocked(auth)
}

// device_id_source enum values for the farm telemetry contract. These string
// constants are the single source of truth shared with the management
// projection (GET /auth-files/account-settings) and, downstream, the frontend
// (field name device_id_source). Do not rename without updating the three-end
// contract.
const (
	// DeviceIDSourceContainerSynced marks a Claude account bound to a real
	// container that persisted a valid claude_device_id override. This is the
	// only "normal"/servable state under the supply-atomicity gate.
	DeviceIDSourceContainerSynced = "container_synced"
	// DeviceIDSourceSynthetic marks a Claude account with no container binding:
	// it runs on the per-account synthetic derived device_id (never provisioned,
	// or the override was explicitly cleared).
	DeviceIDSourceSynthetic = "synthetic"
	// DeviceIDSourceDrift marks a Claude account whose persisted claude_device_id
	// metadata carries a non-empty value that no longer validates as a real
	// device_id (historical binding that drifted/corrupted). The runtime mirror
	// is cleared so serving falls back to the synthetic value, but the residual
	// metadata distinguishes it from a clean never-bound account.
	DeviceIDSourceDrift = "drift"
	// DeviceIDSourceUnknown marks accounts the farm binding concept does not
	// apply to (nil auth or a non-Claude provider). farm_bound is always false
	// here; other providers are never fail-closed by the gate.
	DeviceIDSourceUnknown = "unknown"
)

// claudeDeviceIDMetadataValue reports whether a persisted claude_device_id
// metadata entry exists and returns its trimmed string value. It reads the
// persisted Metadata (not the hydrated Attributes mirror) so it can observe a
// residual value even after applyClaudeDeviceIDFromMetadata clears the invalid
// attribute mirror.
func claudeDeviceIDMetadataValue(auth *Auth) (present bool, value string) {
	if auth == nil || auth.Metadata == nil {
		return false, ""
	}
	raw, ok := auth.Metadata[ClaudeDeviceIDMetadataKey]
	if !ok {
		return false, ""
	}
	str, _ := raw.(string)
	return true, strings.TrimSpace(str)
}

// ClaudeDeviceIDSource classifies a Claude account's device_id provenance for
// the farm telemetry contract and reports whether it is farm-bound. It is the
// canonical derivation reused by the management projection so the two ends never
// diverge, and — crucially — its container_synced/farm_bound decision reuses the
// exact same predicate (authHasProvisionedDeviceBinding) the supply-atomicity
// fail-closed gate uses, so farm_bound == true is precisely the set of accounts
// the gate would allow to serve.
//
// It is scoped to Claude only ("只管 Claude"): a nil auth or any non-Claude
// provider is reported as unknown / not farm-bound, never blocked and never
// mislabeled. The classification never mutates auth.
func ClaudeDeviceIDSource(auth *Auth) (source string, farmBound bool) {
	if auth == nil || strings.ToLower(strings.TrimSpace(auth.Provider)) != "claude" {
		return DeviceIDSourceUnknown, false
	}
	// container_synced is decided by the exact gate predicate (attribute mirror),
	// guaranteeing farm_bound and the gate can never disagree.
	if authHasProvisionedDeviceBinding(auth) {
		return DeviceIDSourceContainerSynced, true
	}
	// A residual, non-empty-but-invalid persisted override marks historical
	// drift: a device_id was recorded once but no longer validates. An explicitly
	// empty value (operator cleared the override) is an intentional synthetic
	// fallback, not drift.
	if present, value := claudeDeviceIDMetadataValue(auth); present && value != "" && !IsValidClaudeDeviceID(value) {
		return DeviceIDSourceDrift, false
	}
	// No binding and no residual override: pure per-account synthetic device_id.
	return DeviceIDSourceSynthetic, false
}
