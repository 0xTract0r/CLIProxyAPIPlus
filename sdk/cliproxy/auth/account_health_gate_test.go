package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// healthGateTestNow is a fixed clock so day-math and cooldown windows are
// deterministic across the ANCHOR-Q4 tests.
var healthGateTestNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// healthGateTestConfig returns the default adaptive-scheduling config, whose
// health gate is enabled with the design §10 defaults (failure-cluster 3 /
// backoff 1 / window 30m / demote 1 / cooldown 30m) on the default 5-stage
// warm-up curve (w1..w7-8, mature at day 60).
func healthGateTestConfig() internalconfig.AccountSchedulingConfig {
	return internalconfig.DefaultAccountSchedulingConfig()
}

// claudeAuthAgedDays builds a Claude auth anchored `days` before now (so its
// AccountAgeDays == days). days<0 yields an un-anchored ("cold") account.
func claudeAuthAgedDays(days int, now time.Time) *Auth {
	a := &Auth{ID: "acct", Provider: "claude", Metadata: map[string]any{}}
	if days >= 0 {
		a.SetAccountFirstProductionAt(now.Add(-time.Duration(days) * 24 * time.Hour))
	}
	return a
}

// --- pure helpers: ageWarmupStageIndex + effectiveWarmupAge (min clamp) ---

func TestAgeWarmupStageIndex_DefaultCurve(t *testing.T) {
	curve := internalconfig.DefaultAccountWarmupCurve()
	tests := []struct {
		age  int
		want int
	}{
		{0, 0}, {6, 0}, {7, 1}, {13, 1}, {14, 2}, {29, 2},
		{30, 3}, {44, 3}, {45, 4}, {59, 4}, {60, 5}, {1000, 5}, {-5, 0},
	}
	for _, tc := range tests {
		if got := ageWarmupStageIndex(curve, tc.age); got != tc.want {
			t.Fatalf("ageWarmupStageIndex(age=%d) = %d, want %d", tc.age, got, tc.want)
		}
	}
}

func TestEffectiveWarmupAge_MinClamp(t *testing.T) {
	cfg := healthGateTestConfig()
	curve := cfg.WarmupCurve
	tests := []struct {
		name        string
		rawAge      int
		hasAnchor   bool
		cap         int
		hasCap      bool
		gateEnabled bool
		wantAge     int
		wantAnchor  bool
	}{
		{"cap below age clamps to capped stage min", 50, true, 1, true, true, curve[1].MinAgeDays, true},
		{"cap equal to age is no-op", 50, true, 4, true, true, 50, true},
		{"cap above age is no-op", 10, true, 4, true, true, 10, true},
		// Fix 1 (HIGH, corrected behavior): a MATURE account (age 100 -> ageIdx 5,
		// past the 5-stage curve) is out of the health gate's scope, so a stale cap
		// is IGNORED and its real age passes through unchanged. Previously this
		// demoted a mature account to curve[4].MinAgeDays; the gate only governs
		// warmup-period accounts (spec "养号期账号"), and a mature account is covered
		// by cooldown / quota-deweight / auto-quarantine instead.
		{"mature account excluded -> raw age (stale cap ignored)", 100, true, 4, true, true, 100, true},
		{"no cap recorded -> raw age", 100, true, 0, false, true, 100, true},
		{"no anchor -> pass-through cold regardless of cap", 0, false, 1, true, true, 0, false},
		{"gate disabled -> raw age even with cap", 50, true, 1, true, false, 50, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			c.HealthGate.Enabled = tc.gateEnabled
			a := &Auth{Provider: "claude", Metadata: map[string]any{}}
			if tc.hasCap {
				a.setHealthStageCap(tc.cap)
			}
			gotAge, gotAnchor := effectiveWarmupAge(a, c, tc.rawAge, tc.hasAnchor)
			if gotAge != tc.wantAge || gotAnchor != tc.wantAnchor {
				t.Fatalf("effectiveWarmupAge = (%d,%v), want (%d,%v)", gotAge, gotAnchor, tc.wantAge, tc.wantAnchor)
			}
		})
	}
}

