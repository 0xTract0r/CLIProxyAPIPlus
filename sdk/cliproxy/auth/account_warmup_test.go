package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var accountWarmupTestAnchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestAccountWarmupStageForAge_DefaultCurveBoundaries walks every stage
// boundary of the design §5.1 default curve and asserts the resolved limits
// land in exactly the expected stage -- MinAgeDays inclusive, MaxAgeDays
// exclusive.
func TestAccountWarmupStageForAge_DefaultCurveBoundaries(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	mature := internalconfig.DefaultAccountMatureLimits()

	tests := []struct {
		name             string
		ageDays          int
		wantStage        string
		wantDailyBudget  int
		wantRPMLimit     int
		wantConcurrency  int
		wantMature       bool
	}{
		{"day 0 -> w1 start", 0, "w1", 200, 3, 1, false},
		{"day 6 -> still w1", 6, "w1", 200, 3, 1, false},
		{"day 7 -> w2 start (w1 max exclusive)", 7, "w2", 500, 5, 1, false},
		{"day 13 -> still w2", 13, "w2", 500, 5, 1, false},
		{"day 14 -> w3-4 start", 14, "w3-4", 2000, 12, 2, false},
		{"day 29 -> still w3-4", 29, "w3-4", 2000, 12, 2, false},
		{"day 30 -> w5-6 start", 30, "w5-6", 4500, 20, 2, false},
		{"day 44 -> still w5-6", 44, "w5-6", 4500, 20, 2, false},
		{"day 45 -> w7-8 start", 45, "w7-8", 6500, 30, 3, false},
		{"day 59 -> still w7-8", 59, "w7-8", 6500, 30, 3, false},
		{"day 60 -> mature (w7-8 max exclusive)", 60, "mature", 0, mature.RPMLimit, mature.ConcurrencyLimit, true},
		{"day 61 -> still mature", 61, "mature", 0, mature.RPMLimit, mature.ConcurrencyLimit, true},
		{"day 1000 -> still mature", 1000, "mature", 0, mature.RPMLimit, mature.ConcurrencyLimit, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AccountWarmupStageForAge(tc.ageDays, true, curve, mature)
			if got.StageName != tc.wantStage {
				t.Fatalf("StageName = %q, want %q", got.StageName, tc.wantStage)
			}
			if got.DailyBudget != tc.wantDailyBudget {
				t.Fatalf("DailyBudget = %d, want %d", got.DailyBudget, tc.wantDailyBudget)
			}
			if got.RPMLimit != tc.wantRPMLimit {
				t.Fatalf("RPMLimit = %d, want %d", got.RPMLimit, tc.wantRPMLimit)
			}
			if got.ConcurrencyLimit != tc.wantConcurrency {
				t.Fatalf("ConcurrencyLimit = %d, want %d", got.ConcurrencyLimit, tc.wantConcurrency)
			}
			if got.Mature != tc.wantMature {
				t.Fatalf("Mature = %v, want %v", got.Mature, tc.wantMature)
			}
			if tc.wantMature {
				if got.FreshnessFactor != 1 {
					t.Fatalf("FreshnessFactor = %v, want 1 (mature)", got.FreshnessFactor)
				}
			} else if got.FreshnessFactor >= 1 {
				t.Fatalf("FreshnessFactor = %v, want < 1 while still in warm-up curve", got.FreshnessFactor)
			}
		})
	}
}

// TestAccountWarmupStageForAge_Cold covers design §5.1's "冷置" state: no
// first_production_at anchor recorded yet. It must resolve to the curve's
// first (most restrictive) configured stage's limits with FreshnessFactor
// pinned to 0, regardless of whatever ageDays value happens to be passed
// (ageDays is meaningless without an anchor and MUST be ignored).
func TestAccountWarmupStageForAge_Cold(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	mature := internalconfig.DefaultAccountMatureLimits()

	for _, ignoredAge := range []int{0, 5, 30, 9999, -1} {
		got := AccountWarmupStageForAge(ignoredAge, false, curve, mature)
		if got.StageName != accountWarmupColdStageName {
			t.Fatalf("ageDays=%d: StageName = %q, want %q", ignoredAge, got.StageName, accountWarmupColdStageName)
		}
		if got.Mature {
			t.Fatalf("ageDays=%d: Mature = true, want false (cold is never mature)", ignoredAge)
		}
		if got.FreshnessFactor != 0 {
			t.Fatalf("ageDays=%d: FreshnessFactor = %v, want 0", ignoredAge, got.FreshnessFactor)
		}
		// Cold limits must match the curve's first stage exactly (curve[0]
		// is guaranteed MinAgeDays==0 by Validate) -- i.e. at least as
		// restrictive as day 0 of the curve, never more lenient.
		first := curve[0]
		if got.DailyBudget != first.DailyBudget || got.RPMLimit != first.RPMLimit || got.ConcurrencyLimit != first.ConcurrencyLimit {
			t.Fatalf("ageDays=%d: limits = {%d,%d,%d}, want first-stage limits {%d,%d,%d}",
				ignoredAge, got.DailyBudget, got.RPMLimit, got.ConcurrencyLimit,
				first.DailyBudget, first.RPMLimit, first.ConcurrencyLimit)
		}
	}
}

