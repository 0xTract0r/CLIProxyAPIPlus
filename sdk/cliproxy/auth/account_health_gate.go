package auth

import (
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// This file implements openspec/changes/add-adaptive-account-scheduling
// ANCHOR-Q4 ("养号放量按健康信号门控", design §10): the health-gated warm-up ramp.
//
// The pre-ANCHOR-Q4 warm-up ramp was purely calendar-based -- an account climbed
// stages by age alone (account_warmup.go / account_weight.go). ANCHOR-Q4 adds a
// SECOND gate: an account only reaches its age-deserved stage while it stays
// healthy; showing an early risk-control signal (distress) clamps its EFFECTIVE
// warm-up stage DOWN and holds it there until it recovers. The gate only ever
// LOWERS the effective stage (fail-safe, design §10.1 "只降不升") -- it can never
// push an account above the stage its age already earns.
//
// Effective stage = min(age-based stage, health-allowed stage cap). The clamp is
// applied on the READ side by feeding an "effective age" (the age clamped into
// the capped stage's day range) through the existing age->stage lookups, so BOTH
// the grading view (AccountWarmupStatusFor -> rpm/daily-budget/concurrency) AND
// the weight view (AccountFreshnessWeightFactor / AccountIsMature -> selection
// weight) see the lowered stage from one shared source of truth. The WRITE side
// (evaluateAccountHealthGate, driven from MarkResult) maintains the persisted cap
// and last-distress timestamp.
//
// Scope: Claude-only (design §10.7 -- codex AccountTierBaseWeight is 0 and is not
// managed by adaptive warm-up; xai/gemini likewise). Hard failures
// (quarantine / reauth / active cooldown) are NOT this layer's concern: those are
// already filtered out of the selectable pool by isAuthBlockedForModel, so the
// gate only reacts to the "still servable but showing early distress" band
// (design §10.3). v1 deliberately uses only the two signals core already tracks
// (recentRequests failure clusters + Quota.BackoffLevel); it adds no new precise
// 429 counter (design §10.3 "v1 取舍").

// accountSchedulingHealthStageCapKey / accountSchedulingLastDistressAtKey are the
// two ANCHOR-Q4 sub-keys stored inside the top-level account_scheduling metadata
// object (design §10.5): the current health-allowed max warm-up stage index and
// the wall-clock of the most recent distress. Like rate_scale / tier_source (and
// unlike tier_override / first_production_at) they have no legacy bare top-level
// form -- they only ever live inside the account_scheduling object, and survive a
// quota-refresh Clone because that object is a top-level key (design §6.4/§8.5).
const (
	accountSchedulingHealthStageCapKey = "warmup_health_stage_cap"
	accountSchedulingLastDistressAtKey = "warmup_last_distress_at"
)

// AccountHealthStageCap returns the persisted health-allowed maximum warm-up
// stage index for a, if one has been recorded. The index is into the configured
// warm-up curve (0 = the first/most-restrictive stage); len(curve) would mean
// "mature allowed" (no restriction), though the write path clears the cap
// entirely once it reaches the age-deserved stage rather than storing that.
//
// ok is false when a is nil, has no metadata, the key is absent, or the stored
// value is not a parseable integer -- callers treat that as "no health cap"
// (effective stage == age stage), the backward-compatible default.
func AccountHealthStageCap(a *Auth) (int, bool) {
	if a == nil || len(a.Metadata) == 0 {
		return 0, false
	}
	raw, ok := accountSchedulingRawValue(a.Metadata, accountSchedulingHealthStageCapKey)
	if !ok {
		return 0, false
	}
	return parseIntAny(raw)
}

// AccountLastDistressAt returns the wall-clock of a's most recent recorded
// distress, if any. ok is false when absent or unparseable. It reuses the same
// generic RFC3339/time.Time value parser the first-production anchor uses, so an
// in-memory time.Time fixture and a persisted-and-reloaded RFC3339 string behave
// identically.
func AccountLastDistressAt(a *Auth) (time.Time, bool) {
	if a == nil || len(a.Metadata) == 0 {
		return time.Time{}, false
	}
	raw, ok := accountSchedulingRawValue(a.Metadata, accountSchedulingLastDistressAtKey)
	if !ok {
		return time.Time{}, false
	}
	return parseFirstProductionAtValue(raw)
}

// setHealthStageCap / clearHealthStageCap / setLastDistressAt /
// clearLastDistressAt write the ANCHOR-Q4 state into the namespaced
// account_scheduling object (design §10.5), mirroring the first_production_at /
// rate_scale writers. Callers mutating a live shared *Auth must hold the same
// lock every other Metadata mutator in this package holds (m.mu) -- the sole
// production caller, evaluateAccountHealthGate via MarkResult, already does.
func (a *Auth) setHealthStageCap(cap int) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	setAccountSchedulingValue(a.Metadata, accountSchedulingHealthStageCapKey, cap)
}

