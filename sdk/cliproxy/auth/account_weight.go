package auth

import (
	"strings"
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
// mirrors (see AccountFreshnessWeightFactor's no-anchor branch below),
// weighted down to the curve's cold first-stage factor but kept strictly
// above zero rather than perpetually starved -- zeroing its quota factor here
// would silently contradict that low-but-nonzero trickle and could permanently
// exclude an otherwise-healthy account from ever winning a weighted pick, so it
// would never get the chance to complete a real quota poll in the first place.
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
//	weight = warmupClampedBase(a) x AccountQuotaWeightFactor(a) x AccountFreshnessWeightFactor(a, cfg, now)
//
// where warmupClampedBase(a) is AccountTierBaseWeight(a) for a mature account
// and min(AccountTierBaseWeight(a), warmupBaseCeiling(a)) while the account is
// still warming (design.md D1, with the warm-up base clamp added to close the
// bug where a warming high-tier account was over-weighted into a primary
// traffic-bearing role -- see the clamp block below). now is caller-supplied
// (never time.Now() internally) so results are deterministic and testable;
// production call sites (task 1.2, not this file) pass the real wall-clock time
// at selection time.
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

	// Warm-up base clamp (design.md D3/D4, spec.md "新账号养号期渐进放量"): while
	// an account is still warming, cap its tier base at this provider's warm-up
	// baseline ceiling (Claude Pro / Codex Plus) so a high-tier credential (e.g.
	// a max_20x pinned via tier_override) cannot ride its full tier base into a
	// primary traffic-bearing role BEFORE it has matured. Without this clamp a
	// warming max_20x's base (20) alone dwarfs a mature Pro's base (1), so even
	// after the freshness factor it can still out-weight and starve the pool --
	// the production-observed bug where a brand-new w1 account took ~77% of the
	// selection share.
	//
	// The maturity test MUST be the grading-side AccountWarmupStatusFor(...)
	// .Mature, NOT AccountIsMature: AccountIsMature reports a no-anchor ("cold")
	// credential mature (its weight-bootstrap divergence, see its doc), which
	// would skip the clamp for exactly the freshly-added account this guards
	// against. AccountWarmupStatusFor treats both the no-anchor cold state and
	// every in-curve stage as NOT mature, so the clamp covers them all; a truly
	// mature account (past the curve) skips the clamp and keeps its full tier
	// base, leaving the mature max_20x:5x:pro distribution completely unchanged.
	base = warmupClampedBase(a, base, cfg, now)

	weight := base * AccountQuotaWeightFactor(a) * AccountFreshnessWeightFactor(a, cfg, now)
	if weight < 0 {
		return 0
	}
	return weight
}

// warmupClampedBase returns base unchanged for a mature account (grading-side
// AccountWarmupStatusFor(...).Mature == true) and min(base, warmupBaseCeiling)
// while the account is still warming. A non-positive ceiling (a provider this
// scheduler does not tier-weight, or a config with no positive baseline weight)
// is treated as "do not clamp" so the clamp can only ever LOWER a warming
// account's base, never zero out or raise it.
func warmupClampedBase(a *Auth, base float64, cfg internalconfig.AccountSchedulingConfig, now time.Time) float64 {
	if AccountWarmupStatusFor(a, now, cfg).Mature {
		return base
	}
	ceiling := warmupBaseCeiling(a, cfg)
	if ceiling > 0 && ceiling < base {
		return ceiling
	}
	return base
}

