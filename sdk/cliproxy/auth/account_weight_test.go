package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var accountWeightTestNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// almostEqual mirrors the ±1e-9 tolerance pattern already established in
// account_quota_test.go for asserting an expected value against a headroom
// derived from a non-power-of-2-friendly percentage (e.g. 1-90.0/100 is not
// bit-identical to the literal 0.1: classic IEEE 754 division/subtraction
// rounding, not a bug -- 20*(1-0.9) and 20*0.1 are genuinely different
// float64 values). Exact `==`/`!=` is only safe here when the test mirrors
// production's exact runtime arithmetic formula bit-for-bit (see the warm-up
// ratio assertions below, which do that instead of using this helper).
func almostEqual(got, want float64) bool {
	const eps = 1e-9
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < eps
}

// matureAuth builds a Claude auth fixture with the given rate_limit_tier and
// utilization%, anchored far enough in the past (well beyond the default
// curve's last stage) that AccountFreshnessWeightFactor always resolves to 1
// for it -- isolating the tier/quota axes from the freshness axis in tests
// that are not specifically about warm-up.
func matureAuth(rateLimitTier string, utilizationPercent float64) *Auth {
	return &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": rateLimitTier},
				},
				"usage": map[string]any{
					"five_hour": map[string]any{"utilization": utilizationPercent},
				},
			},
		},
	}
}

func TestAccountSelectionWeight_TierOrdering(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	max20x := AccountSelectionWeight(matureAuth("default_claude_max_20x", 0), cfg, accountWeightTestNow)
	max5x := AccountSelectionWeight(matureAuth("default_claude_max_5x", 0), cfg, accountWeightTestNow)
	pro := AccountSelectionWeight(matureAuth("default_claude_pro", 0), cfg, accountWeightTestNow)

	if !(max20x > max5x && max5x > pro) {
		t.Fatalf("expected max_20x(%v) > max_5x(%v) > pro(%v)", max20x, max5x, pro)
	}
	// With 0% utilization and a fully-mature anchor, both other factors are
	// exactly 1, so the weight should equal the raw tier base weight exactly.
	if max20x != internalconfig.DefaultAccountTierWeightClaudeMax20x {
		t.Fatalf("max_20x weight = %v, want exactly the tier base weight %v (quota/freshness factors should both be 1 here)", max20x, internalconfig.DefaultAccountTierWeightClaudeMax20x)
	}
	if max5x != internalconfig.DefaultAccountTierWeightClaudeMax5x {
		t.Fatalf("max_5x weight = %v, want exactly %v", max5x, internalconfig.DefaultAccountTierWeightClaudeMax5x)
	}
	if pro != internalconfig.DefaultAccountTierWeightClaudePro {
		t.Fatalf("pro weight = %v, want exactly %v", pro, internalconfig.DefaultAccountTierWeightClaudePro)
	}
}

// TestAccountSelectionWeight_CodexScoresZero pins the §8.2 claude-only收敛
// decision: Codex accounts are dropped from adaptive scheduling (AccountTierBaseWeight
// returns 0 for provider "codex"), so every Codex account scores exactly 0 weight
// regardless of plan_type -- which makes the adaptive selector skip it and退回
// 普通轮询 (the round-robin fallback). Before §8.2 Codex pro/plus scored positive,
// ordered weights; this test would have asserted pro > plus.
func TestAccountSelectionWeight_CodexScoresZero(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	mkCodex := func(planType string) *Auth {
		return &Auth{
			Provider:   "codex",
			Attributes: map[string]string{"plan_type": planType},
			Metadata: map[string]any{
				FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
			},
		}
	}
	for _, planType := range []string{"pro", "plus", "team", ""} {
		if got := AccountSelectionWeight(mkCodex(planType), cfg, accountWeightTestNow); got != 0 {
			t.Fatalf("codex plan_type=%q weight = %v, want 0 (§8.2 claude-only收敛)", planType, got)
		}
	}
}