// TestAccountWarmupStageForAge_MatureHasNoDailyBudget locks down the
// tasks.md 3.1 acceptance line ">60d 成熟无日预算/系数1": a mature account's
// DailyBudget is always 0 and FreshnessFactor is always exactly 1, while its
// rpm/concurrency ceiling comes from config.AccountMatureLimitsConfig, not
// from any warm-up stage.
func TestAccountWarmupStageForAge_MatureHasNoDailyBudget(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	mature := internalconfig.AccountMatureLimitsConfig{RPMLimit: 45, Burst: 10, ConcurrencyLimit: 4}

	got := AccountWarmupStageForAge(90, true, curve, mature)
	if !got.Mature {
		t.Fatalf("Mature = false, want true")
	}
	if got.DailyBudget != 0 {
		t.Fatalf("DailyBudget = %d, want 0 (mature is quota-driven, not daily-budget-capped)", got.DailyBudget)
	}
	if got.FreshnessFactor != 1 {
		t.Fatalf("FreshnessFactor = %v, want 1", got.FreshnessFactor)
	}
	if got.RPMLimit != 45 {
		t.Fatalf("RPMLimit = %d, want 45 (from mature-limits config, not warmup-curve)", got.RPMLimit)
	}
	if got.ConcurrencyLimit != 4 {
		t.Fatalf("ConcurrencyLimit = %d, want 4 (from mature-limits config)", got.ConcurrencyLimit)
	}
}

// TestAccountWarmupStageForAge_EmptyCurve covers the caller-constructed
// zero-value edge case (an AccountSchedulingConfig that never went through
// DefaultAccountSchedulingConfig/config-load defaulting): with no curve to
// be "cold" or "in-stage" relative to, every account -- anchored or not --
// resolves straight to the mature ceiling.
func TestAccountWarmupStageForAge_EmptyCurve(t *testing.T) {
	mature := internalconfig.AccountMatureLimitsConfig{RPMLimit: 45, Burst: 10, ConcurrencyLimit: 4}

	for _, tc := range []struct {
		name      string
		ageDays   int
		hasAnchor bool
	}{
		{"anchored, young", 0, true},
		{"anchored, old", 500, true},
		{"cold (no anchor)", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AccountWarmupStageForAge(tc.ageDays, tc.hasAnchor, nil, mature)
			if !got.Mature {
				t.Fatalf("Mature = false, want true (empty curve => trivially mature)")
			}
			if got.StageName != accountWarmupMatureStageName {
				t.Fatalf("StageName = %q, want %q", got.StageName, accountWarmupMatureStageName)
			}
			if got.FreshnessFactor != 1 {
				t.Fatalf("FreshnessFactor = %v, want 1", got.FreshnessFactor)
			}
			if got.DailyBudget != 0 || got.RPMLimit != 45 || got.ConcurrencyLimit != 4 {
				t.Fatalf("limits = {%d,%d,%d}, want {0,45,4}", got.DailyBudget, got.RPMLimit, got.ConcurrencyLimit)
			}
		})
	}
}

// TestAccountWarmupStageForAge_NegativeAgeClamped mirrors AccountAge's own
// clock-skew clamp (account_freshness.go): a negative ageDays (which
// AccountAgeDays itself can never actually produce, but a caller bypassing
// that wrapper could pass directly) must not be treated as "younger than the
// curve" and fall through to mature -- it is clamped to 0 and resolved into
// the first stage, exactly like a genuine day-0 account.
func TestAccountWarmupStageForAge_NegativeAgeClamped(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	mature := internalconfig.DefaultAccountMatureLimits()

	got := AccountWarmupStageForAge(-5, true, curve, mature)
	want := AccountWarmupStageForAge(0, true, curve, mature)
	if got != want {
		t.Fatalf("AccountWarmupStageForAge(-5, ...) = %+v, want same as age 0 = %+v", got, want)
	}
}

