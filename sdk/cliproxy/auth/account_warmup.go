package auth

import (
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// This file implements openspec/changes/add-adaptive-account-scheduling
// tasks.md Phase 3 task 3.1: a pure function mapping an account's age (since
// its first_production_at anchor, Phase 0 -- account_freshness.go) against
// the configured warm-up curve (internal/config/account_scheduling.go) to
// that account's currently effective per-account limits (daily budget / rpm /
// concurrency) and a 0..1 "freshness factor" for the Phase 1 weight function
// (design.md D1: weight = tier capacity x quota headroom x freshness factor).
//
// Scope boundary: this file only resolves "what stage is this account in
// right now, and what does that stage allow". It does not itself enforce
// anything (no token bucket, no request counting -- that is Phase 2, tasks.md
// 2.1) and does not itself decide selection weight (Phase 1, tasks.md 1.1)
// or sticky-session override behavior (Phase 4, tasks.md 4.1). Those phases
// are expected to call AccountWarmupStatusFor (or the lower-level
// AccountWarmupStageForAge) and consume its result.

// AccountWarmupStatus is the resolved outcome of mapping an account's age to
// the design §5.1 warm-up curve: the effective per-account limits at this
// exact age, plus a freshness factor for the Phase 1 weight function.
type AccountWarmupStatus struct {
	// StageName identifies which stage this account currently falls under:
	// a config.AccountWarmupStage.Name (e.g. "w1", "w7-8") for an in-curve
	// account, the synthetic "cold" label for a not-yet-anchored account (see
	// accountWarmupColdStageName), or the synthetic "mature" label once past
	// the curve entirely (accountWarmupMatureStageName).
	StageName string

	// DailyBudget is the max requests/day this stage allows. 0 means
	// unbounded: always 0 for the mature state (design §5.1: "按额度打满为
	// 准，不设日预算" -- quota headroom governs instead of a fixed daily
	// cap), and only 0 for an in-curve stage if the configured
	// config.AccountWarmupStage.DailyBudget for that stage is itself 0.
	DailyBudget int

	// RPMLimit is the requests-per-minute burst-smoothing ceiling for this
	// stage -- a coarse anti-burst backstop; DailyBudget (when nonzero) is
	// the primary throttle signal (design §5).
	RPMLimit int

	// ConcurrencyLimit is the max concurrent in-flight requests for one
	// account at this stage.
	ConcurrencyLimit int

	// FreshnessFactor is the design D1 weight multiplier for this stage, in
	// [0,1]: exactly 0 for a not-yet-anchored ("cold") account, ramping
	// upward (strictly less than 1) while inside the configured warm-up
	// curve, and exactly 1 once Mature is true. It is an explicit resolved
	// value -- not derived from DailyBudget/RPMLimit/ConcurrencyLimit -- so
	// operators can tune warm-up throttling (the limits above) independently
	// of how aggressively the weighted selector should deprioritize a
	// warming account (see warmupFreshnessFactor for the ramp formula and
	// its documented rationale).
	FreshnessFactor float64

	// Mature reports whether the account is past the configured warm-up
	// curve entirely (config.AccountMatureLimitsConfig applies). False for
	// both the "cold" (no anchor yet) state and any in-curve stage -- an
	// account is Mature only once its age has advanced beyond every
	// configured stage's upper bound (or no curve is configured at all, in
	// which case every account is trivially mature -- see
	// AccountWarmupStageForAge).
	Mature bool
}

// accountWarmupColdStageName / accountWarmupMatureStageName are the synthetic
// stage names AccountWarmupStatus.StageName carries for the two states that
// are not literal config.AccountWarmupStage entries: "cold" (design §5.1's
// pre-first-production state -- an account that has never yet served a real
// request, so it has no first_production_at anchor at all) and "mature"
// (past the configured curve, config.AccountMatureLimitsConfig applies).
const (
	accountWarmupColdStageName   = "cold"
	accountWarmupMatureStageName = "mature"
)

// AccountWarmupStatusFor resolves auth's current warm-up stage as of now,
// deriving its age via AccountAgeDays (Phase 0's first_production_at anchor,
// account_freshness.go) and looking it up against cfg's warm-up curve and
// mature ceiling (internal/config/account_scheduling.go). This is the
// convenience entry point later phases (Phase 1 weight function, Phase 2
// token bucket, Phase 4 sticky-session conflict handling) are expected to
// call; the actual pure stage-lookup logic lives in
// AccountWarmupStageForAge, which this simply wires up with the Phase 0 age
// derivation.
//
// now is caller-supplied (not time.Now()) for the same determinism reason as
// every other function in this package that takes a "now" (see
// account_freshness.go's AccountAge/AccountAgeDays doc).
func AccountWarmupStatusFor(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) AccountWarmupStatus {
	ageDays, hasAnchor := AccountAgeDays(a, now)
	return AccountWarmupStageForAge(ageDays, hasAnchor, cfg.WarmupCurve, cfg.MatureLimits)
}

// AccountWarmupStageForAge is the pure age -> warm-up-status mapping (design
// §5.1): given an account's age in whole days since its first_production_at
// anchor (ageDays, meaningful only when hasAnchor is true -- matching
// AccountAgeDays' own (int, bool) contract) plus the configured warm-up curve
// and mature ceiling, it returns that account's currently effective limits
// and freshness factor. Deterministic and side-effect free: no clock reads,
// no I/O, no mutation of curve/mature.
//
// hasAnchor=false is design §5.1's "冷置" (cold-storage) state: an account
// that has never yet served a real request, so AuthFirstProductionAt/
// AccountAgeDays returned ok=false. This is NOT the same as "age 0" -- age 0
// means the account's very first production request already happened (the
// anchor was just minted) and it has entered the curve's first stage; "cold"
// means that first request has not happened yet. A cold account is resolved
// to the curve's first (necessarily most-restrictive, since Validate
// requires curve[0].MinAgeDays == 0) configured stage's limits, so a fresh
// account is still allowed a conservative trickle of traffic -- enough for
// EnsureAuthFirstProductionAt to eventually mint its anchor on first actual
// use -- while its FreshnessFactor is pinned to 0 (strictly lower than that
// same first stage's own age-based ramp value, matching the task brief's
// "冷置...极低": at least as conservative as day 0 of the curve, never more
// lenient). ageDays is ignored entirely when hasAnchor is false.
//
// When curve is empty (no warm-up curve configured -- e.g. a caller-
// constructed zero-value AccountSchedulingConfig that bypassed
// DefaultAccountSchedulingConfig/config-load defaulting), there is no curve
// to be "cold" or "in-stage" relative to, so every account -- cold or aged --
// is resolved directly to the mature ceiling.
//
// Age boundaries are [MinAgeDays, MaxAgeDays): MinAgeDays is inclusive,
// MaxAgeDays is exclusive, and MaxAgeDays == 0 means that stage is unbounded
// (only valid, and only meaningful, on the curve's last stage -- see
// config.AccountWarmupStage and config.validateAccountWarmupCurve). This
// function assumes curve has already passed
// AccountSchedulingConfig.Validate() (contiguous, ascending, non-overlapping,
// starting at MinAgeDays 0): given a Validate()-clean curve, every
// non-negative ageDays falls into exactly one stage or, once past the last
// stage's MaxAgeDays (when finite), is mature. On a malformed curve that
// somehow reached this function anyway (a gap before the first stage, or a
// negative MinAgeDays), the loop conservatively falls through to mature
// rather than panicking or misattributing a stage -- it does not attempt to
// second-guess or repair an invalid curve.
func AccountWarmupStageForAge(ageDays int, hasAnchor bool, curve []internalconfig.AccountWarmupStage, mature internalconfig.AccountMatureLimitsConfig) AccountWarmupStatus {
	if len(curve) == 0 {
		return matureAccountWarmupStatus(mature)
	}
	if !hasAnchor {
		return coldAccountWarmupStatus(curve)
	}
	if ageDays < 0 {
		ageDays = 0
	}
	for _, stage := range curve {
		if ageDays < stage.MinAgeDays {
			// A gap before this stage on an otherwise-Validate()-clean curve
			// cannot happen (stage 0 always starts at MinAgeDays 0); this is
			// only reachable on a malformed curve, and the safe fallback is
			// to fall through to mature rather than guess.
			break
		}
		if stage.MaxAgeDays == 0 || ageDays < stage.MaxAgeDays {
			return AccountWarmupStatus{
				StageName:        stage.Name,
				DailyBudget:      stage.DailyBudget,
				RPMLimit:         stage.RPMLimit,
				ConcurrencyLimit: stage.ConcurrencyLimit,
				FreshnessFactor:  warmupFreshnessFactor(ageDays, curve),
				Mature:           false,
			}
		}
	}
	return matureAccountWarmupStatus(mature)
}

// coldAccountWarmupStatus resolves the "no anchor yet" state. curve is
// guaranteed non-empty by AccountWarmupStageForAge's caller-side check.
func coldAccountWarmupStatus(curve []internalconfig.AccountWarmupStage) AccountWarmupStatus {
	first := curve[0]
	return AccountWarmupStatus{
		StageName:        accountWarmupColdStageName,
		DailyBudget:      first.DailyBudget,
		RPMLimit:         first.RPMLimit,
		ConcurrencyLimit: first.ConcurrencyLimit,
		FreshnessFactor:  0,
		Mature:           false,
	}
}

// matureAccountWarmupStatus resolves the "past the curve" state. DailyBudget
// is always 0 here (design §5.1: mature accounts are quota-driven, not
// capped by a fixed daily request count) and FreshnessFactor is always 1.
func matureAccountWarmupStatus(mature internalconfig.AccountMatureLimitsConfig) AccountWarmupStatus {
	return AccountWarmupStatus{
		StageName:        accountWarmupMatureStageName,
		DailyBudget:      0,
		RPMLimit:         mature.RPMLimit,
		ConcurrencyLimit: mature.ConcurrencyLimit,
		FreshnessFactor:  1,
		Mature:           true,
	}
}

// warmupFreshnessFactor computes the design D1 freshness ramp for an
// in-curve (not cold, not mature) account: 0 at the curve's very start,
// climbing linearly with age, and always strictly representable as < 1 while
// still inside the curve (a day-59-of-60 account is closer to mature than a
// day-0 account, but is not yet mature -- design §5.2: "新鲜度系数（养号中
// <1，成熟=1）"). Neither design.md nor spec.md prescribes an exact
// per-stage freshness formula beyond "monotonically increasing, <1 during
// warm-up, =1 once mature" -- a plain linear ramp across the curve's
// configured day span is the simplest choice that satisfies that contract
// without inventing per-stage numbers no source document specifies; Phase 1
// (tasks.md 1.1, the weight function) is free to replace this ramp with a
// more elaborate curve later without needing to touch this file's contract
// (AccountWarmupStatus.FreshnessFactor stays a plain float64 either way).
//
// curve is assumed non-empty (callers only reach this from an in-curve stage
// match in AccountWarmupStageForAge).
func warmupFreshnessFactor(ageDays int, curve []internalconfig.AccountWarmupStage) float64 {
	start := curve[0].MinAgeDays
	last := curve[len(curve)-1]
	end := last.MaxAgeDays
	if end == 0 {
		// Unbounded terminal stage (only valid on the last stage -- see
		// config.validateAccountWarmupCurve): there is no finite "reaches
		// mature" boundary to ramp toward, since this curve, by the config
		// author's own choice, never promotes an account to
		// AccountMatureLimitsConfig at all. Ramp across the curve's finite
		// portion instead, reaching 1 once the account enters the unbounded
		// terminal stage and staying there for any age beyond that -- the
		// terminal stage is, by construction, the most "mature-like" state
		// this curve models, even though AccountWarmupStatus.Mature stays
		// false for it (Mature specifically tracks "past the curve into
		// AccountMatureLimitsConfig", which by definition cannot happen
		// here).
		end = last.MinAgeDays
	}
	if end <= start {
		// Degenerate curve (e.g. a single stage spanning [0, unbounded)):
		// nothing to ramp across.
		return 1
	}
	fraction := float64(ageDays-start) / float64(end-start)
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	return fraction
}
