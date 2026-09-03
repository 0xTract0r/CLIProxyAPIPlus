// Farm account liveness detection (openspec change farm-account-liveness-detection).
//
// This file carries the STAGED-ROLLOUT env gating and the shared eligibility /
// classification helpers for two cooperating mechanisms that close the incident
// gap "an idle farm account revoked upstream keeps showing green for 10+ hours,
// with zero automatic detection":
//
//   - Phase 1 (FARM_LIVENESS_DETECTION_ENABLED): the background quota/profile
//     refresh escalates a confirmed `credential unauthorized` from the
//     non-authoritative quota_refresh_status sub-field into the authoritative
//     reauth-required lock (Auth.MarkCredentialUnauthorized), makes that lock
//     sticky against transient network errors, and stamps a health-blind marker
//     when the anti-corr gate skips an ever-bound account.
//   - Phase 2 (FARM_LIVENESS_PROBE_ENABLED): a serving-independent low-frequency
//     probe (farm_liveness_probe.go) actively re-checks ever-bound accounts the
//     quota poller skips (container-dead / refresh-frozen), so a revoked idle
//     account is caught even with no serving traffic.
//
// BOTH flags default OFF (allowlist truthy parse, mirroring
// FARM_REQUIRE_CONTAINER_ALIVE's staged-rollout default-off shape, NOT the
// PG-1 default-armed denylist). Test-side arms them first; production stays off
// until validated. Neither flag ever relaxes the anti-corr leak-prevention
// boundary: probing is scoped to accounts whose device_id is already on-wire
// exposed (AuthEverBoundToContainer), never to never-bound synthetic accounts.
package management

import (
	"os"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	// FarmLivenessDetectionEnvVar arms Phase 1: authoritative escalation of a
	// probe/quota-confirmed credential-unauthorized signal + anti-overwrite
	// stickiness + the health-blind marker. Default off (allowlist).
	FarmLivenessDetectionEnvVar = "FARM_LIVENESS_DETECTION_ENABLED"
	// FarmLivenessProbeEnvVar arms Phase 2: the serving-independent liveness
	// probe loop. Default off (allowlist).
	FarmLivenessProbeEnvVar = "FARM_LIVENESS_PROBE_ENABLED"
)

const (
	// farmLivenessProbeInterval is the base cadence of the serving-independent
	// liveness probe loop. It is deliberately minutes-scale (far above any
	// stress-test frequency) so the probe never looks like abusive traffic; the
	// per-account decision layered on top only probes an account whose own last
	// health refresh is already stale (see farmLivenessProbeStaleThreshold), so
	// the effective per-account rate is much lower than this loop tick.
	farmLivenessProbeInterval = 5 * time.Minute
	// farmLivenessProbeStaleThreshold is how stale an eligible account's last
	// health signal must be before the liveness probe re-checks it. It bounds
	// the effective per-account probe rate (account-level throttle) and ensures
	// the probe only covers accounts the normal quota poller is NOT keeping
	// fresh (frozen/blocked), never double-probing healthy accounts.
	farmLivenessProbeStaleThreshold = 30 * time.Minute
	// farmLivenessColdStartExemption exempts freshly-provisioned / cold-start
	// accounts from the liveness probe: an account whose first-production anchor
	// is younger than this (or that has no anchor yet, i.e. never served) is
	// skipped, honoring "别用刚投产新号压测" (do not probe brand-new accounts).
	farmLivenessColdStartExemption = 30 * time.Minute
	// farmLivenessProbeJitterMax is the maximum per-account random delay inserted
	// between consecutive probes within one loop pass, so the probe is serial and
	// jittered rather than a synchronized burst.
	farmLivenessProbeJitterMax = 15 * time.Second
	// farmLivenessProviderTimeout bounds a single probe's credential-acquisition +
	// request time. It matches the quota poller's own provider timeout.
	farmLivenessProviderTimeout = quotaSnapshotProviderTimeout
)