func TestAccountSelectionWeight_QuotaHeadroomLowersWeight(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	lowUtilization := AccountSelectionWeight(matureAuth("default_claude_max_20x", 10), cfg, accountWeightTestNow)
	highUtilization := AccountSelectionWeight(matureAuth("default_claude_max_20x", 90), cfg, accountWeightTestNow)

	if !(lowUtilization > highUtilization) {
		t.Fatalf("expected low-utilization weight(%v) > high-utilization weight(%v)", lowUtilization, highUtilization)
	}

	// Pin the numbers: base(20) x headroom(1-90/100=0.1) x freshness(1).
	// Tolerance, not exact `!=`: 1-90.0/100 computed at runtime is not
	// bit-identical to the literal 0.1 (IEEE 754 rounding on a
	// non-power-of-2 fraction; see almostEqual's doc comment).
	wantHigh := internalconfig.DefaultAccountTierWeightClaudeMax20x * 0.1
	if !almostEqual(highUtilization, wantHigh) {
		t.Fatalf("90%% utilization weight = %v, want ~%v", highUtilization, wantHigh)
	}
	wantLow := internalconfig.DefaultAccountTierWeightClaudeMax20x * 0.9
	if !almostEqual(lowUtilization, wantLow) {
		t.Fatalf("10%% utilization weight = %v, want ~%v", lowUtilization, wantLow)
	}
}

func TestAccountSelectionWeight_MultipleWindowsUseTightest(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	auth := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
				},
				"usage": map[string]any{
					"five_hour": map[string]any{"utilization": 20.0},
					"seven_day": map[string]any{"utilization": 95.0}, // binding: tightest headroom
				},
			},
		},
	}

	got := AccountSelectionWeight(auth, cfg, accountWeightTestNow)
	want := internalconfig.DefaultAccountTierWeightClaudeMax20x * 0.05 // 1 - 95/100
	if !almostEqual(got, want) {
		t.Fatalf("weight = %v, want ~%v (must bind on the tightest window, not the loosest)", got, want)
	}
}

func TestAccountSelectionWeight_WarmupFactorBelowMature(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warmingAuth := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			// 3 days old -> squarely inside the default curve's w1 stage.
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-3 * 24 * time.Hour).Format(time.RFC3339),
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
				},
				"usage": map[string]any{
					"five_hour": map[string]any{"utilization": 0.0},
				},
			},
		},
	}
	mature := matureAuth("default_claude_max_20x", 0)

	warmingWeight := AccountSelectionWeight(warmingAuth, cfg, accountWeightTestNow)
	matureWeight := AccountSelectionWeight(mature, cfg, accountWeightTestNow)

	if !(warmingWeight < matureWeight) {
		t.Fatalf("expected warming account weight(%v) < mature account weight(%v)", warmingWeight, matureWeight)
	}
	if !(warmingWeight > 0) {
		t.Fatalf("warming account weight must stay > 0 (still eligible, just deprioritized), got %v", warmingWeight)
	}

	// Pin the exact warming value AFTER the FIX 2 warm-up base clamp: a warming
	// max_20x has its tier base clamped from 20 down to the Claude Pro baseline
	// (1) because it is not yet mature, so the weight is
	// Pro(1) x quota(1) x (w1.RPMLimit=3 / mature.RPMLimit=45), NOT the old
	// unclamped max_20x(20) x (3/45).
	wantFactor := float64(internalconfig.DefaultAccountWarmupCurve()[0].RPMLimit) / float64(internalconfig.DefaultAccountMatureRPMLimit)
	want := internalconfig.DefaultAccountTierWeightClaudePro * wantFactor
	if warmingWeight != want {
		t.Fatalf("warming weight = %v, want exactly %v (clamped Pro base x w1 ratio %v)", warmingWeight, want, wantFactor)
	}
	if wantFactor >= 1 {
		t.Fatalf("test fixture invalid: w1 ratio %v should be well below 1", wantFactor)
	}
	// Negative control (self-check): were the FIX 2 clamp removed, warmingWeight
	// would be max_20x(20) x (3/45) ≈ 1.333, which exceeds the mature Pro
	// weight below -- exactly the warm-up domination bug. Assert it does not.
	maturePro := AccountSelectionWeight(matureAuth("default_claude_pro", 0), cfg, accountWeightTestNow)
	if !(warmingWeight <= maturePro) {
		t.Fatalf("warming max_20x weight(%v) must not exceed mature Pro weight(%v): the clamp failed", warmingWeight, maturePro)
	}
}