// --- distress signals ---

func TestAccountInDistress_Signals(t *testing.T) {
	cfg := healthGateTestConfig() // failure-cluster 3, backoff 1, window 30m
	now := healthGateTestNow

	t.Run("backoff level at threshold -> distress", func(t *testing.T) {
		a := &Auth{Provider: "claude"}
		a.Quota.BackoffLevel = 1
		if !AccountInDistress(a, cfg, now) {
			t.Fatal("want distress from BackoffLevel>=threshold")
		}
	})
	t.Run("failure cluster at threshold -> distress", func(t *testing.T) {
		a := &Auth{Provider: "claude"}
		for i := 0; i < 3; i++ {
			a.recordRecentRequest(now, false)
		}
		if !AccountInDistress(a, cfg, now) {
			t.Fatal("want distress from failure cluster >= threshold")
		}
	})
	t.Run("below both thresholds -> healthy", func(t *testing.T) {
		a := &Auth{Provider: "claude"}
		a.recordRecentRequest(now, false)
		a.recordRecentRequest(now, true)
		if AccountInDistress(a, cfg, now) {
			t.Fatal("2 failures < threshold 3 and no backoff must not be distress")
		}
	})
	t.Run("stale failures outside window are not counted", func(t *testing.T) {
		a := &Auth{Provider: "claude"}
		old := now.Add(-2 * time.Hour) // outside the 30m window
		for i := 0; i < 5; i++ {
			a.recordRecentRequest(old, false)
		}
		if AccountInDistress(a, cfg, now) {
			t.Fatal("failures older than the observation window must not trip distress")
		}
	})
	t.Run("disabled gate never distress", func(t *testing.T) {
		c := cfg
		c.HealthGate.Enabled = false
		a := &Auth{Provider: "claude"}
		a.Quota.BackoffLevel = 9
		if AccountInDistress(a, c, now) {
			t.Fatal("disabled gate must report no distress")
		}
	})
}

func TestRecentFailuresWithin_WindowBucketing(t *testing.T) {
	a := &Auth{Provider: "claude"}
	now := healthGateTestNow
	// 2 failures in the current bucket, 3 failures ~15m ago (still within 30m),
	// 4 failures ~50m ago (outside 30m).
	for i := 0; i < 2; i++ {
		a.recordRecentRequest(now, false)
	}
	for i := 0; i < 3; i++ {
		a.recordRecentRequest(now.Add(-15*time.Minute), false)
	}
	for i := 0; i < 4; i++ {
		a.recordRecentRequest(now.Add(-50*time.Minute), false)
	}
	if got := a.recentFailuresWithin(now, 30*time.Minute); got != 5 {
		t.Fatalf("recentFailuresWithin(30m) = %d, want 5 (2 now + 3 at -15m, excluding -50m)", got)
	}
	if got := a.recentFailuresWithin(now, 0); got != 0 {
		t.Fatalf("recentFailuresWithin(0) = %d, want 0", got)
	}
}

// --- write-side state machine: evaluateAccountHealthGate ---