func (a *Auth) clearHealthStageCap() {
	if a == nil || a.Metadata == nil {
		return
	}
	clearAccountSchedulingValue(a.Metadata, accountSchedulingHealthStageCapKey)
}

func (a *Auth) setLastDistressAt(t time.Time) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	setAccountSchedulingValue(a.Metadata, accountSchedulingLastDistressAtKey, t.UTC().Format(time.RFC3339))
}

func (a *Auth) clearLastDistressAt() {
	if a == nil || a.Metadata == nil {
		return
	}
	clearAccountSchedulingValue(a.Metadata, accountSchedulingLastDistressAtKey)
}

// ageWarmupStageIndex returns the warm-up stage index an anchored account of
// ageDays falls into: the matched curve index in [0, len(curve)-1], or
// len(curve) once the account is past every stage (mature). It mirrors
// AccountWarmupStageForAge's own loop (including the malformed-curve break that
// conservatively falls through to "mature" rather than guessing) so the health
// gate's notion of "which stage does this age deserve" cannot drift from the
// grading lookup. An empty curve returns 0 -- but callers gate on len(curve)==0
// before using this, so an empty curve never reaches here in practice.
func ageWarmupStageIndex(curve []internalconfig.AccountWarmupStage, ageDays int) int {
	if ageDays < 0 {
		ageDays = 0
	}
	for i, stage := range curve {
		if ageDays < stage.MinAgeDays {
			break
		}
		if stage.MaxAgeDays == 0 || ageDays < stage.MaxAgeDays {
			return i
		}
	}
	return len(curve)
}

// effectiveWarmupAge applies the ANCHOR-Q4 health cap to an account's raw age,
// returning the age to actually resolve the warm-up stage from (design §10.2:
// effective stage = min(age stage, health cap)). It is the single read-side clamp
// shared by AccountWarmupStatusFor (grading) and AccountFreshnessWeightFactor /
// AccountIsMature (weight) so both views see the same lowered stage.
//
// It is a strict pass-through (returns rawAgeDays/hasAnchor unchanged) in every
// case that must preserve pre-ANCHOR-Q4 behavior:
//   - no anchor yet (cold): the account is already at the most-restrictive stage,
//     nothing to cap; the cap only matters once an account is climbing the curve.
//   - health gate disabled (the backward-compatible / zero-value config state).
//   - no warm-up curve configured (no stages to cap).
//   - no health cap recorded for this account.
//   - the account is already MATURE (past every curve stage, ageIdx >= len(curve)):
//     ANCHOR-Q4's scope is warmup-period accounts only (spec "养号放量按健康信号
//     门控" 明写作用对象是"养号期账号"), so a mature account is governed by pure age
//     and any (stale) cap is ignored here -- a mature account showing distress is
//     already covered by cooldown / quota-deweight / auto-quarantine, and demoting
//     it into a warm-up daily-budget hard gate could 429 legitimate traffic on a
//     thin pool (production has a single mature 20x account, ANCHOR-2). The write
//     side (evaluateAccountHealthGate) likewise never records a cap for a mature
//     account, so in practice a mature account carries no cap to ignore.
//   - the recorded cap does not actually restrict (cap >= the age-based stage).
//
// Only when a recorded cap is strictly below the age-based stage does it clamp:
// it returns the lowest age that still lands in the capped stage
// (curve[cap].MinAgeDays), so the existing age->stage lookups resolve exactly the
// capped stage's limits and freshness. The clamp can only ever LOWER the stage
// (cap < ageIdx by construction here), never raise it -- the fail-safe invariant.
func effectiveWarmupAge(a *Auth, cfg internalconfig.AccountSchedulingConfig, rawAgeDays int, hasAnchor bool) (int, bool) {
	if !hasAnchor {
		return rawAgeDays, false
	}
	if !cfg.HealthGate.Enabled {
		return rawAgeDays, true
	}
	curve := cfg.WarmupCurve
	if len(curve) == 0 {
		return rawAgeDays, true
	}
	capIdx, ok := AccountHealthStageCap(a)
	if !ok {
		return rawAgeDays, true
	}
	ageIdx := ageWarmupStageIndex(curve, rawAgeDays)
	// Fix 1 (HIGH): the health gate governs warmup-period accounts only. A mature
	// account (ageIdx past the last curve stage) is out of scope -- ignore any
	// stale cap and pass its real age through so it is graded/weighted by pure age.
	if ageIdx >= len(curve) {
		return rawAgeDays, true
	}
	if capIdx >= ageIdx {
		return rawAgeDays, true
	}
	if capIdx < 0 {
		capIdx = 0
	}
	return curve[capIdx].MinAgeDays, true
}