func TestAccountSelectionWeight_NilAuth(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	if got := AccountSelectionWeight(nil, cfg, accountWeightTestNow); got != 0 {
		t.Fatalf("weight for nil auth = %v, want 0", got)
	}
}

func TestAccountSelectionWeight_UnrecognizedProviderIsZero(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	auth := &Auth{Provider: "gemini", Metadata: map[string]any{
		FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
	}}
	if got := AccountSelectionWeight(auth, cfg, accountWeightTestNow); got != 0 {
		t.Fatalf("weight for unrecognized provider = %v, want 0 (never guessed nonzero)", got)
	}
}

func TestAccountSelectionWeight_ZeroConfiguredTierWeightExcludes(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.TierWeights.Claude.Unknown = 0 // operator opts to hard-exclude unrecognized tiers
	auth := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "some_future_tier"},
				},
			},
		},
	}
	if got := AccountSelectionWeight(auth, cfg, accountWeightTestNow); got != 0 {
		t.Fatalf("weight = %v, want 0 when the configured tier weight is 0", got)
	}
}

func TestAccountQuotaWeightFactor(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want float64
	}{
		{name: "nil auth falls back to unknown", auth: nil, want: unknownAccountQuotaHeadroom},
		{name: "no quota_snapshot falls back to unknown", auth: &Auth{}, want: unknownAccountQuotaHeadroom},
		{
			name: "0% utilization -> full headroom",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{"usage": map[string]any{
					"five_hour": map[string]any{"utilization": 0.0},
				}},
			}},
			want: 1,
		},
		{
			name: "100% utilization -> zero headroom",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{"usage": map[string]any{
					"five_hour": map[string]any{"utilization": 100.0},
				}},
			}},
			want: 0,
		},
		{
			name: "45% utilization -> 0.55 headroom",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{"usage": map[string]any{
					"five_hour": map[string]any{"utilization": 45.0},
				}},
			}},
			want: 0.55,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Tolerance: some cases (e.g. 45% utilization -> 0.55) route
			// through a runtime 1-utilization/100 division that is not
			// bit-identical to a hand-typed decimal literal (see
			// almostEqual's doc comment); 0/1/unknown-fallback cases are
			// exact regardless, so the tolerance costs nothing there.
			if got := AccountQuotaWeightFactor(tt.auth); !almostEqual(got, tt.want) {
				t.Fatalf("AccountQuotaWeightFactor() = %v, want ~%v", got, tt.want)
			}
		})
	}
}

