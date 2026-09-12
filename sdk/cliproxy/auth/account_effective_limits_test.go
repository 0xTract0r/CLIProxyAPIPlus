package auth

import (
	"math"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// effWithScale stamps a per-account rate_scale override onto an adaptive test
// auth, mirroring the withScale helper in account_rate_scale_test.go.
func effWithScale(a *Auth, v float64) *Auth {
	a.Metadata[AccountSchedulingMetadataKey] = map[string]any{accountSchedulingRateScaleKey: v}
	return a
}

// TestAccountEffectiveLimits_MatureParity locks AccountEffectiveLimits against the
// serving path for MATURE accounts: because mature accounts are never paced,
// rateLimitParams' rpm/burst are the exact effective ceilings, so RPM/Burst must
// equal them byte-for-byte. Concurrency/DailyBudget/TokenDailyBudget must equal the
// same scaled expressions hasConcurrencyHeadroom / overDailyBudget / overTokenBudget
// gate on. Covers rate_scale=1 (no-op) and rate_scale=4 (>1 lift). If any serving
// scaling formula changes without this projection following, these assertions fail.
func TestAccountEffectiveLimits_MatureParity(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg}, WithAdaptiveClock(fixedClock()))
	defer s.Stop()

	cases := []struct {
		name  string
		scale float64
	}{
		{name: "rate_scale=1", scale: 1},
		{name: "rate_scale=4", scale: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdaptiveClaudeAuth("eff-mature", "default_claude_max_20x", matureFirstProd())
			effWithScale(a, tc.scale)

			status := AccountWarmupStatusFor(a, adaptiveTestNow, cfg)
			if !status.Mature {
				t.Fatalf("test account is not mature; got stage %q", status.StageName)
			}
			scale := AccountRateScale(a, cfg)
			eff := AccountEffectiveLimits(a, adaptiveTestNow, cfg)

			// rpm/burst must match the selector's real derivation exactly (no pacing
			// on a mature account, so the ceiling IS the served value).
			servingRPM, servingBurst := s.rateLimitParams(a, cfg, adaptiveTestNow)
			if float64(eff.RPM) != servingRPM {
				t.Fatalf("RPM = %d, want rateLimitParams rpm %v (mature has no pacing)", eff.RPM, servingRPM)
			}
			if eff.Burst != servingBurst {
				t.Fatalf("Burst = %d, want rateLimitParams burst %d", eff.Burst, servingBurst)
			}

			// concurrency must equal the exact value hasConcurrencyHeadroom scales to.
			if want := scaleLimitInt(status.ConcurrencyLimit, scale); eff.Concurrency != want {
				t.Fatalf("Concurrency = %d, want scaleLimitInt(%d, %v) = %d", eff.Concurrency, status.ConcurrencyLimit, scale, want)
			}
			// mature daily budget is unbounded (0), preserved through scaling, matching
			// overDailyBudget's mature short-circuit (DailyBudget <= 0 -> never over).
			if want := scaleLimitInt(status.DailyBudget, scale); eff.DailyBudget != want || eff.DailyBudget != 0 {
				t.Fatalf("DailyBudget = %d, want scaleLimitInt(%d, %v) = %d (0 = unbounded)", eff.DailyBudget, status.DailyBudget, scale, want)
			}
			// mature token budget defaults to unbounded (0), matching overTokenBudget.
			if want := scaleLimitInt(resolveTokenDailyBudget(cfg, status), scale); eff.TokenDailyBudget != want {
				t.Fatalf("TokenDailyBudget = %d, want scaleLimitInt(resolveTokenDailyBudget, %v) = %d", eff.TokenDailyBudget, scale, want)
			}
			if eff.PacingApplies {
				t.Fatalf("PacingApplies = true, want false for a mature account")
			}
		})
	}
}

