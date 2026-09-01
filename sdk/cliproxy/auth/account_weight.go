package auth

import (
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// This file implements openspec/changes/add-adaptive-account-scheduling
// tasks.md Phase 1 task 1.1: the pure account-selection-weight function the
// later-phase adaptive Selector (task 1.2, not this file) will consume when
// routing.strategy == internalconfig.RoutingStrategyAdaptive.
//
// design.md D1 defines the weighting axis:
//
//	weight = tier base capacity x (1 - quota utilization%) x freshness factor
//
// Every input this file reads is a pure read of already-persisted or
// caller-supplied state (Phase 0's ClaudeSubscriptionTier/CodexSubscriptionTier,
// ParseAccountQuotaUtilization/AccountQuotaHeadroom, AuthFirstProductionAt/
// AccountAge, and the AccountSchedulingConfig loaded at startup) -- this file
// introduces no new persistence, no caching, and no I/O. Given the same
// (*Auth, AccountSchedulingConfig, now) triple it always returns the same
// float64.

// unknownAccountQuotaHeadroom is the fallback headroom used when
// AccountQuotaHeadroom cannot determine a real value (ok=false: no
// quota_snapshot has been polled yet, or the snapshot has no recognizable
// usage window -- see account_quota.go's AccountQuotaHeadroom doc, which
// explicitly leaves this exact policy call to Phase 1).
//
// This is deliberately NOT 1.0 (full headroom): design.md's stated bias is
// that "unknown" must never be read as "safe to flood" (design.md §1.1/D1,
// account_quota.go's own doc comment). It is also deliberately NOT 0.0 (zero
// headroom, i.e. total exclusion from weighted selection): a Claude/Codex
// account that simply has not completed its first ~45min quota poll yet (see
// account_quota.go's file doc) is not necessarily near its limit, and a
// freshly-added account with no anchor is, by the sibling policy this file
// mirrors (see accountFreshnessWeightFactor's no-anchor branch below),
// treated as mature rather than perpetually starved -- zeroing its quota
// factor here would silently contradict that and could permanently exclude
// an otherwise-healthy account from ever winning a weighted pick, so it would
// never get the chance to complete a real quota poll in the first place.
// 0.5 is a documented, deliberately-conservative middle value (below the
// "fully available" 1.0, above total exclusion) rather than a value derived
// from design.md numerics -- design.md gives no specific fallback number for
// this case, so this is flagged in this slice's gaps for the maintainers to
// revisit/calibrate (or promote to a config field) once real unpolled-account
// behavior is observed in 201 test (see design.md O2's sibling calibration
// pattern).
const unknownAccountQuotaHeadroom = 0.5

// AccountSelectionWeight computes a's current adaptive-selector weight:
//
//	weight = AccountTierBaseWeight(a) x AccountQuotaWeightFactor(a) x AccountFreshnessWeightFactor(a, cfg, now)
//
// per design.md D1. now is caller-supplied (never time.Now() internally) so
// results are deterministic and testable; production call sites (task 1.2,
// not this file) pass the real wall-clock time at selection time.
//
// Returns 0 for a nil Auth, for a provider AccountTierBaseWeight does not
// recognize (see its doc -- weights are only meaningful within a single
// provider, so an unrecognized provider must not receive a guessed nonzero
// weight), or for any weight configuration that would otherwise resolve
// negative (defensive clamp; AccountSchedulingConfig.Validate already
// rejects non-positive tier weights and limits at config-load time, so this
// clamp exists only to keep this function panic-/NaN-safe against a config
// that reached this call site without having been validated, not because
// well-formed config is expected to hit it).
//
// This function never selects, mutates, or persists anything -- it is a pure
// scoring function. What the adaptive Selector does with the score (weighted
// random pick, sort, tie-break against session affinity, etc.) is task 1.2's
// concern.
func AccountSelectionWeight(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) float64 {
	if a == nil {
		return 0
	}
	base := a.AccountTierBaseWeight(cfg.TierWeights)
	if base <= 0 {
		// Either an unrecognized provider (AccountTierBaseWeight's documented
		// 0 return) or a config that assigns this tier a non-positive weight
		// on purpose (e.g. Unknown: 0 to hard-exclude unrecognized tiers).
		// Either way there is nothing left to compute -- skip the quota/
		// freshness reads entirely rather than doing pointless work whose
		// result is multiplied by zero anyway.
		return 0
	}

	weight := base * AccountQuotaWeightFactor(a) * AccountFreshnessWeightFactor(a, cfg, now)
	if weight < 0 {
		return 0
	}
	return weight
}

// AccountQuotaWeightFactor returns a's current quota-headroom multiplier in
// [0,1]: the tightest (most-exhausted) known usage window's headroom
// (AccountQuotaHeadroom -- already 1-utilization%, clamped [0,1]), or
// unknownAccountQuotaHeadroom when no usable quota_snapshot exists yet.
//
// Exported (not folded into AccountSelectionWeight as a private helper) so
// this axis is independently unit-testable per this slice's brief ("额度低
// 权重低"), and so later phases that need "how much quota headroom does this
// account have right now" for a different purpose (e.g. a management-API
// surfaced field, task 5.2) can reuse this exact, already-tested read path
// instead of re-deriving it.
func AccountQuotaWeightFactor(a *Auth) float64 {
	result, ok := AccountQuotaHeadroom(a)
	if !ok {
		return unknownAccountQuotaHeadroom
	}
	return result.Headroom
}

// AccountFreshnessWeightFactor is THE single source of truth for the freshness
// axis of the live adaptive selector's weight (design.md D1: weight = tier
// capacity x quota headroom x freshness factor). It returns a's current warm-up
// freshness multiplier in [0,1]: <1 while the account is inside cfg.WarmupCurve's
// age-based stages (design.md D1/D3/D4: a warming account must be weighted down
// so traffic -- and especially a workflow-style burst -- prefers a mature account
// instead), and exactly 1 once the account is mature.
//
// Do not confuse this with AccountWarmupStatus.FreshnessFactor
// (account_warmup.go): that is a separate warm-up-status *view* the selector does
// NOT consume for weighting, and it deliberately diverges from this function on
// the no-anchor case (the warmup view pins un-anchored accounts to 0 "cold" as an
// anti-ban fail-safe, whereas this weight view returns 1 -- the bootstrap
// documented in case 1 below). AccountSelectionWeight consumes only this
// function; the warmup view exists for observability / the warmup unit tests.
//
// "Mature" here covers three cases, all intentionally returning 1 rather
// than being treated as still-warming:
//
//  1. No first-production anchor is recorded yet (AccountAgeDays ok=false).
//     This mirrors the precedent already set for this exact ambiguity by
//     AccountSchedulingConfig.MatureLimits' doc comment (account_scheduling.go):
//     "has no usable freshness anchor and is conservatively treated as
//     mature rather than perpetually throttled". A legacy credential that
//     predates this change, or one whose anchor has not been minted yet by
//     whatever Phase-1-integration call site does the minting (task 1.2's
//     concern, not this file's), must not be silently punished with the
//     harshest (first-stage) warm-up weight forever.
//  2. The account's age is beyond every configured stage's upper bound (it
//     has graduated the curve).
//  3. cfg.WarmupCurve is empty (an operator can configure an empty curve to
//     disable warm-up throttling entirely; an empty curve trivially matches
//     no stage, which correctly falls through to "mature").
//
// The per-stage multiplier itself (case: account age matches a configured
// stage) is stage.RPMLimit / cfg.MatureLimits.RPMLimit, clamped to [0,1].
// design.md's warm-up table (§5.1) does not specify an exact weight-factor
// formula -- it only specifies the daily-budget/rpm/concurrency *throttle*
// values a warming account is capped at (a separate mechanism, Phase 2/3's
// per-account token bucket, not this weight function). This ratio is chosen
// as a principled, config-consistent proxy for "how much of a mature
// account's capacity this stage represents" using fields the config already
// defines for an unrelated purpose, rather than inventing a new unconfigured
// magic-number curve: it is monotonically increasing across the default
// curve (w1=3/45≈0.067 ... w7-8=30/45≈0.667) and reaches 1 only once an
// account is actually mature, which matches design.md D3/D4's intent
// ("早期极低...逐周抬升") without requiring a second, independently-tuned
// weight table alongside WarmupCurve. This is flagged in this slice's gaps
// as a deliberate design choice open to recalibration, since design.md
// itself does not pin an exact numeric formula for this specific axis.
func AccountFreshnessWeightFactor(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) float64 {
	ageDays, ok := AccountAgeDays(a, now)
	if !ok {
		return 1
	}

	stage, matched := currentAccountWarmupStage(cfg.WarmupCurve, ageDays)
	if !matched {
		return 1
	}

	matureRPM := cfg.MatureLimits.RPMLimit
	if matureRPM <= 0 || stage.RPMLimit <= 0 {
		// Defensive only: AccountSchedulingConfig.Validate rejects a
		// non-positive mature-limits.rpm-limit or warmup-curve[*].rpm-limit
		// at config-load time, so a well-formed config never reaches this
		// branch. A pure function must still never divide by zero or return
		// a garbage negative/NaN factor if it is ever called with an
		// unvalidated config (e.g. from a future caller that builds one by
		// hand in a test or a not-yet-validated hot-reload path) -- fall
		// back to "no additional warm-up penalty" rather than panicking.
		return 1
	}

	factor := float64(stage.RPMLimit) / float64(matureRPM)
	if factor < 0 {
		return 0
	}
	if factor > 1 {
		return 1
	}
	return factor
}

// AccountIsMature reports whether a is past its warm-up curve as of now,
// using the exact same anchor/stage-lookup rules as
// AccountFreshnessWeightFactor (including the same "no anchor recorded ->
// mature" and "empty curve -> mature" fallbacks). It is the weight-side
// maturity counterpart of that freshness factor, so it stays consistent with
// the weight bootstrap: an un-anchored account is reported mature here for the
// same reason its freshness factor is 1 (do not perpetually starve/throttle a
// credential that has not yet had a chance to mint its anchor).
//
// NOTE (single source of truth): this is NOT the maturity signal the live
// adaptive selector uses for design D5 / spec.md's "会话粘性与限额冲突分级"
// stickiness grading. AdaptiveSelector.isMature reads
// AccountWarmupStatusFor(...).Mature (account_warmup.go) instead, which is
// fail-safe "cold" (not mature) on the no-anchor case -- deliberately the
// opposite of this function on that one case, so an un-anchored account can win
// a weighted pick (weight-side "mature" here) yet does not hold stickiness or
// absorb floods until actually anchored (grading-side "cold" there). This
// weight-side classifier is retained as a tested boolean convenience (avoiding
// an unsafe factor==1 comparison, since a hand-tuned warming stage's RPMLimit
// could coincidentally equal cfg.MatureLimits.RPMLimit); it must not be
// swapped in for the selector's grading maturity without reconciling that
// intentional no-anchor divergence.
func AccountIsMature(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	ageDays, ok := AccountAgeDays(a, now)
	if !ok {
		return true
	}
	_, matched := currentAccountWarmupStage(cfg.WarmupCurve, ageDays)
	return !matched
}

// currentAccountWarmupStage returns the first stage in curve whose
// [MinAgeDays, MaxAgeDays) range contains ageDays (MaxAgeDays == 0 means
// unbounded), and whether a match was found. A linear scan is deliberate and
// sufficient here: curve is at most a handful of stages (design.md §5.1's
// default curve has 5), well-formed config (AccountSchedulingConfig.Validate)
// guarantees the stages are contiguous and sorted, and this function does not
// itself assume that ordering -- it takes the first range-containing match
// regardless of slice order, so it degrades gracefully (rather than picking
// silently-wrong stage) against a config that reached this call site without
// having been validated.
func currentAccountWarmupStage(curve []internalconfig.AccountWarmupStage, ageDays int) (internalconfig.AccountWarmupStage, bool) {
	for _, stage := range curve {
		if ageDays < stage.MinAgeDays {
			continue
		}
		if stage.MaxAgeDays == 0 || ageDays < stage.MaxAgeDays {
			return stage, true
		}
	}
	return internalconfig.AccountWarmupStage{}, false
}