func TestAccountFreshnessWeightFactor(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	t.Run("no anchor recorded resolves to the cold first-stage factor, not full 1.0", func(t *testing.T) {
		// FIX 1 (no-anchor bootstrap): a credential with no first_production_at
		// anchor is design §5.1's "cold" state. It must be weighted at the
		// curve's FIRST stage's factor (matching coldAccountWarmupStatus), NOT
		// the old full-1.0 bootstrap that let a fresh high-tier account dominate.
		got := AccountFreshnessWeightFactor(&Auth{}, cfg, accountWeightTestNow)
		want := float64(internalconfig.DefaultAccountWarmupCurve()[0].RPMLimit) / float64(internalconfig.DefaultAccountMatureRPMLimit)
		if got != want {
			t.Fatalf("factor = %v, want %v (cold curve[0] factor 3/45)", got, want)
		}
		// Negative control (self-check): if the no-anchor fix were reverted to
		// `return 1`, `got` would be 1 and this bound would fail -- i.e. commenting
		// out FIX 1 turns this assertion red.
		if !(got > 0 && got < 1) {
			t.Fatalf("cold factor = %v, want strictly in (0,1): >0 so a fresh account is a trickle candidate not starved, <1 so it cannot dominate the pool", got)
		}
	})

	t.Run("no anchor with empty curve stays mature (1)", func(t *testing.T) {
		// An empty curve means warm-up throttling is disabled entirely -- there
		// is no first stage to be "cold" relative to, so a no-anchor account
		// falls through to mature, consistent with the anchored empty-curve path.
		emptyCfg := cfg
		emptyCfg.WarmupCurve = nil
		got := AccountFreshnessWeightFactor(&Auth{}, emptyCfg, accountWeightTestNow)
		if got != 1 {
			t.Fatalf("factor = %v, want 1 (empty curve disables warm-up throttling even for a no-anchor account)", got)
		}
	})

	t.Run("age beyond every stage is mature", func(t *testing.T) {
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		}}
		got := AccountFreshnessWeightFactor(auth, cfg, accountWeightTestNow)
		if got != 1 {
			t.Fatalf("factor = %v, want 1 (90d is past the default curve's 60d ceiling)", got)
		}
	})

	t.Run("exact boundary rolls into the next stage, not the previous one", func(t *testing.T) {
		// Default curve: w1=[0,7) RPM=3, w2=[7,14) RPM=5. Age exactly 7 must
		// land in w2, not w1 (MinAgeDays inclusive, MaxAgeDays exclusive).
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		}}
		got := AccountFreshnessWeightFactor(auth, cfg, accountWeightTestNow)
		want := float64(internalconfig.DefaultAccountWarmupCurve()[1].RPMLimit) / float64(internalconfig.DefaultAccountMatureRPMLimit)
		if got != want {
			t.Fatalf("factor at age=7 = %v, want %v (w2's ratio)", got, want)
		}
	})

	t.Run("empty curve is always mature", func(t *testing.T) {
		emptyCfg := cfg
		emptyCfg.WarmupCurve = nil
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Format(time.RFC3339), // age 0
		}}
		got := AccountFreshnessWeightFactor(auth, emptyCfg, accountWeightTestNow)
		if got != 1 {
			t.Fatalf("factor = %v, want 1 (empty curve disables warm-up throttling)", got)
		}
	})

	t.Run("defensive: non-positive mature rpm limit never divides by zero or goes negative", func(t *testing.T) {
		badCfg := cfg
		badCfg.MatureLimits.RPMLimit = 0
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWeightTestNow.Add(-3 * 24 * time.Hour).Format(time.RFC3339),
		}}
		got := AccountFreshnessWeightFactor(auth, badCfg, accountWeightTestNow)
		if got != 1 {
			t.Fatalf("factor = %v, want 1 (defensive fallback for unvalidated config)", got)
		}
	})
}

func TestAccountIsMature(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	if !AccountIsMature(&Auth{}, cfg, accountWeightTestNow) {
		t.Fatalf("no-anchor account should be classified mature")
	}

	warming := &Auth{Metadata: map[string]any{
		FirstProductionAtMetadataKey: accountWeightTestNow.Add(-3 * 24 * time.Hour).Format(time.RFC3339),
	}}
	if AccountIsMature(warming, cfg, accountWeightTestNow) {
		t.Fatalf("3-day-old account (inside w1) should not be classified mature")
	}

	mature := &Auth{Metadata: map[string]any{
		FirstProductionAtMetadataKey: accountWeightTestNow.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
	}}
	if !AccountIsMature(mature, cfg, accountWeightTestNow) {
		t.Fatalf("200-day-old account should be classified mature")
	}
}