// accountHealthGateApplies reports whether the health-gated warm-up ramp governs
// a. Claude-only (design §10.7): codex/xai/gemini are not managed by adaptive
// warm-up, so the write path never records a cap for them. The read path is
// naturally a no-op for them regardless (they never carry a cap), so this gate
// lives on the authoritative write side.
func accountHealthGateApplies(a *Auth) bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "claude")
}

// AccountInDistress reports whether a currently exhibits an early risk-control
// signal per cfg.HealthGate (design §10.3). Two OR-ed soft signals, both read
// from state core already tracks (no new counter introduced in v1):
//
//   - Quota.BackoffLevel (the escalating plan-quota 429 backoff exponent,
//     aggregated across models by updateAggregatedAvailability) at or above
//     BackoffLevelThreshold -- a 429 the account is actively backing off from.
//   - a cluster of >= FailureClusterThreshold failed requests within the last
//     ObservationWindowMinutes (the recentRequests success/failed ring).
//
// It is purely observational (no mutation) and provider-agnostic so the
// management projection can surface it for any account; the write-side gate is
// what restricts persistence to Claude. Returns false when the gate is disabled.
func AccountInDistress(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if a == nil || !cfg.HealthGate.Enabled {
		return false
	}
	gate := cfg.HealthGate
	if gate.BackoffLevelThreshold > 0 && a.Quota.BackoffLevel >= gate.BackoffLevelThreshold {
		return true
	}
	if gate.FailureClusterThreshold > 0 {
		window := time.Duration(gate.ObservationWindowMinutes) * time.Minute
		if a.recentFailuresWithin(now, window) >= int64(gate.FailureClusterThreshold) {
			return true
		}
	}
	return false
}

// recentFailuresWithin sums a's failed-request count over the recentRequests ring
// buckets covering the last `window` (design §10.3's failure cluster). It mirrors
// RecentRequestsSnapshot's bucket-walk (the same ring, the same modular index and
// bucketID staleness check) but only totals failures and only over the requested
// window rather than the whole ring. A non-positive window returns 0.
func (a *Auth) recentFailuresWithin(now time.Time, window time.Duration) int64 {
	if a == nil || window <= 0 {
		return 0
	}
	bucketSpan := time.Duration(recentRequestBucketSeconds) * time.Second
	buckets := int((window + bucketSpan - 1) / bucketSpan) // ceil to whole buckets
	if buckets < 1 {
		buckets = 1
	}
	if buckets > recentRequestBucketCount {
		buckets = recentRequestBucketCount
	}
	currentBucketID := recentRequestBucketID(now)
	var failed int64
	for i := 0; i < buckets; i++ {
		bucketID := currentBucketID - int64(i)
		bucket := a.recentRequests.buckets[recentRequestBucketIndex(bucketID)]
		if bucket.bucketID == bucketID {
			failed += bucket.failed
		}
	}
	return failed
}

