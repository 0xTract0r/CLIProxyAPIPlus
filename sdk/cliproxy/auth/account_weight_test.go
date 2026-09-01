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

func TestAccountSelectionWeight_CodexTierOrdering(t *testing.T) {
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
	pro := AccountSelectionWeight(mkCodex("pro"), cfg, accountWeightTestNow)
	plus := AccountSelectionWeight(mkCodex("plus"), cfg, accountWeightTestNow)
	if !(pro > plus) {
		t.Fatalf("expected codex pro(%v) > plus(%v)", pro, plus)
	}
	// No quota_snapshot at all for Codex here -> quota factor falls back to
	// the documented unknown headroom, not 1.0.
	wantPro := internalconfig.DefaultAccountTierWeightCodexPro * unknownAccountQuotaHeadroom
	if pro != wantPro {
		t.Fatalf("codex pro weight = %v, want %v (base x unknown-quota fallback x mature freshness)", pro, wantPro)
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

	// Pin the exact w1 ratio: base(20) x quota(1) x (w1.RPMLimit=3 / mature.RPMLimit=45).
	wantFactor := float64(internalconfig.DefaultAccountWarmupCurve()[0].RPMLimit) / float64(internalconfig.DefaultAccountMatureRPMLimit)
	want := internalconfig.DefaultAccountTierWeightClaudeMax20x * wantFactor
	if warmingWeight != want {
		t.Fatalf("warming weight = %v, want exactly %v (w1 ratio %v)", warmingWeight, want, wantFactor)
	}
	if wantFactor >= 1 {
		t.Fatalf("test fixture invalid: w1 ratio %v should be well below 1", wantFactor)
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

	t.Run("no anchor recorded is treated as mature", func(t *testing.T) {
		got := AccountFreshnessWeightFactor(&Auth{}, cfg, accountWeightTestNow)
		if got != 1 {
			t.Fatalf("factor = %v, want 1 (no-anchor accounts must not be perpetually throttled)", got)
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