func TestCurrentAccountWarmupStage(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()

	tests := []struct {
		name      string
		ageDays   int
		wantName  string
		wantFound bool
	}{
		{name: "age 0 -> w1", ageDays: 0, wantName: "w1", wantFound: true},
		{name: "age 6 -> w1", ageDays: 6, wantName: "w1", wantFound: true},
		{name: "age 7 -> w2 (boundary, not w1)", ageDays: 7, wantName: "w2", wantFound: true},
		{name: "age 13 -> w2", ageDays: 13, wantName: "w2", wantFound: true},
		{name: "age 14 -> w3-4", ageDays: 14, wantName: "w3-4", wantFound: true},
		{name: "age 59 -> w7-8", ageDays: 59, wantName: "w7-8", wantFound: true},
		{name: "age 60 -> beyond curve (mature)", ageDays: 60, wantFound: false},
		{name: "age 1000 -> beyond curve (mature)", ageDays: 1000, wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stage, found := currentAccountWarmupStage(curve, tt.ageDays)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if tt.wantFound && stage.Name != tt.wantName {
				t.Fatalf("stage.Name = %q, want %q", stage.Name, tt.wantName)
			}
		})
	}

	t.Run("empty curve never matches", func(t *testing.T) {
		_, found := currentAccountWarmupStage(nil, 0)
		if found {
			t.Fatalf("found = true for an empty curve, want false")
		}
	})

	t.Run("unbounded final stage matches any age at or above its floor", func(t *testing.T) {
		unbounded := []internalconfig.AccountWarmupStage{
			{Name: "only", MinAgeDays: 0, MaxAgeDays: 0, RPMLimit: 1, ConcurrencyLimit: 1},
		}
		stage, found := currentAccountWarmupStage(unbounded, 99999)
		if !found || stage.Name != "only" {
			t.Fatalf("expected the single unbounded stage to match a very large age, got stage=%v found=%v", stage, found)
		}
	})
}

// warmingClaudeAuth builds a Claude auth fixture pinned to tierOverride
// ("max_20x"/"max_5x"/"pro") -- mirroring how the real production test accounts
// declare their tier, since their upstream rate_limit_tier ("default_claude_ai")
// is intentionally left unmapped by claudeRateLimitTierValues. ageDays < 0 omits
// the first_production_at anchor entirely (design §5.1's no-anchor "cold" state);
// ageDays >= 0 anchors the account that many days before accountWeightTestNow.
// utilizationPercent drives the five_hour quota window.
func warmingClaudeAuth(tierOverride string, ageDays int, utilizationPercent float64) *Auth {
	meta := map[string]any{
		TierOverrideMetadataKey: tierOverride,
		"quota_snapshot": map[string]any{
			"usage": map[string]any{
				"five_hour": map[string]any{"utilization": utilizationPercent},
			},
		},
	}
	if ageDays >= 0 {
		meta[FirstProductionAtMetadataKey] = accountWeightTestNow.Add(-time.Duration(ageDays) * 24 * time.Hour).Format(time.RFC3339)
	}
	return &Auth{Provider: "claude", Metadata: meta}
}

// TestAccountSelectionWeight_WarmingHighTierNotAboveWarmingPro is the core FIX 2
// invariant: a still-warming max_20x account must never out-weigh a warming Pro
// account of the SAME age and quota, because its tier base is clamped down to
// the Pro baseline during warm-up. Covers the no-anchor cold state and several
// in-curve ages.
func TestAccountSelectionWeight_WarmingHighTierNotAboveWarmingPro(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	for _, ageDays := range []int{-1 /* no anchor / cold */, 0 /* w1 day 0 */, 3 /* w1 */, 10 /* w2 */, 20 /* w3-4 */} {
		max20x := AccountSelectionWeight(warmingClaudeAuth("max_20x", ageDays, 25), cfg, accountWeightTestNow)
		pro := AccountSelectionWeight(warmingClaudeAuth("pro", ageDays, 25), cfg, accountWeightTestNow)
		if !(max20x <= pro) {
			// Negative control (self-check): without the FIX 2 base clamp, max20x would be
			// base(20) x ... and pro base(1) x ..., so max20x > pro and this
			// fails -- i.e. commenting out the clamp turns this red.
			t.Fatalf("ageDays=%d: warming max_20x weight(%v) must be <= warming Pro weight(%v) at same age/quota", ageDays, max20x, pro)
		}
		if !(max20x > 0) {
			t.Fatalf("ageDays=%d: warming max_20x weight must stay > 0 (deprioritized, not excluded), got %v", ageDays, max20x)
		}
	}
}

