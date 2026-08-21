package auth

import (
	"testing"
	"time"
)

// TestNextRefreshCheckAt_ProvisionedGate covers the R5-3e auto-refresh scheduler
// fail-closed gate: an enrolled-but-unprovisioned Claude account must be
// unscheduled (never auto-refreshed) once FARM_REQUIRE_PROVISIONED is armed,
// while every immune population keeps its normal schedule byte-identically:
//   - flag off (the account is scheduled exactly as today),
//   - unenrolled / pre-existing "old" accounts (never opted into the farm),
//   - non-Claude providers (no claude_device_id binding concept),
//   - enrolled-AND-provisioned accounts (a real container binding is present).
//
// Each schedulable case pins NextRefreshAfter in the future so nextRefreshCheckAt
// returns that exact time; the gate short-circuits BEFORE that branch, so a
// blocked account returns (zero, false) instead.
func TestNextRefreshCheckAt_ProvisionedGate(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	interval := time.Minute

	// enrolledUnprovisioned is a schedulable enrolled Claude account carrying only
	// the synthetic device_id (no valid override binding): the exact population the
	// gate fail-closes when armed.
	enrolledUnprovisioned := func() *Auth {
		a := claudeAuthEnrolledWithOverride("")
		a.NextRefreshAfter = future
		return a
	}

	t.Run("flag off: enrolled+unprovisioned stays scheduled (byte-identical)", func(t *testing.T) {
		t.Setenv(FarmRequireProvisionedEnvVar, "")
		next, ok := nextRefreshCheckAt(now, enrolledUnprovisioned(), interval)
		if !ok || !next.Equal(future) {
			t.Fatalf("nextRefreshCheckAt = (%v, %v), want (%v, true) with flag off", next, ok, future)
		}
	})

	t.Run("flag on: enrolled+unprovisioned is unscheduled (fail-closed)", func(t *testing.T) {
		t.Setenv(FarmRequireProvisionedEnvVar, "1")
		next, ok := nextRefreshCheckAt(now, enrolledUnprovisioned(), interval)
		if ok || !next.IsZero() {
			t.Fatalf("nextRefreshCheckAt = (%v, %v), want (zero, false) — enrolled+unprovisioned must not auto-refresh", next, ok)
		}
	})

	t.Run("flag on: unenrolled old account stays scheduled (immune)", func(t *testing.T) {
		t.Setenv(FarmRequireProvisionedEnvVar, "1")
		a := claudeAuthWithOverride("") // never farm-enrolled
		a.NextRefreshAfter = future
		next, ok := nextRefreshCheckAt(now, a, interval)
		if !ok || !next.Equal(future) {
			t.Fatalf("nextRefreshCheckAt = (%v, %v), want (%v, true) — old accounts must stay immune", next, ok, future)
		}
	})

	t.Run("flag on: non-Claude stays scheduled (immune)", func(t *testing.T) {
		t.Setenv(FarmRequireProvisionedEnvVar, "1")
		a := &Auth{ID: "codex-acct", Provider: "codex", Status: StatusActive, NextRefreshAfter: future}
		next, ok := nextRefreshCheckAt(now, a, interval)
		if !ok || !next.Equal(future) {
			t.Fatalf("nextRefreshCheckAt = (%v, %v), want (%v, true) — non-Claude must stay immune", next, ok, future)
		}
	})

	t.Run("flag on: enrolled+provisioned stays scheduled (servable/recovery)", func(t *testing.T) {
		t.Setenv(FarmRequireProvisionedEnvVar, "1")
		a := claudeAuthEnrolledWithOverride(validProvisionedDeviceID)
		a.NextRefreshAfter = future
		next, ok := nextRefreshCheckAt(now, a, interval)
		if !ok || !next.Equal(future) {
			t.Fatalf("nextRefreshCheckAt = (%v, %v), want (%v, true) — provisioned account is refreshable", next, ok, future)
		}
	})
}