// TestAccountWarmupStageForAge_MalformedCurveGapFallsThroughToMature
// documents the defensive fallback for a curve that reached this function
// without passing AccountSchedulingConfig.Validate() first (e.g. a gap
// before the first stage) -- it must not panic or misattribute a stage; the
// safe fallback is mature.
func TestAccountWarmupStageForAge_MalformedCurveGapFallsThroughToMature(t *testing.T) {
	gappy := []internalconfig.AccountWarmupStage{
		{Name: "late-start", MinAgeDays: 5, MaxAgeDays: 10, DailyBudget: 100, RPMLimit: 2, ConcurrencyLimit: 1},
	}
	mature := internalconfig.DefaultAccountMatureLimits()

	got := AccountWarmupStageForAge(0, true, gappy, mature)
	if !got.Mature {
		t.Fatalf("Mature = false, want true (age 0 falls in the gap before the first malformed stage)")
	}
}

// TestWarmupFreshnessFactor_MonotonicAndBoundedBelowOne walks every day of
// the default curve and asserts the freshness ramp is non-decreasing and
// strictly less than 1 throughout (design §5.2: "养号中 <1").
func TestWarmupFreshnessFactor_MonotonicAndBoundedBelowOne(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	mature := internalconfig.DefaultAccountMatureLimits()

	prev := -1.0
	for day := 0; day < 60; day++ {
		status := AccountWarmupStageForAge(day, true, curve, mature)
		if status.FreshnessFactor < prev {
			t.Fatalf("day %d: FreshnessFactor = %v, went backward from %v", day, status.FreshnessFactor, prev)
		}
		if status.FreshnessFactor >= 1 {
			t.Fatalf("day %d: FreshnessFactor = %v, want < 1 (not yet mature)", day, status.FreshnessFactor)
		}
		prev = status.FreshnessFactor
	}
	// Day 59 (last day still inside the curve) should be noticeably closer
	// to 1 than day 0.
	day0 := AccountWarmupStageForAge(0, true, curve, mature)
	day59 := AccountWarmupStageForAge(59, true, curve, mature)
	if day0.FreshnessFactor != 0 {
		t.Fatalf("day 0: FreshnessFactor = %v, want 0", day0.FreshnessFactor)
	}
	// Computed the same way the implementation does (runtime float64
	// division of the same two integers) rather than an untyped constant
	// literal, so this assertion cannot drift from the implementation due to
	// any difference between compile-time constant rounding and runtime
	// IEEE-754 rounding.
	if want := float64(59) / float64(60); day59.FreshnessFactor != want {
		t.Fatalf("day 59: FreshnessFactor = %v, want %v (linear ramp over the 0-60 day curve span)", day59.FreshnessFactor, want)
	}
}

// TestWarmupFreshnessFactor_UnboundedTerminalStage exercises a
// custom-configured curve whose last stage leaves MaxAgeDays unbounded (0) --
// a config author's deliberate choice to never promote an account to
// AccountMatureLimitsConfig at all. Freshness must still be a bounded,
// monotonic [0,1] ramp: 0 at the curve start, reaching (and staying at) 1
// once the account enters the unbounded terminal stage, while Mature stays
// false forever (this curve, by construction, has no mature transition).
func TestWarmupFreshnessFactor_UnboundedTerminalStage(t *testing.T) {
	curve := []internalconfig.AccountWarmupStage{
		{Name: "ramp", MinAgeDays: 0, MaxAgeDays: 10, DailyBudget: 50, RPMLimit: 2, ConcurrencyLimit: 1},
		{Name: "steady", MinAgeDays: 10, MaxAgeDays: 0, DailyBudget: 500, RPMLimit: 10, ConcurrencyLimit: 2},
	}
	mature := internalconfig.DefaultAccountMatureLimits()

	mid := AccountWarmupStageForAge(5, true, curve, mature)
	if mid.StageName != "ramp" || mid.Mature {
		t.Fatalf("day 5: StageName=%q Mature=%v, want ramp/false", mid.StageName, mid.Mature)
	}
	if want := 0.5; mid.FreshnessFactor != want {
		t.Fatalf("day 5: FreshnessFactor = %v, want %v", mid.FreshnessFactor, want)
	}

	atTerminalStart := AccountWarmupStageForAge(10, true, curve, mature)
	if atTerminalStart.StageName != "steady" || atTerminalStart.Mature {
		t.Fatalf("day 10: StageName=%q Mature=%v, want steady/false", atTerminalStart.StageName, atTerminalStart.Mature)
	}
	if atTerminalStart.FreshnessFactor != 1 {
		t.Fatalf("day 10: FreshnessFactor = %v, want 1 (entering the unbounded terminal stage)", atTerminalStart.FreshnessFactor)
	}

	farIntoTerminal := AccountWarmupStageForAge(100000, true, curve, mature)
	if farIntoTerminal.StageName != "steady" || farIntoTerminal.Mature {
		t.Fatalf("day 100000: StageName=%q Mature=%v, want steady/false (unbounded stage never matures)", farIntoTerminal.StageName, farIntoTerminal.Mature)
	}
	if farIntoTerminal.FreshnessFactor != 1 {
		t.Fatalf("day 100000: FreshnessFactor = %v, want 1", farIntoTerminal.FreshnessFactor)
	}
}