// TestAccountSelectionWeight_MatureTierDistributionUnchanged proves the fixes
// have ZERO effect on mature accounts: the clamp is skipped (Mature==true) and
// freshness is 1, so mature weights are exactly the tier base weights and the
// max_20x:5x:pro distribution is precisely 20:5:1, identical to pre-fix.
func TestAccountSelectionWeight_MatureTierDistributionUnchanged(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	max20x := AccountSelectionWeight(matureAuth("default_claude_max_20x", 0), cfg, accountWeightTestNow)
	max5x := AccountSelectionWeight(matureAuth("default_claude_max_5x", 0), cfg, accountWeightTestNow)
	pro := AccountSelectionWeight(matureAuth("default_claude_pro", 0), cfg, accountWeightTestNow)

	// Absolute weights are exactly the tier base weights (quota=1, freshness=1).
	if max20x != internalconfig.DefaultAccountTierWeightClaudeMax20x {
		t.Fatalf("mature max_20x weight = %v, want exactly %v (clamp must NOT touch mature accounts)", max20x, internalconfig.DefaultAccountTierWeightClaudeMax20x)
	}
	if max5x != internalconfig.DefaultAccountTierWeightClaudeMax5x {
		t.Fatalf("mature max_5x weight = %v, want exactly %v", max5x, internalconfig.DefaultAccountTierWeightClaudeMax5x)
	}
	if pro != internalconfig.DefaultAccountTierWeightClaudePro {
		t.Fatalf("mature pro weight = %v, want exactly %v", pro, internalconfig.DefaultAccountTierWeightClaudePro)
	}
	// Ratio pins (base weights are integer-valued, so these divisions are exact).
	// Negative control (self-check): an over-broad clamp that fired on mature accounts would
	// pull max_20x down to 1 and collapse this 20:5:1 ratio, failing here.
	if max20x/pro != 20.0 {
		t.Fatalf("mature max_20x/pro ratio = %v, want exactly 20 (distribution drifted)", max20x/pro)
	}
	if max5x/pro != 5.0 {
		t.Fatalf("mature max_5x/pro ratio = %v, want exactly 5 (distribution drifted)", max5x/pro)
	}
}

// TestAccountSelectionWeight_NoAnchorMax20xDoesNotDominatePool reproduces the
// production AC-14 bug end to end: a brand-new no-anchor account pinned max_20x
// via tier_override, dropped into a pool of mature accounts. Pre-fix, the full
// 1.0 no-anchor freshness bootstrap (FIX 1) x the un-clamped base 20 (FIX 2)
// gave it ~74% of the weighted selection share (the observed ~77%). Both fixes
// together reduce it to a low-weight trickle candidate.
func TestAccountSelectionWeight_NoAnchorMax20xDoesNotDominatePool(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	fresh := warmingClaudeAuth("max_20x", -1 /* no anchor: the exact AC-14 shape */, 0)
	pool := []*Auth{
		fresh,
		matureAuth("default_claude_pro", 0),
		matureAuth("default_claude_pro", 0),
		matureAuth("default_claude_max_5x", 0),
	}
	var total, freshWeight float64
	for _, a := range pool {
		w := AccountSelectionWeight(a, cfg, accountWeightTestNow)
		total += w
		if a == fresh {
			freshWeight = w
		}
	}
	if total <= 0 {
		t.Fatalf("pool total weight = %v, want > 0", total)
	}
	share := freshWeight / total
	// Post-fix share is ~0.9%. A generous 10% ceiling cleanly separates the fixed
	// behavior from the ~74% pre-fix domination. Negative control (self-check): reverting
	// EITHER fix -- no-anchor freshness back to 1, OR removing the base clamp so
	// base stays 20 -- pushes this share back above 0.10 and turns the assertion
	// red.
	if share >= 0.10 {
		t.Fatalf("no-anchor max_20x selection share = %.4f, want < 0.10 (pre-fix ~0.74 domination bug)", share)
	}
	// Still strictly positive so the fresh account can eventually win a pick and
	// mint its first-production anchor (never starved to 0).
	if !(freshWeight > 0) {
		t.Fatalf("fresh account weight must stay > 0, got %v", freshWeight)
	}
}