// warmupBaseCeiling returns the base-capacity weight a still-warming account of
// a's provider is clamped to: the provider's lowest legitimate paid-tier weight
// (Claude Pro, Codex Plus), read from the SAME TierWeights config the tier base
// itself comes from -- never a hardcoded number -- so an operator who retunes
// the tier table moves the ceiling with it. Returns 0 for a nil auth or a
// provider this scheduler does not tier-weight (the caller reads a non-positive
// ceiling as "do not clamp"; such a provider already scored base <= 0 and never
// reaches the clamp anyway).
func warmupBaseCeiling(a *Auth, cfg internalconfig.AccountSchedulingConfig) float64 {
	if a == nil {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "claude":
		return cfg.TierWeights.Claude.Pro
	case "codex":
		return cfg.TierWeights.Codex.Plus
	default:
		return 0
	}
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
// NOT consume for weighting, and it still diverges from this function on the
// no-anchor case, but only in DEGREE now, not direction: the warmup view pins an
// un-anchored account to exactly 0 "cold" (a hard anti-ban fail-safe that would
// make it unselectable), whereas this weight view resolves the same account to
// the curve's cold FIRST-stage factor (strictly > 0 but well below 1) so it is a
// low-priority trickle candidate that can still occasionally win a pick and mint
// its anchor, rather than being either starved (0) or -- as this function used to
// do -- handed a full 1.0 bootstrap that let a fresh high-tier credential
// dominate the pool. AccountSelectionWeight consumes only this function; the
// warmup view exists for observability / the warmup unit tests.
//
// The no-anchor "cold" case (case 1 below) returns the curve's first-stage
// factor. The remaining two cases intentionally return exactly 1 (fully mature,
// no warm-up penalty):
//
//  1. No first-production anchor is recorded yet (AccountAgeDays ok=false):
//     design §5.1's "cold" state. This mirrors the grading-side
//     coldAccountWarmupStatus (account_warmup.go), which likewise resolves a
//     cold account to the curve's first (most-restrictive) stage. It returns
//     that first stage's factor (curve[0].RPMLimit / MatureLimits.RPMLimit,
//     ≈0.067 on the default curve) rather than 1: a freshly-added credential --
//     especially a max_20x pinned via tier_override -- must NOT be handed full
//     freshness and allowed to out-weight the mature pool before it has proven
//     itself (the warm-up weight bug). It is still strictly > 0 (never the grading
//     view's hard 0), so the account is not perpetually starved and can win the
//     occasional pick that mints its anchor on first real use. When cfg.WarmupCurve
//     is empty there is no first stage to be cold relative to, so this falls
//     through to case 3 (mature, 1) instead.
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
	matureRPM := cfg.MatureLimits.RPMLimit

	ageDays, ok := AccountAgeDays(a, now)
	if !ok {
		// No first-production anchor yet -> design §5.1's "cold" state (case 1
		// in the doc). Weight it at the curve's FIRST (most-restrictive) stage's
		// factor, matching the grading-side coldAccountWarmupStatus, rather than
		// the old full-1.0 bootstrap that let a fresh high-tier credential
		// dominate the pool. An empty curve has no first stage to be cold
		// relative to -> fall through to mature (1), consistent with the
		// anchored empty-curve path below.
		if len(cfg.WarmupCurve) == 0 {
			return 1
		}
		return warmupRPMFreshnessFactor(cfg.WarmupCurve[0].RPMLimit, matureRPM)
	}

	stage, matched := currentAccountWarmupStage(cfg.WarmupCurve, ageDays)
	if !matched {
		return 1
	}
	return warmupRPMFreshnessFactor(stage.RPMLimit, matureRPM)
}

// warmupRPMFreshnessFactor is the shared stage-freshness formula for the weight
// view: stageRPM / matureRPM, clamped to [0,1]. Extracted so the in-curve stage
// match and the no-anchor "cold -> curve[0]" bootstrap compute the exact same
// factor from the exact same fields.
//
// It falls back to 1 ("no additional warm-up penalty") when either rpm is
// non-positive. That is defensive only: AccountSchedulingConfig.Validate rejects
// a non-positive mature-limits.rpm-limit or warmup-curve[*].rpm-limit at
// config-load time, so a well-formed config never hits it -- but a pure function
// must still never divide by zero or return a garbage negative/NaN factor if
// ever called with an unvalidated config (e.g. a future caller that builds one
// by hand in a test or a not-yet-validated hot-reload path).
func warmupRPMFreshnessFactor(stageRPM, matureRPM int) float64 {
	if matureRPM <= 0 || stageRPM <= 0 {
		return 1
	}
	factor := float64(stageRPM) / float64(matureRPM)
	if factor < 0 {
		return 0
	}
	if factor > 1 {
		return 1
	}
	return factor
}

// AccountIsMature reports whether a is past its warm-up curve as of now, using
// the same anchor/stage-lookup rules as AccountFreshnessWeightFactor for the
// anchored cases, plus the same "empty curve -> mature" fallback. It returns
// true for a no-anchor credential (AccountAgeDays ok=false).
//
// NOTE (divergence from the freshness factor on the no-anchor case): this
// boolean still reports a no-anchor account mature, but AccountFreshnessWeightFactor
// no longer returns 1 for that account -- it now returns the curve's cold
// first-stage factor (< 1). The two stopped being a strict 1<->true / <1<->false
// pair specifically on no-anchor: this classifier keeps the weight-side
// "un-anchored accounts are not perpetually starved" intent (an un-anchored
// account is still selectable), while the freshness axis independently pulls its
// weight down toward the cold stage. This function is NOT used by
// AccountSelectionWeight's warm-up clamp (that uses the grading-side
// AccountWarmupStatusFor(...).Mature, which reports no-anchor as NOT mature, so
// the clamp still applies to a fresh high-tier credential).
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
