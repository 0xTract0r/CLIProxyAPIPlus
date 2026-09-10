package auth

import (
	"math"
	"testing"
	"time"
)

// setSingleQuotaWindow replaces auth's quota_snapshot with a single Claude usage
// window, so AccountQuotaHeadroom binds to it deterministically. resets may be ""
// to omit resets_at (headroom.ResetsAt then stays zero).
func setSingleQuotaWindow(a *Auth, window string, utilization float64, resets string) {
	w := map[string]any{"utilization": utilization}
	if resets != "" {
		w["resets_at"] = resets
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	a.Metadata["quota_snapshot"] = map[string]any{
		"usage": map[string]any{window: w},
	}
}

func approxEqual(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

// TestUpdateAccountBurnObservability_FirstSampleSeedsBaselineOnly: the first
// sample cannot yield a rate; it seeds the baseline and reports burn/projection
// as unknown (Has*=false), never 0.
func TestUpdateAccountBurnObservability_FirstSampleSeedsBaselineOnly(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}
	setSingleQuotaWindow(a, "seven_day", 40, "")

	UpdateAccountBurnObservability(a, a, t0)

	st := ReadAccountBurnState(a)
	if !st.HasPrev {
		t.Fatal("first sample must seed a baseline (HasPrev=true)")
	}
	if st.PrevWindow != "seven_day" {
		t.Fatalf("PrevWindow = %q, want seven_day", st.PrevWindow)
	}
	if !approxEqual(st.PrevUtilizationPercent, 40, 1e-9) {
		t.Fatalf("PrevUtilizationPercent = %v, want 40", st.PrevUtilizationPercent)
	}
	if !st.PrevAt.Equal(t0) {
		t.Fatalf("PrevAt = %v, want %v", st.PrevAt, t0)
	}
	if st.HasBurnRate {
		t.Fatal("first sample must NOT report a burn rate")
	}
	if st.HasProjection {
		t.Fatal("first sample must NOT report a projection")
	}
}

// TestUpdateAccountBurnObservability_SecondSampleComputesRateAndProjection: two
// same-window samples yield ΔUtil%/Δt and a projected exhaustion.
func TestUpdateAccountBurnObservability_SecondSampleComputesRateAndProjection(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}

	setSingleQuotaWindow(a, "seven_day", 40, "")
	UpdateAccountBurnObservability(a, a, t0)

	// 2h later, utilization climbed 40 -> 60 => 10 util-points/hour.
	setSingleQuotaWindow(a, "seven_day", 60, "")
	UpdateAccountBurnObservability(a, a, t0.Add(2*time.Hour))

	st := ReadAccountBurnState(a)
	if !st.HasBurnRate {
		t.Fatal("second same-window sample must yield a burn rate")
	}
	if !approxEqual(st.BurnRatePerHour, 10, 1e-9) {
		t.Fatalf("BurnRatePerHour = %v, want 10", st.BurnRatePerHour)
	}
	// headroom now 0.4 => 40 remaining points / 10 per hour = 4h out from t0+2h.
	if !st.HasProjection {
		t.Fatal("expected a projection when burning")
	}
	wantProjected := t0.Add(6 * time.Hour)
	if !st.ProjectedExhaustionAt.Equal(wantProjected) {
		t.Fatalf("ProjectedExhaustionAt = %v, want %v", st.ProjectedExhaustionAt, wantProjected)
	}
	// Baseline advanced to the fresh sample.
	if !approxEqual(st.PrevUtilizationPercent, 60, 1e-9) || !st.PrevAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("baseline not advanced: util=%v at=%v", st.PrevUtilizationPercent, st.PrevAt)
	}
}