// evaluateAccountHealthGate is the ANCHOR-Q4 write-side state machine (design
// §10.4), driven once per result from MarkResult AFTER all other status/quota
// mutations for that result (so BackoffLevel and the recentRequests ring are
// already up to date). It maintains the persisted health cap and last-distress
// timestamp; it never itself picks or throttles -- the read side consumes the cap.
//
// Transitions (only ever lowering the effective ramp on distress, re-raising only
// after sustained health -- design §10.1 "只降不升"):
//
//   - distress (either signal): clamp the cap to (min(existing cap, age stage) -
//     DemoteStep), floored at 0, and stamp last_distress_at = now. This can only
//     lower the cap. last_distress_at is refreshed on every distress hit so the
//     promote cooldown restarts.
//   - healthy success, cap present, and >= PromoteCooldownMinutes since the last
//     distress: raise the cap by ONE stage toward the age-deserved stage and
//     re-stamp last_distress_at = now (Fix 2, see below) so the next promote must
//     wait a fresh cooldown window; once the cap reaches the age stage, drop it
//     entirely (back to pure age-based) and clear the timestamp.
//   - healthy success still inside the cooldown, or a plain failure that is not
//     (yet) a distress cluster: hold the cap unchanged.
//
// "不许靠熬账龄绕过" (design §10.4): promotion requires health + cooldown, never
// age alone -- an account that keeps failing never climbs no matter how old it is,
// because distress keeps re-clamping and resetting the cooldown.
//
// Scope guards (the gate is a warmup-period-only mechanism, spec "养号期账号"):
//   - Fix 3 (LOW): an account with no first_production_at anchor yet ("cold")
//     early-returns without writing anything. The read side already pass-throughs a
//     cold account (effectiveWarmupAge's no-anchor branch), so a cap here would be
//     dead metadata -- skipping it avoids pointless persistence churn.
//   - Fix 1 (HIGH): a MATURE account (ageIdx past the last curve stage)
//     early-returns without writing/stamping. Mature accounts are out of the
//     health gate's scope; they are covered by cooldown / quota-deweight /
//     auto-quarantine and must not be demoted into a warm-up daily-budget hard
//     gate (which could 429 legitimate traffic on a thin pool). The read side
//     likewise ignores any cap for a mature account.
//
// Claude-only and gated on cfg.HealthGate.Enabled + a configured curve; a no-op
// (no metadata write) in every other case and whenever nothing actually changes,
// so it adds no persistence churn on the healthy steady state.
func evaluateAccountHealthGate(auth *Auth, success bool, now time.Time, cfg internalconfig.AccountSchedulingConfig) {
	if auth == nil || !cfg.HealthGate.Enabled || !accountHealthGateApplies(auth) {
		return
	}
	curve := cfg.WarmupCurve
	if len(curve) == 0 {
		return
	}

	rawAge, hasAnchor := AccountAgeDays(auth, now)
	if !hasAnchor {
		// Fix 3 (LOW): cold account (no anchor) -- read side pass-throughs it, so a
		// cap/stamp here is dead metadata. Skip to avoid persistence churn.
		return
	}
	ageIdx := ageWarmupStageIndex(curve, rawAge)
	if ageIdx >= len(curve) {
		// Fix 1 (HIGH): mature account -- out of the warmup-period scope. Never
		// record a cap/stamp for it; the read side ignores any stale cap too.
		return
	}
	curCap, hasCap := AccountHealthStageCap(auth)

	if AccountInDistress(auth, cfg, now) {
		// Demote: base is the current effective ceiling -- the age stage, further
		// lowered by any existing cap -- so a repeated distress hit keeps stepping
		// down from where it already is rather than resetting to the age stage.
		base := ageIdx
		if hasCap && curCap < base {
			base = curCap
		}
		newCap := base - cfg.HealthGate.DemoteStep
		if newCap < 0 {
			newCap = 0
		}
		auth.setLastDistressAt(now)
		// newCap <= base <= curCap (when a cap already exists), so this only ever
		// lowers; skip the write when the value is unchanged (already floored).
		if !hasCap || newCap != curCap {
			auth.setHealthStageCap(newCap)
		}
		return
	}

	// Not in distress. A plain failure (below the cluster threshold) neither
	// promotes nor demotes -- only a healthy SUCCESS may re-raise the cap.
	if !success || !hasCap {
		return
	}
	if last, okLast := AccountLastDistressAt(auth); okLast {
		if now.Sub(last) < promoteCooldownWindow(cfg) {
			return // still cooling down -- hold the cap
		}
	}
	newCap := curCap + 1
	if newCap >= ageIdx {
		// Reached (or passed) the age-deserved stage: drop the cap entirely so the
		// account is governed by pure age again, and clear the now-moot distress
		// timestamp so no stale cooldown lingers.
		auth.clearHealthStageCap()
		auth.clearLastDistressAt()
		return
	}
	auth.setHealthStageCap(newCap)
	// Fix 2 (MEDIUM): rate-limit the re-ramp to at most one stage per cooldown
	// window. Re-stamp last_distress_at = now so a burst of healthy successes cannot
	// each bump the cap +1 back to full within seconds -- the next promote must wait
	// a fresh PromoteCooldownMinutes. During recovery this timestamp therefore
	// doubles as the "last ramp step" clock (it is the same value the promote gate
	// above reads); it is not a fresh distress. This keeps the persistence surface
	// minimal (design §10.5 "复用 first_production_at 一套 / 零新持久化子系统")
	// rather than introducing a separate last_promote_at sub-key.
	auth.setLastDistressAt(now)
}

// promoteCooldownWindow is cfg.HealthGate.PromoteCooldownMinutes as a Duration.
func promoteCooldownWindow(cfg internalconfig.AccountSchedulingConfig) time.Duration {
	return time.Duration(cfg.HealthGate.PromoteCooldownMinutes) * time.Minute
}

// evaluateAccountHealthGateLocked resolves the live AccountSchedulingConfig from
// the manager's runtime config snapshot and drives evaluateAccountHealthGate for
// this result. It is the MarkResult hook (design §10.4). Callers must hold m.mu
// (MarkResult does). A nil/unset runtime config, or a config with the gate
// disabled, makes this a no-op -- so before SetConfig has ever run (e.g. a bare
// NewManager in a test) the gate never fires and existing MarkResult behavior is
// unchanged.
func (m *Manager) evaluateAccountHealthGateLocked(auth *Auth, success bool, now time.Time) {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return
	}
	evaluateAccountHealthGate(auth, success, now, cfg.AccountScheduling)
}