// TestAccountEffectiveLimits_WarmingParity locks the projection for WARMING
// accounts against the serving path. With no persisted burn history the pacing
// multiplier is 1, so the pacing-free RPM ceiling equals rateLimitParams' rpm; in
// general it is the >= ceiling (pacing only lowers). Concurrency/DailyBudget/
// TokenDailyBudget must equal the same scaled expressions the selector's warm-up
// gates use. Covers rate_scale=1 and a fractional rate_scale that exercises the
// scaleLimitInt floor-1 boundary. PacingApplies must be true (still warming).
func TestAccountEffectiveLimits_WarmingParity(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg}, WithAdaptiveClock(fixedClock()))
	defer s.Stop()

	cases := []struct {
		name  string
		scale float64
		// wantConcurrency asserts the scaleLimitInt floor-1 boundary explicitly.
		wantConcurrency int
	}{
		// w1 (age 2d): rpm 3, concurrency 1, daily 200. scale 1 is a no-op.
		{name: "rate_scale=1", scale: 1, wantConcurrency: 1},
		// scale 0.5: concurrency 1 -> round(0.5)=0 -> floored to 1, not dropped to 0.
		{name: "fractional rate_scale floors concurrency at 1", scale: 0.5, wantConcurrency: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdaptiveClaudeAuth("eff-warming", "default_claude_max_20x", warmupFirstProd())
			effWithScale(a, tc.scale)

			status := AccountWarmupStatusFor(a, adaptiveTestNow, cfg)
			if status.Mature {
				t.Fatalf("test account unexpectedly mature; want a warming stage")
			}
			scale := AccountRateScale(a, cfg)
			eff := AccountEffectiveLimits(a, adaptiveTestNow, cfg)

			// RPM is the pacing-free ceiling: it must equal the shared primitive and
			// be >= the selector's (possibly paced) served rpm. Here there is no burn
			// history so pacing == 1 and the two coincide.
			if want := scaleLimitRPMInt(status.RPMLimit, scale); eff.RPM != want {
				t.Fatalf("RPM = %d, want scaleLimitRPMInt(%d, %v) = %d", eff.RPM, status.RPMLimit, scale, want)
			}
			servingRPM, servingBurst := s.rateLimitParams(a, cfg, adaptiveTestNow)
			if float64(eff.RPM) < servingRPM {
				t.Fatalf("RPM ceiling %d < served rpm %v; projection must be the >= ceiling", eff.RPM, servingRPM)
			}
			if eff.Burst != servingBurst {
				t.Fatalf("Burst = %d, want rateLimitParams burst %d", eff.Burst, servingBurst)
			}

			// Concurrency must equal the exact scaled value hasConcurrencyHeadroom
			// gates on, including the floor-1 boundary asserted per-case.
			wantConc := scaleLimitInt(status.ConcurrencyLimit, scale)
			if eff.Concurrency != wantConc || eff.Concurrency != tc.wantConcurrency {
				t.Fatalf("Concurrency = %d, want scaleLimitInt(%d, %v) = %d (expected %d)", eff.Concurrency, status.ConcurrencyLimit, scale, wantConc, tc.wantConcurrency)
			}
			// DailyBudget must equal the exact scaled value overDailyBudget gates on.
			if want := scaleLimitInt(status.DailyBudget, scale); eff.DailyBudget != want {
				t.Fatalf("DailyBudget = %d, want scaleLimitInt(%d, %v) = %d", eff.DailyBudget, status.DailyBudget, scale, want)
			}
			// TokenDailyBudget must equal the exact scaled value overTokenBudget gates
			// on (0 = unbounded on the default curve, preserved through scaling).
			if want := scaleLimitInt(resolveTokenDailyBudget(cfg, status), scale); eff.TokenDailyBudget != want {
				t.Fatalf("TokenDailyBudget = %d, want scaleLimitInt(resolveTokenDailyBudget, %v) = %d", eff.TokenDailyBudget, scale, want)
			}
			if !eff.PacingApplies {
				t.Fatalf("PacingApplies = false, want true for a warming account")
			}
		})
	}
}

// TestAccountEffectiveLimits_WarmingTokenBudgetScaled exercises a NON-zero per-stage
// token daily budget (the default curve leaves it 0 = unbounded), proving the
// projected token budget is scaled by the exact same scaleLimitInt(resolveTokenDailyBudget)
// expression overTokenBudget gates on, and preserves the floor-1 boundary.
func TestAccountEffectiveLimits_WarmingTokenBudgetScaled(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	// Copy the curve slice before mutating so the shared default is not modified.
	curve := append([]internalconfig.AccountWarmupStage(nil), cfg.WarmupCurve...)
	curve[0].TokenDailyBudget = 1000 // w1 stage
	cfg.WarmupCurve = curve

	a := newAdaptiveClaudeAuth("eff-warming-token", "default_claude_max_20x", warmupFirstProd())
	effWithScale(a, 0.5)

	status := AccountWarmupStatusFor(a, adaptiveTestNow, cfg)
	if status.Mature || status.StageName != "w1" {
		t.Fatalf("want warming w1 stage, got mature=%v stage=%q", status.Mature, status.StageName)
	}
	scale := AccountRateScale(a, cfg)
	eff := AccountEffectiveLimits(a, adaptiveTestNow, cfg)

	// resolveTokenDailyBudget is the SAME free function overTokenBudget resolves the
	// budget through; asserting against it proves the projection cannot drift.
	want := scaleLimitInt(resolveTokenDailyBudget(cfg, status), scale) // scaleLimitInt(1000, 0.5) = 500
	if want != 500 {
		t.Fatalf("test setup: scaled token budget = %d, want 500", want)
	}
	if eff.TokenDailyBudget != want {
		t.Fatalf("TokenDailyBudget = %d, want %d", eff.TokenDailyBudget, want)
	}
}

// TestScaleLimitRPMInt covers the integer-rounding rpm projection helper: it rounds
// scaleLimitRPM's (possibly fractional) result to the nearest whole request and
// preserves a non-positive (no-ceiling) rpm unchanged.
func TestScaleLimitRPMInt(t *testing.T) {
	tests := []struct {
		rpm   int
		scale float64
		want  int
	}{
		{rpm: 0, scale: 0.5, want: 0},   // no ceiling preserved
		{rpm: 45, scale: 1.0, want: 45}, // no-op
		{rpm: 45, scale: 4.0, want: 180},
		{rpm: 45, scale: 0.5, want: 23}, // round(22.5) = 23 (round half away from zero)
		{rpm: 3, scale: 0.5, want: 2},   // round(1.5) = 2
		{rpm: 3, scale: 1.0, want: 3},
		{rpm: 10, scale: 2.0, want: 20},
	}
	for _, tc := range tests {
		if got := scaleLimitRPMInt(tc.rpm, tc.scale); got != tc.want {
			t.Fatalf("scaleLimitRPMInt(%d, %v) = %d, want %d", tc.rpm, tc.scale, got, tc.want)
		}
	}
	// Sanity: the helper agrees with math.Round(scaleLimitRPM(...)) for a positive rpm.
	if got, want := scaleLimitRPMInt(7, 0.3), int(math.Round(scaleLimitRPM(7, 0.3))); got != want {
		t.Fatalf("scaleLimitRPMInt(7, 0.3) = %d, want %d", got, want)
	}
}