// TestUpdateAccountBurnObservability_EWMASmoothing: a third sample blends into the
// EWMA rather than jumping to the latest instantaneous rate.
func TestUpdateAccountBurnObservability_EWMASmoothing(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}

	setSingleQuotaWindow(a, "seven_day", 40, "")
	UpdateAccountBurnObservability(a, a, t0)
	setSingleQuotaWindow(a, "seven_day", 60, "") // +10/h
	UpdateAccountBurnObservability(a, a, t0.Add(2*time.Hour))
	setSingleQuotaWindow(a, "seven_day", 90, "") // +15/h instantaneous
	UpdateAccountBurnObservability(a, a, t0.Add(4*time.Hour))

	st := ReadAccountBurnState(a)
	// EWMA = alpha*15 + (1-alpha)*10 with alpha=0.3 => 11.5
	want := BurnRateEWMAAlpha*15 + (1-BurnRateEWMAAlpha)*10
	if !approxEqual(st.BurnRatePerHour, want, 1e-9) {
		t.Fatalf("BurnRatePerHour = %v, want %v (EWMA blend)", st.BurnRatePerHour, want)
	}
}

// TestUpdateAccountBurnObservability_WindowSwitchDropsRate: when the binding
// window changes between samples, a cross-window delta is meaningless, so the
// rate/projection are cleared back to unknown while a fresh baseline is seeded.
func TestUpdateAccountBurnObservability_WindowSwitchDropsRate(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}

	setSingleQuotaWindow(a, "seven_day", 40, "")
	UpdateAccountBurnObservability(a, a, t0)
	setSingleQuotaWindow(a, "seven_day", 60, "")
	UpdateAccountBurnObservability(a, a, t0.Add(2*time.Hour))
	if st := ReadAccountBurnState(a); !st.HasBurnRate {
		t.Fatal("precondition: expected a burn rate before the window switch")
	}

	// Binding window is now five_hour (tightest), a different window.
	setSingleQuotaWindow(a, "five_hour", 80, "")
	UpdateAccountBurnObservability(a, a, t0.Add(3*time.Hour))

	st := ReadAccountBurnState(a)
	if st.HasBurnRate {
		t.Fatal("window switch must drop the burn rate (cross-window delta is meaningless)")
	}
	if st.HasProjection {
		t.Fatal("window switch must drop the projection")
	}
	if st.PrevWindow != "five_hour" || !approxEqual(st.PrevUtilizationPercent, 80, 1e-9) {
		t.Fatalf("baseline not reseeded to new window: window=%q util=%v", st.PrevWindow, st.PrevUtilizationPercent)
	}
}

// TestUpdateAccountBurnObservability_UtilizationDropNoProjection: a utilization
// drop (window reset / jitter) feeds a 0 instantaneous rate so the EWMA decays,
// and yields no projection.
func TestUpdateAccountBurnObservability_UtilizationDropNoProjection(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}

	setSingleQuotaWindow(a, "seven_day", 90, "")
	UpdateAccountBurnObservability(a, a, t0)
	// Window reset: utilization drops 90 -> 10.
	setSingleQuotaWindow(a, "seven_day", 10, "")
	UpdateAccountBurnObservability(a, a, t0.Add(1*time.Hour))

	st := ReadAccountBurnState(a)
	if !st.HasBurnRate {
		t.Fatal("expected a (decayed) burn rate to be recorded")
	}
	if !approxEqual(st.BurnRatePerHour, 0, 1e-9) {
		t.Fatalf("BurnRatePerHour = %v, want 0 after a utilization drop", st.BurnRatePerHour)
	}
	if st.HasProjection {
		t.Fatal("a non-positive burn must not project an exhaustion time")
	}
}

// TestUpdateAccountBurnObservability_UnknownQuotaLeavesStateUntouched: when the
// fresh snapshot has no usable window (unknown), existing burn state is preserved
// and never fabricated from unknown.
func TestUpdateAccountBurnObservability_UnknownQuotaLeavesStateUntouched(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}
	setSingleQuotaWindow(a, "seven_day", 40, "")
	UpdateAccountBurnObservability(a, a, t0)

	// Replace with a snapshot carrying no recognizable window (e.g. codex-shaped).
	a.Metadata["quota_snapshot"] = map[string]any{
		"usage": map[string]any{"extra_usage": map[string]any{"is_enabled": false}},
	}
	UpdateAccountBurnObservability(a, a, t0.Add(5*time.Hour))

	st := ReadAccountBurnState(a)
	if !approxEqual(st.PrevUtilizationPercent, 40, 1e-9) || !st.PrevAt.Equal(t0) {
		t.Fatalf("unknown quota must leave baseline untouched: util=%v at=%v", st.PrevUtilizationPercent, st.PrevAt)
	}
}