func TestEvaluateAccountHealthGate_DistressDemotesOnlyDown(t *testing.T) {
	cfg := healthGateTestConfig() // demoteStep 1, backoff threshold 1
	now := healthGateTestNow
	// Fix 1: the demote mechanics are now only exercised on a warmup-period account
	// (mature accounts are excluded), so anchor this at day 50 -> ageIdx 4 (w7-8),
	// the highest warmup stage, rather than the old mature day-100 vehicle.
	a := claudeAuthAgedDays(50, now) // warming, ageIdx 4
	a.Quota.BackoffLevel = 2         // distress

	evaluateAccountHealthGate(a, false, now, cfg)
	if cap, ok := AccountHealthStageCap(a); !ok || cap != 3 {
		t.Fatalf("after 1st distress cap = (%d,%v), want (3,true) [ageIdx 4 - demote 1]", cap, ok)
	}
	if _, ok := AccountLastDistressAt(a); !ok {
		t.Fatal("last_distress_at must be stamped on distress")
	}

	evaluateAccountHealthGate(a, false, now, cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 2 {
		t.Fatalf("after 2nd distress cap = %d, want 2 (steps down from existing cap, not age)", cap)
	}

	// Drive down to the floor and assert it never goes negative.
	for i := 0; i < 10; i++ {
		evaluateAccountHealthGate(a, false, now, cfg)
	}
	if cap, _ := AccountHealthStageCap(a); cap != 0 {
		t.Fatalf("cap floored = %d, want 0", cap)
	}
}

func TestEvaluateAccountHealthGate_DemoteUsesMinOfCapAndAge(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow
	a := claudeAuthAgedDays(50, now) // warming, ageIdx 4 (Fix 1: mature excluded)
	a.setHealthStageCap(2)           // existing cap below age
	a.Quota.BackoffLevel = 5         // distress

	evaluateAccountHealthGate(a, false, now, cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 1 {
		t.Fatalf("cap = %d, want 1 (min(existingCap 2, ageIdx 4) - 1)", cap)
	}
}

func TestEvaluateAccountHealthGate_PromoteAfterCooldownThenClear(t *testing.T) {
	cfg := healthGateTestConfig() // cooldown 30m
	now := healthGateTestNow
	// Fix 1: warmup-period account (ageIdx 4) instead of the old mature day-100.
	a := claudeAuthAgedDays(50, now)
	a.setHealthStageCap(2)
	a.setLastDistressAt(now.Add(-40 * time.Minute)) // cooldown (30m) elapsed

	// Healthy success -> promote one stage (2 -> 3). Fix 2 re-stamps last_distress_at
	// to now, so the promote clock restarts here.
	evaluateAccountHealthGate(a, true, now, cfg)
	if cap, ok := AccountHealthStageCap(a); !ok || cap != 3 {
		t.Fatalf("after promote cap = (%d,%v), want (3,true)", cap, ok)
	}

	// Fix 2 (corrected behavior): a second success in the SAME cooldown window must
	// NOT promote again -- previously each success bumped +1 back to full in seconds.
	evaluateAccountHealthGate(a, true, now, cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 3 {
		t.Fatalf("cap = %d, want 3 (rate-limited: at most one promote per cooldown window)", cap)
	}

	// After another cooldown window elapses, the next success promotes 3 -> 4;
	// newCap 4 >= ageIdx 4 -> clear entirely (back to pure age-based).
	later := now.Add(40 * time.Minute)
	evaluateAccountHealthGate(a, true, later, cfg)
	if _, ok := AccountHealthStageCap(a); ok {
		t.Fatal("cap must be cleared once it reaches the age-deserved stage")
	}
	if _, ok := AccountLastDistressAt(a); ok {
		t.Fatal("last_distress_at must be cleared with the cap")
	}
}

// TestEvaluateAccountHealthGate_PromoteRateLimitedPerCooldown is the dedicated Fix 2
// (MEDIUM) guard: a burst of healthy successes after the cooldown has elapsed may
// raise the cap by at most ONE stage; only after a further full cooldown window may
// it rise again. Before Fix 2 the promote branch bumped the cap +1 on every healthy
// success once the cooldown had passed (last_distress_at was not refreshed on
// promote), letting a demoted account race back to full ramp within seconds and
// defeating the gradual re-ramp the spec's "冷静期" requires.
func TestEvaluateAccountHealthGate_PromoteRateLimitedPerCooldown(t *testing.T) {
	cfg := healthGateTestConfig() // cooldown 30m
	now := healthGateTestNow
	a := claudeAuthAgedDays(50, now) // warming, ageIdx 4
	a.setHealthStageCap(1)
	a.setLastDistressAt(now.Add(-40 * time.Minute)) // cooldown elapsed

	// A burst of five successes at the same instant promotes exactly once (1 -> 2).
	for i := 0; i < 5; i++ {
		evaluateAccountHealthGate(a, true, now, cfg)
	}
	if cap, ok := AccountHealthStageCap(a); !ok || cap != 2 {
		t.Fatalf("cap after burst = (%d,%v), want (2,true) (at most +1 per cooldown window)", cap, ok)
	}

	// Still inside the fresh cooldown window (re-stamped at now): no further promote.
	evaluateAccountHealthGate(a, true, now.Add(20*time.Minute), cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 2 {
		t.Fatalf("cap = %d, want 2 (still within the re-stamped cooldown window)", cap)
	}

	// A full cooldown after the last promote: one more promote (2 -> 3).
	evaluateAccountHealthGate(a, true, now.Add(40*time.Minute), cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 3 {
		t.Fatalf("cap = %d, want 3 (+1 allowed after another cooldown window)", cap)
	}
}

func TestEvaluateAccountHealthGate_HoldDuringCooldown(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow
	a := claudeAuthAgedDays(50, now) // warming, ageIdx 4 (Fix 1: mature excluded)
	a.setHealthStageCap(3)
	a.setLastDistressAt(now.Add(-10 * time.Minute)) // still inside 30m cooldown

	evaluateAccountHealthGate(a, true, now, cfg)
	if cap, _ := AccountHealthStageCap(a); cap != 3 {
		t.Fatalf("cap = %d, want 3 (held during cooldown, no promotion)", cap)
	}
}

func TestEvaluateAccountHealthGate_NoPromoteWithoutSuccessOrAge(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow

	t.Run("plain failure (not distress) never promotes", func(t *testing.T) {
		a := claudeAuthAgedDays(50, now) // warming, ageIdx 4 (Fix 1: mature excluded)
		a.setHealthStageCap(3)
		a.setLastDistressAt(now.Add(-40 * time.Minute)) // cooldown elapsed
		evaluateAccountHealthGate(a, false, now, cfg)   // failed, but below cluster threshold
		if cap, _ := AccountHealthStageCap(a); cap != 3 {
			t.Fatalf("cap = %d, want 3 (a failure must not promote even after cooldown)", cap)
		}
	})

	t.Run("age alone cannot bypass a health cap", func(t *testing.T) {
		// A still-warming, still-distressed account never climbs: distress keeps
		// re-clamping and resetting the cooldown, so 熬账龄 does not raise the
		// effective stage. (Fix 1: exercised on a warmup account, ageIdx 4.)
		a := claudeAuthAgedDays(50, now)
		a.setHealthStageCap(2)
		a.Quota.BackoffLevel = 3 // persistent distress
		for i := 0; i < 5; i++ {
			evaluateAccountHealthGate(a, false, now, cfg)
		}
		if cap, _ := AccountHealthStageCap(a); cap != 0 {
			t.Fatalf("cap = %d, want 0 (persistent distress drives it to the floor, age never bypasses)", cap)
		}
	})
}

func TestEvaluateAccountHealthGate_ClaudeOnly(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow
	a := &Auth{ID: "cx", Provider: "codex", Metadata: map[string]any{}}
	a.SetAccountFirstProductionAt(now.Add(-100 * 24 * time.Hour))
	a.Quota.BackoffLevel = 9 // would be distress if it applied

	evaluateAccountHealthGate(a, false, now, cfg)
	if _, ok := AccountHealthStageCap(a); ok {
		t.Fatal("health gate must not write a cap for a non-Claude (codex) account")
	}
}

func TestEvaluateAccountHealthGate_DisabledNoOp(t *testing.T) {
	cfg := healthGateTestConfig()
	cfg.HealthGate.Enabled = false
	now := healthGateTestNow
	a := claudeAuthAgedDays(100, now)
	a.Quota.BackoffLevel = 9

	evaluateAccountHealthGate(a, false, now, cfg)
	if _, ok := AccountHealthStageCap(a); ok {
		t.Fatal("disabled health gate must not write a cap")
	}
}

func TestEvaluateAccountHealthGate_FailureClusterDemotes(t *testing.T) {
	cfg := healthGateTestConfig() // failure-cluster threshold 3
	now := healthGateTestNow
	a := claudeAuthAgedDays(50, now) // warming, ageIdx 4 (Fix 1: mature excluded)

	// Two failures: below threshold -> no cap yet.
	a.recordRecentRequest(now, false)
	evaluateAccountHealthGate(a, false, now, cfg)
	a.recordRecentRequest(now, false)
	evaluateAccountHealthGate(a, false, now, cfg)
	if _, ok := AccountHealthStageCap(a); ok {
		t.Fatal("2 failures (< threshold 3) must not demote")
	}
	// Third failure trips the cluster -> demote once.
	a.recordRecentRequest(now, false)
	evaluateAccountHealthGate(a, false, now, cfg)
	if cap, ok := AccountHealthStageCap(a); !ok || cap != 3 {
		t.Fatalf("cap = (%d,%v), want (3,true) after failure cluster tripped [ageIdx 4 - 1]", cap, ok)
	}
}

// TestEvaluateAccountHealthGate_MatureExcluded is the Fix 1 (HIGH) guard: a mature
// Claude account (past every warm-up stage) is OUT of the health gate's scope, so no
// amount of distress records a cap and its effective warm-up stage stays mature. The
// gate governs only warmup-period accounts (spec "养号期账号"); a mature account is
// covered by cooldown / quota-deweight / auto-quarantine instead. Before Fix 1 the
// write side demoted a mature account into a warm-up daily-budget hard gate, which on
// a thin pool (a single mature 20x account, ANCHOR-2) could 429 legitimate traffic.
func TestEvaluateAccountHealthGate_MatureExcluded(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow
	a := claudeAuthAgedDays(100, now) // mature, ageIdx 5 (past the 5-stage curve)
	a.Quota.BackoffLevel = 9          // strong, persistent distress

	// Both distress signals, driven repeatedly: still no cap for a mature account.
	for i := 0; i < 6; i++ {
		a.recordRecentRequest(now, false)
	}
	for i := 0; i < 10; i++ {
		evaluateAccountHealthGate(a, false, now, cfg)
	}
	if cap, ok := AccountHealthStageCap(a); ok {
		t.Fatalf("mature account must never get a health cap, got (%d,%v)", cap, ok)
	}
	if _, ok := AccountLastDistressAt(a); ok {
		t.Fatal("mature account must not be stamped with last_distress_at")
	}

	// Effective stage stays mature: no cap, and the read side would ignore one anyway.
	if st := AccountWarmupStatusFor(a, now, cfg); !st.Mature {
		t.Fatalf("mature account effective stage = %q mature=%v, want mature/true", st.StageName, st.Mature)
	}
}

// TestEvaluateAccountHealthGate_ColdAccountSkipped is the Fix 3 (LOW) guard: an
// account with no first_production_at anchor ("cold") early-returns without writing a
// cap or stamping last_distress_at, even under distress. The read side already
// pass-throughs a cold account (effectiveWarmupAge's no-anchor branch), so a cap here
// would be dead metadata; before Fix 3 the write side stamped last_distress_at and
// wrote a cap of 0 on every cold-account distress result -- pointless persistence churn.
func TestEvaluateAccountHealthGate_ColdAccountSkipped(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow
	a := claudeAuthAgedDays(-1, now) // un-anchored (cold)
	a.Quota.BackoffLevel = 9         // would be distress if the gate applied

	for i := 0; i < 5; i++ {
		a.recordRecentRequest(now, false)
		evaluateAccountHealthGate(a, false, now, cfg)
	}
	if cap, ok := AccountHealthStageCap(a); ok {
		t.Fatalf("cold account must not get a health cap, got (%d,%v)", cap, ok)
	}
	if _, ok := AccountLastDistressAt(a); ok {
		t.Fatal("cold account must not be stamped with last_distress_at")
	}
}

// --- persistence across Clone (quota-refresh survival, design §10.5) ---

func TestAccountHealthGateState_SurvivesClone(t *testing.T) {
	now := healthGateTestNow
	a := claudeAuthAgedDays(100, now)
	a.setHealthStageCap(2)
	a.setLastDistressAt(now)

	clone := a.Clone()
	if cap, ok := AccountHealthStageCap(clone); !ok || cap != 2 {
		t.Fatalf("cloned cap = (%d,%v), want (2,true)", cap, ok)
	}
	last, ok := AccountLastDistressAt(clone)
	if !ok || !last.Equal(now) {
		t.Fatalf("cloned last_distress_at = (%v,%v), want (%v,true)", last, ok, now)
	}
}

// TestAccountHealthGateState_SurvivesQuotaRefreshClone models the actual
// production survival path (design §6.4/§10.5): a quota refresh replaces the
// nested quota_snapshot wholesale but Clone copies the top-level
// account_scheduling key through, so the health cap and last-distress timestamp
// survive. It asserts the cap is still readable after a Clone that also swaps
// quota_snapshot, which is the property the spec's "重启不回高档 / 跨配额刷新存活"
// scenario actually requires (independent-deep-copy of the nested object is NOT a
// Clone guarantee and is not needed -- the snapshot is only ever read downstream).
func TestAccountHealthGateState_SurvivesQuotaRefreshClone(t *testing.T) {
	now := healthGateTestNow
	a := claudeAuthAgedDays(100, now)
	a.setHealthStageCap(2)
	a.Metadata["quota_snapshot"] = map[string]any{"stale": true}

	clone := a.Clone()
	// Simulate a quota refresh replacing quota_snapshot wholesale on the clone.
	clone.Metadata["quota_snapshot"] = map[string]any{"fresh": true}

	if cap, ok := AccountHealthStageCap(clone); !ok || cap != 2 {
		t.Fatalf("cap after quota-refresh clone = (%d,%v), want (2,true) [top-level account_scheduling survives]", cap, ok)
	}
}

// --- read side reflects the cap (grading + weight views) ---

func TestAccountWarmupStatusFor_ReflectsHealthCap(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow

	mature := claudeAuthAgedDays(100, now)
	if st := AccountWarmupStatusFor(mature, now, cfg); st.StageName != "mature" || !st.Mature {
		t.Fatalf("uncapped mature account stage = %q mature=%v, want mature/true", st.StageName, st.Mature)
	}

	// Fix 1 (HIGH, corrected behavior): a stale cap on a MATURE account is ignored on
	// the read side -- it stays mature rather than being demoted into a warm-up stage.
	matureCapped := claudeAuthAgedDays(100, now)
	matureCapped.setHealthStageCap(4) // stale cap; must be ignored for a mature account
	if st := AccountWarmupStatusFor(matureCapped, now, cfg); st.StageName != "mature" || !st.Mature {
		t.Fatalf("mature account with a stale cap = %q mature=%v, want mature/true (cap ignored)", st.StageName, st.Mature)
	}

	// A cap on a still-WARMING account (ageIdx 4 = w7-8) DOES clamp it down: cap 1 ->
	// the effective stage resolves to w2's limits.
	capped := claudeAuthAgedDays(50, now)
	capped.setHealthStageCap(1) // clamp w7-8 -> w2
	st := AccountWarmupStatusFor(capped, now, cfg)
	if st.StageName != "w2" || st.Mature {
		t.Fatalf("capped warming account stage = %q mature=%v, want w2/false", st.StageName, st.Mature)
	}
	if st.RPMLimit != 5 || st.DailyBudget != 500 || st.ConcurrencyLimit != 1 {
		t.Fatalf("capped limits = rpm %d daily %d conc %d, want 5/500/1 (w2)", st.RPMLimit, st.DailyBudget, st.ConcurrencyLimit)
	}
}

func TestAccountFreshnessWeightFactor_ReflectsHealthCap(t *testing.T) {
	cfg := healthGateTestConfig()
	now := healthGateTestNow

	mature := claudeAuthAgedDays(100, now)
	if f := AccountFreshnessWeightFactor(mature, cfg, now); f != 1 {
		t.Fatalf("uncapped mature freshness = %v, want 1", f)
	}

	// Fix 1 (HIGH, corrected behavior): a stale cap on a mature account is ignored on
	// the weight side too -- freshness stays 1, not decelerated.
	matureCapped := claudeAuthAgedDays(100, now)
	matureCapped.setHealthStageCap(1)
	if f := AccountFreshnessWeightFactor(matureCapped, cfg, now); f != 1 {
		t.Fatalf("mature account with a stale cap freshness = %v, want 1 (cap ignored)", f)
	}

	// A cap on a still-WARMING account (ageIdx 4) decelerates its freshness weight in
	// lock-step with the lowered effective stage.
	capped := claudeAuthAgedDays(50, now)
	capped.setHealthStageCap(1) // clamp to w2
	want := warmupRPMFreshnessFactor(cfg.WarmupCurve[1].RPMLimit, cfg.MatureLimits.RPMLimit)
	if f := AccountFreshnessWeightFactor(capped, cfg, now); f != want {
		t.Fatalf("capped freshness = %v, want %v (w2 rpm / mature rpm)", f, want)
	}
	if f := AccountFreshnessWeightFactor(capped, cfg, now); f >= 1 {
		t.Fatalf("capped freshness = %v, must be < 1 (decelerated)", f)
	}
}

// TestHealthGateDisabled_NoBehaviorChange asserts that with the gate disabled a
// capped account behaves exactly as an un-capped one -- the backward-compat
// guarantee (design §10.6: 关闭时行为完全等同现状).
func TestHealthGateDisabled_NoBehaviorChange(t *testing.T) {
	cfg := healthGateTestConfig()
	cfg.HealthGate.Enabled = false
	now := healthGateTestNow

	a := claudeAuthAgedDays(100, now)
	a.setHealthStageCap(0) // deepest possible cap, but gate is off

	st := AccountWarmupStatusFor(a, now, cfg)
	if !st.Mature {
		t.Fatalf("disabled gate must ignore the cap; stage = %q, want mature", st.StageName)
	}
	if f := AccountFreshnessWeightFactor(a, cfg, now); f != 1 {
		t.Fatalf("disabled gate freshness = %v, want 1 (cap ignored)", f)
	}
}

// --- MarkResult integration (wiring, design §10.4) ---

func planQuotaError() *Error {
	return &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit reached; quota exceeded"}}`}
}

func TestManagerMarkResult_HealthGateBackoffSignalDemotes(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.runtimeConfig.Store(&internalconfig.Config{AccountScheduling: healthGateTestConfig()})
	ctx := WithSkipPersist(context.Background())

	// Fix 1: the demote wiring is exercised on a warmup-period account (mature
	// accounts are excluded), so anchor this ~50 days back -> ageIdx 4 (w7-8).
	a := &Auth{ID: "claude-warming", Provider: "claude", Metadata: map[string]any{}}
	a.SetAccountFirstProductionAt(time.Now().Add(-50 * 24 * time.Hour)) // warming, ageIdx 4
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	// One plan-quota 429 (no RetryAfter) escalates Quota.BackoffLevel to >=1,
	// which trips the BackoffLevel distress signal and demotes the cap.
	mgr.MarkResult(ctx, Result{AuthID: "claude-warming", Provider: "claude", Model: "claude-sonnet-4", Success: false, QuotaExceeded: true, Error: planQuotaError()})

	got, ok := mgr.GetByID("claude-warming")
	if !ok || got == nil {
		t.Fatalf("GetByID ok=%v", ok)
	}
	if got.Quota.BackoffLevel < 1 {
		t.Fatalf("precondition: BackoffLevel = %d, want >=1 (plan-quota 429 should escalate)", got.Quota.BackoffLevel)
	}
	if cap, capOK := AccountHealthStageCap(got); !capOK || cap != 3 {
		t.Fatalf("cap after plan-quota 429 = (%d,%v), want (3,true) [ageIdx 4 - 1]", cap, capOK)
	}
	if _, lok := AccountLastDistressAt(got); !lok {
		t.Fatal("last_distress_at must be stamped through MarkResult")
	}
}

func TestManagerMarkResult_HealthGateFailureClusterDemotes(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.runtimeConfig.Store(&internalconfig.Config{AccountScheduling: healthGateTestConfig()})
	ctx := WithSkipPersist(context.Background())

	a := &Auth{ID: "claude-cluster", Provider: "claude", Metadata: map[string]any{}}
	a.SetAccountFirstProductionAt(time.Now().Add(-50 * 24 * time.Hour)) // warming, ageIdx 4 (Fix 1)
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	// Three transient 429s (rate_limit, no plan-quota) do NOT escalate BackoffLevel
	// but DO accumulate a failure cluster >= threshold 3 -> demote once.
	for i := 0; i < 3; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "claude-cluster", Provider: "claude", Model: "claude-sonnet-4", Success: false, Error: rateLimitError()})
	}

	got, _ := mgr.GetByID("claude-cluster")
	if cap, ok := AccountHealthStageCap(got); !ok || cap != 3 {
		t.Fatalf("cap after failure cluster = (%d,%v), want (3,true) [ageIdx 4 - 1]", cap, ok)
	}
}

// TestManagerMarkResult_HealthGateMatureExcluded is the Fix 1 (HIGH) guard at the
// MarkResult wiring level: a mature Claude account driven through the same
// plan-quota-429 distress path that demotes a warming account must NOT be capped --
// mature accounts are out of the health gate's scope (spec "养号期账号").
func TestManagerMarkResult_HealthGateMatureExcluded(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.runtimeConfig.Store(&internalconfig.Config{AccountScheduling: healthGateTestConfig()})
	ctx := WithSkipPersist(context.Background())

	a := &Auth{ID: "claude-mature", Provider: "claude", Metadata: map[string]any{}}
	a.SetAccountFirstProductionAt(time.Now().Add(-100 * 24 * time.Hour)) // mature, ageIdx 5
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	for i := 0; i < 3; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "claude-mature", Provider: "claude", Model: "claude-sonnet-4", Success: false, QuotaExceeded: true, Error: planQuotaError()})
	}

	got, _ := mgr.GetByID("claude-mature")
	if got.Quota.BackoffLevel < 1 {
		t.Fatalf("precondition: BackoffLevel = %d, want >=1 (plan-quota 429 should escalate)", got.Quota.BackoffLevel)
	}
	if cap, ok := AccountHealthStageCap(got); ok {
		t.Fatalf("mature account must not be capped through MarkResult, got (%d,%v)", cap, ok)
	}
	if _, ok := AccountLastDistressAt(got); ok {
		t.Fatal("mature account must not be stamped with last_distress_at through MarkResult")
	}
}

func TestManagerMarkResult_HealthGateNoConfigNoOp(t *testing.T) {
	// A manager whose runtime config was never set (bare NewManager) must never
	// write a health cap -- MarkResult behavior is unchanged pre-SetConfig.
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	a := &Auth{ID: "claude-noconf", Provider: "claude", Metadata: map[string]any{}}
	// Warming account (ageIdx 4): so the no-op is attributable to the missing runtime
	// config, not to Fix 1's mature exclusion.
	a.SetAccountFirstProductionAt(time.Now().Add(-50 * 24 * time.Hour))
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	for i := 0; i < 5; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "claude-noconf", Provider: "claude", Model: "claude-sonnet-4", Success: false, Error: planQuotaError()})
	}
	got, _ := mgr.GetByID("claude-noconf")
	if _, ok := AccountHealthStageCap(got); ok {
		t.Fatal("no runtime config -> health gate must be a no-op (no cap written)")
	}
}