// TestWarmupFreshnessFactor_DegenerateSingleUnboundedStage covers a single-
// stage curve spanning [0, unbounded) -- there is no finite span to ramp
// across at all, so freshness is simply 1 from day 0 onward.
func TestWarmupFreshnessFactor_DegenerateSingleUnboundedStage(t *testing.T) {
	curve := []internalconfig.AccountWarmupStage{
		{Name: "only", MinAgeDays: 0, MaxAgeDays: 0, DailyBudget: 100, RPMLimit: 5, ConcurrencyLimit: 1},
	}
	mature := internalconfig.DefaultAccountMatureLimits()

	for _, age := range []int{0, 1, 365} {
		got := AccountWarmupStageForAge(age, true, curve, mature)
		if got.FreshnessFactor != 1 {
			t.Fatalf("age %d: FreshnessFactor = %v, want 1 (degenerate curve)", age, got.FreshnessFactor)
		}
		if got.Mature {
			t.Fatalf("age %d: Mature = true, want false", age)
		}
	}
}

// TestAccountWarmupStatusFor exercises the *Auth + now convenience wrapper
// end to end, reusing the Phase 0 first_production_at anchor machinery
// (account_freshness.go) instead of hand-supplying (ageDays, hasAnchor).
func TestAccountWarmupStatusFor(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	t.Run("no anchor -> cold", func(t *testing.T) {
		got := AccountWarmupStatusFor(&Auth{}, accountWarmupTestAnchor, cfg)
		if got.StageName != accountWarmupColdStageName {
			t.Fatalf("StageName = %q, want %q", got.StageName, accountWarmupColdStageName)
		}
		if got.FreshnessFactor != 0 {
			t.Fatalf("FreshnessFactor = %v, want 0", got.FreshnessFactor)
		}
	})

	t.Run("anchored 20 days ago -> w3-4", func(t *testing.T) {
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWarmupTestAnchor.Format(time.RFC3339),
		}}
		now := accountWarmupTestAnchor.Add(20 * 24 * time.Hour)
		got := AccountWarmupStatusFor(auth, now, cfg)
		if got.StageName != "w3-4" {
			t.Fatalf("StageName = %q, want w3-4", got.StageName)
		}
		if got.DailyBudget != 2000 {
			t.Fatalf("DailyBudget = %d, want 2000", got.DailyBudget)
		}
	})

	t.Run("anchored 90 days ago -> mature", func(t *testing.T) {
		auth := &Auth{Metadata: map[string]any{
			FirstProductionAtMetadataKey: accountWarmupTestAnchor.Format(time.RFC3339),
		}}
		now := accountWarmupTestAnchor.Add(90 * 24 * time.Hour)
		got := AccountWarmupStatusFor(auth, now, cfg)
		if !got.Mature {
			t.Fatalf("Mature = false, want true")
		}
		if got.DailyBudget != 0 {
			t.Fatalf("DailyBudget = %d, want 0", got.DailyBudget)
		}
		if got.RPMLimit != internalconfig.DefaultAccountMatureRPMLimit {
			t.Fatalf("RPMLimit = %d, want %d", got.RPMLimit, internalconfig.DefaultAccountMatureRPMLimit)
		}
	})

	t.Run("nil auth behaves like no anchor", func(t *testing.T) {
		got := AccountWarmupStatusFor(nil, accountWarmupTestAnchor, cfg)
		if got.StageName != accountWarmupColdStageName {
			t.Fatalf("StageName = %q, want %q", got.StageName, accountWarmupColdStageName)
		}
	})
}