// TestUpdateAccountBurnObservability_NilUpdatedIsNoop guards the defensive nil path.
func TestUpdateAccountBurnObservability_NilUpdatedIsNoop(t *testing.T) {
	UpdateAccountBurnObservability(nil, nil, time.Now()) // must not panic
}

// TestAccountPacingObservabilityFor_NullWithoutBurnHistory: no burn rate persisted
// => pacing factor is unknown (not a fabricated 1.0).
func TestAccountPacingObservabilityFor_NullWithoutBurnHistory(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}
	setSingleQuotaWindow(a, "seven_day", 60, now.Add(4*time.Hour).Format(time.RFC3339))

	out := AccountPacingObservabilityFor(a, now)
	if out.HasBurnRate {
		t.Fatal("no burn history should mean HasBurnRate=false")
	}
	if out.HasPacingFactor {
		t.Fatal("pacing factor must be unknown without a burn rate")
	}
}

// TestAccountPacingObservabilityFor_ComputesClampedFactor: p = clamp(k*fair/burn,
// floor, 1) on the binding window.
func TestAccountPacingObservabilityFor_ComputesClampedFactor(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// headroom 0.4 => 40 remaining points; reset in 4h => fair = 10/h.
	base := func(burn float64) *Auth {
		a := &Auth{Provider: "claude"}
		setSingleQuotaWindow(a, "seven_day", 60, now.Add(4*time.Hour).Format(time.RFC3339))
		a.Metadata["account_scheduling"] = map[string]any{
			accountSchedulingBurnRateEWMAKey:   burn,
			accountSchedulingBurnPrevWindowKey: "seven_day",
		}
		return a
	}

	t.Run("mid-range burn produces fair/burn", func(t *testing.T) {
		out := AccountPacingObservabilityFor(base(20), now) // fair 10 / burn 20 = 0.5
		if !out.HasPacingFactor {
			t.Fatal("expected a pacing factor")
		}
		if !approxEqual(out.PacingFactorDryRun, 0.5, 1e-9) {
			t.Fatalf("PacingFactorDryRun = %v, want 0.5", out.PacingFactorDryRun)
		}
	})

	t.Run("runaway burn clamps to floor", func(t *testing.T) {
		out := AccountPacingObservabilityFor(base(1000), now) // 10/1000 = 0.01 < floor
		if !approxEqual(out.PacingFactorDryRun, PacingFactorFloor, 1e-9) {
			t.Fatalf("PacingFactorDryRun = %v, want floor %v", out.PacingFactorDryRun, PacingFactorFloor)
		}
	})

	t.Run("slow burn clamps to 1", func(t *testing.T) {
		out := AccountPacingObservabilityFor(base(1), now) // 10/1 = 10 -> clamp 1
		if !approxEqual(out.PacingFactorDryRun, 1, 1e-9) {
			t.Fatalf("PacingFactorDryRun = %v, want 1", out.PacingFactorDryRun)
		}
	})
}

// TestAccountPacingObservabilityFor_NullWhenResetsMissing: without a binding-window
// resets_at, fair cannot be computed, so pacing is unknown even with a burn rate.
func TestAccountPacingObservabilityFor_NullWhenResetsMissing(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "claude"}
	setSingleQuotaWindow(a, "seven_day", 60, "") // no resets_at
	a.Metadata["account_scheduling"] = map[string]any{
		accountSchedulingBurnRateEWMAKey: 20.0,
	}

	out := AccountPacingObservabilityFor(a, now)
	if !out.HasBurnRate {
		t.Fatal("burn rate should still be reported")
	}
	if out.HasPacingFactor {
		t.Fatal("missing resets_at must make the pacing factor unknown")
	}
}
