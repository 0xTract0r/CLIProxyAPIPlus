package auth

import (
	"math"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// EffectiveLimits is the read-only projection of an account's ACTUAL outbound
// rate ceilings -- the values that remain after the per-account rate_scale
// multiplier (design §8.3) has been applied on top of its current warm-up-stage
// (or mature) limits. The raw AccountWarmupStatus carries the PRE-scale stage
// numbers; these are the numbers the adaptive selector's serving gates actually
// enforce, so the management UI can show "this account may run up to N rpm / M
// concurrent" without re-deriving (and drifting from) the serving formulas. See
// AccountEffectiveLimits for the exact per-field derivation and its deliberate
// parity with the serving-side path (adaptive_selector.go).
type EffectiveLimits struct {
	// RPM is the per-minute request ceiling: scaleLimitRPM(stage rpm, scale),
	// rounded to a whole request for this integer display projection. It is the
	// pacing-FREE ceiling (stage rpm x rate_scale); a still-warming account may be
	// transiently slowed further BELOW this by quota-aware pacing at serving time
	// (surfaced separately as pacing_factor_dryrun), which is why PacingApplies is
	// exposed alongside rather than folded in here.
	RPM int

	// Burst is the token-bucket burst allowance: the mature burst allowance for a
	// mature account, else the stage's concurrency limit while warming, scaled by
	// rate_scale -- matching the selector's rateLimitParams burst branch.
	Burst int

	// Concurrency is the max concurrent in-flight ceiling: scaleLimitInt(stage
	// concurrency, scale) -- the same value hasConcurrencyHeadroom gates on.
	Concurrency int

	// DailyBudget is the scaled UTC-daily request budget: scaleLimitInt(stage
	// daily budget, scale). 0 = unbounded (a mature account, or a stage with no
	// daily cap), preserved through scaling -- the same value overDailyBudget
	// gates on.
	DailyBudget int

	// TokenDailyBudget is the scaled rolling-24h billable-token budget:
	// scaleLimitInt(resolveTokenDailyBudget(stage), scale). 0 = unbounded, the
	// same value overTokenBudget gates on.
	TokenDailyBudget int

	// PacingApplies reports whether this account is still WARMING (not mature),
	// i.e. whether quota-aware rpm pacing CAN apply to it at serving time. It is a
	// static "is warming" flag, not "pacing is currently active": mature accounts
	// are exempt from pacing entirely (rateLimitParams short-circuits on Mature).
	PacingApplies bool
}

// AccountEffectiveLimits projects the ACTUAL outbound rate ceilings the adaptive
// selector enforces for a, as of now, under cfg. It is the single source of
// truth shared with the management-view projection (buildAccountSchedulingView)
// so the frontend can display post-rate_scale limits without re-deriving them.
//
// It intentionally reuses the exact same primitives the serving path uses -- it
// resolves the raw stage via AccountWarmupStatusFor, the multiplier via
// AccountRateScale, and scales with the shared scaleLimitRPM / scaleLimitInt /
// resolveTokenDailyBudget helpers -- rather than re-implementing the math, so a
// change to any serving ceiling formula flows through here automatically (and the
// account_effective_limits_test parity assertions fail loudly if the projection
// ever diverges from what the selector actually enforces).
//
// Parity notes with the serving path (adaptive_selector.go):
//   - RPM mirrors rateLimitParams' rpm BEFORE its warming-only pacing multiplier.
//     Mature accounts are never paced, so for a mature account RPM equals
//     rateLimitParams' rpm exactly; pacing only ever LOWERS a warming account's
//     rpm, so RPM is always the >= ceiling, surfaced with PacingApplies so the
//     frontend can label it.
//   - Burst mirrors rateLimitParams' burst branch (mature burst vs warming stage
//     concurrency) and scaling. It does NOT re-apply rateLimitParams' final
//     "burst < 1 -> 1" token-bucket-safety floor, so a (non-default) mature config
//     with Burst 0 reports 0 here where serving would clamp to 1; with any
//     positive configured burst the two are identical.
//   - Concurrency / DailyBudget / TokenDailyBudget mirror the scaled values
//     hasConcurrencyHeadroom / overDailyBudget / overTokenBudget respectively gate
//     on, including the 0 = unbounded semantics preserved through scaling.
func AccountEffectiveLimits(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) EffectiveLimits {
	status := AccountWarmupStatusFor(a, now, cfg)
	scale := AccountRateScale(a, cfg)

	// Burst input branch mirrors rateLimitParams: the mature burst allowance for a
	// mature account, otherwise the (tight) warming stage concurrency limit.
	burstBase := status.ConcurrencyLimit
	if status.Mature {
		burstBase = cfg.MatureLimits.Burst
	}

	return EffectiveLimits{
		RPM:              scaleLimitRPMInt(status.RPMLimit, scale),
		Burst:            scaleLimitInt(burstBase, scale),
		Concurrency:      scaleLimitInt(status.ConcurrencyLimit, scale),
		DailyBudget:      scaleLimitInt(status.DailyBudget, scale),
		TokenDailyBudget: scaleLimitInt(resolveTokenDailyBudget(cfg, status), scale),
		PacingApplies:    !status.Mature,
	}
}

// scaleLimitRPMInt applies scaleLimitRPM and rounds the resulting (possibly
// fractional) rpm ceiling to a whole request for the integer EffectiveLimits.RPM
// projection, matching scaleLimitInt's round-to-nearest convention. A
// non-positive rpm (no ceiling configured) is preserved unchanged, mirroring
// scaleLimitRPM's own 0/negative passthrough.
func scaleLimitRPMInt(rpm int, scale float64) int {
	scaled := scaleLimitRPM(float64(rpm), scale)
	if scaled <= 0 {
		return int(scaled)
	}
	return int(math.Round(scaled))
}