// quotaRefreshStatusHealthBlind is the quota-entry status surfaced for an
// ever-bound farm account the anti-corr gate is skipping from health probing
// (coreauth.FarmHealthBlind). It is distinct from ok/error/reauth_required/stale
// so the management projection can render it as unknown/health-blind (gray +
// alert) instead of a falsely-green cached snapshot. It is only ever written
// when FARM_LIVENESS_DETECTION_ENABLED is armed.
const quotaRefreshStatusHealthBlind = "health_blind"

const (
	// farmHealthBlindMetadataKey is the dedicated, projection-independent machine
	// signal that an ever-bound farm account is currently health-blind (see
	// coreauth.FarmHealthBlind). Unlike the quota_refresh_status channel it is not
	// subject to that field's display-precedence overrides (refresh_disabled etc.),
	// so upper layers (orchestrator/frontend alerting) have an unambiguous flag.
	farmHealthBlindMetadataKey = "farm_health_blind"
	// farmHealthBlindAtMetadataKey records when the health-blind state was first
	// observed (RFC3339 UTC), for "last confirmed alive" style rendering.
	farmHealthBlindAtMetadataKey = "farm_health_blind_at"
	// healthBlindQuotaErrorMessage is the sanitized, tokenless explanation stored
	// alongside the health-blind quota status.
	healthBlindQuotaErrorMessage = "container liveness heartbeat stale; account skipped by the anti-corr fail-closed gate and cannot be health-probed (health-blind)"
)

// farmLivenessDetectionEnabled reports whether Phase 1 authoritative escalation
// is armed. Allowlist parse (only a recognized truthy token arms it); unset /
// empty / unrecognized leaves it off, so a deployment never silently escalates
// during staged rollout.
func farmLivenessDetectionEnabled() bool {
	return parseFarmLivenessFlag(os.Getenv(FarmLivenessDetectionEnvVar))
}

// farmLivenessProbeEnabled reports whether Phase 2 (the serving-independent
// probe loop) is armed. Same allowlist / default-off shape as
// farmLivenessDetectionEnabled.
func farmLivenessProbeEnabled() bool {
	return parseFarmLivenessFlag(os.Getenv(FarmLivenessProbeEnvVar))
}

func parseFarmLivenessFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// farmLivenessProbeEligible reports whether an account may be actively probed by
// the serving-independent liveness probe. It is the leak-safety + noise-safety
// gate for Phase 2:
//
//   - claude only (other providers have no farm device_id / container concept);
//   - farm-enrolled only (never touch pre-existing/production-stable accounts);
//   - ever-bound-to-container only (AuthEverBoundToContainer) — its device_id is
//     ALREADY on-wire exposed, so re-probing adds no NEW leak surface; a
//     never-bound synthetic account is never probed (anti-corr leak boundary);
//   - past the cold-start exemption window — a brand-new / just-provisioned
//     account (young or missing first-production anchor) is skipped so the probe
//     never stress-tests a fresh account.
//
// It intentionally does NOT itself check freshness/blocked-state; the loop layers
// the account-level throttle (farmLivenessProbeStaleThreshold) on top so this
// predicate stays a pure "is this account allowed to be probed at all" test.
func farmLivenessProbeEligible(auth *coreauth.Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "claude" {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if !coreauth.AuthFarmEnrolled(auth) {
		return false
	}
	if !coreauth.AuthEverBoundToContainer(auth) {
		return false
	}
	return !farmAccountInColdStartWindow(auth, now)
}

// farmAccountInColdStartWindow reports whether an account is still within its
// cold-start exemption window: it has no first-production anchor yet (never
// actually served), or the anchor is younger than farmLivenessColdStartExemption.
// Such accounts are exempt from active probing.
func farmAccountInColdStartWindow(auth *coreauth.Auth, now time.Time) bool {
	anchor, ok := coreauth.AuthFirstProductionAt(auth)
	if !ok {
		return true
	}
	return now.Sub(anchor) < farmLivenessColdStartExemption
}
