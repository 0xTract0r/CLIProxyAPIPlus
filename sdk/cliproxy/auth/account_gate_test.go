package auth

import (
	"sync"
	"testing"
	"time"
)

// gateFixedClock returns a clock pinned to a fixed instant so daily-budget UTC
// math is deterministic and free of naked time.Now.
func gateFixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestAccountConcurrencyGateAcquireReportsLimit verifies Acquire always records
// the slot but only reports within-limit while the post-increment count is <=
// limit, and that a released slot re-opens headroom.
func TestAccountConcurrencyGateAcquireReportsLimit(t *testing.T) {
	g := NewAccountConcurrencyGate(gateClockOpt())

	// limit 2: first two acquires are within limit, the third exceeds.
	if ok := g.Acquire("a", 2); !ok {
		t.Fatalf("Acquire #1 = false, want true (within limit 2)")
	}
	if ok := g.Acquire("a", 2); !ok {
		t.Fatalf("Acquire #2 = false, want true (within limit 2)")
	}
	if ok := g.Acquire("a", 2); ok {
		t.Fatalf("Acquire #3 = true, want false (over limit 2)")
	}
	if got := g.InFlight("a"); got != 3 {
		t.Fatalf("InFlight after 3 acquires = %d, want 3 (Acquire always records)", got)
	}

	// Release the over-limit slot: back within limit, next acquire is within.
	g.Release("a")
	if got := g.InFlight("a"); got != 2 {
		t.Fatalf("InFlight after one release = %d, want 2", got)
	}
	g.Release("a")
	if ok := g.Acquire("a", 2); !ok {
		t.Fatalf("Acquire after releases = false, want true (headroom restored)")
	}
}

// TestAccountConcurrencyGateLimitZeroUnbounded verifies a non-positive limit is
// treated as "no ceiling": the slot is still tracked (so InFlight stays
// accurate) but Acquire never reports over-limit.
func TestAccountConcurrencyGateLimitZeroUnbounded(t *testing.T) {
	g := NewAccountConcurrencyGate(gateClockOpt())
	for i := 0; i < 5; i++ {
		if ok := g.Acquire("a", 0); !ok {
			t.Fatalf("Acquire #%d with limit 0 = false, want true (unbounded)", i)
		}
	}
	if got := g.InFlight("a"); got != 5 {
		t.Fatalf("InFlight = %d, want 5 (tracked even when unbounded)", got)
	}
}

// TestAccountConcurrencyGateReleaseFloorsAndReclaims verifies Release never
// drives the count negative and deletes the entry at zero (bounding the map),
// and that an unknown / empty authID is a harmless no-op.
func TestAccountConcurrencyGateReleaseFloorsAndReclaims(t *testing.T) {
	g := NewAccountConcurrencyGate(gateClockOpt())
	g.Acquire("a", 4)
	g.Release("a")
	// Extra releases must not underflow.
	g.Release("a")
	g.Release("a")
	if got := g.InFlight("a"); got != 0 {
		t.Fatalf("InFlight after over-release = %d, want 0 (floored)", got)
	}
	if _, ok := g.inflight["a"]; ok {
		t.Fatalf("inflight entry for a not reclaimed at zero")
	}

	// Empty authID: Acquire returns true, records nothing; Release/InFlight safe.
	if ok := g.Acquire("", 1); !ok {
		t.Fatalf("Acquire(\"\") = false, want true")
	}
	g.Release("")
	if got := g.InFlight(""); got != 0 {
		t.Fatalf("InFlight(\"\") = %d, want 0", got)
	}
	if len(g.inflight) != 0 {
		t.Fatalf("inflight map non-empty after empty-authID ops: %v", g.inflight)
	}
}

// TestAccountConcurrencyGateConcurrentNoLeak stress-tests Acquire/Release from
// many goroutines against several accounts. With -race it also proves the type
// is race-free. Every acquire is paired with a release, so every account's
// final in-flight count must be exactly 0 (no leaked slot) and every map entry
// must have been reclaimed.
func TestAccountConcurrencyGateConcurrentNoLeak(t *testing.T) {
	g := NewAccountConcurrencyGate(gateClockOpt())
	accounts := []string{"a", "b", "c", "d"}
	const goroutines = 64
	const iterations = 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for gi := 0; gi < goroutines; gi++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := accounts[(seed+i)%len(accounts)]
				// Acquire then release, always paired -- mirrors the execution
				// path's defer release. Interleave a read to race the maps.
				g.Acquire(id, 3)
				_ = g.InFlight(id)
				g.Release(id)
			}
		}(gi)
	}
	wg.Wait()

	for _, id := range accounts {
		if got := g.InFlight(id); got != 0 {
			t.Fatalf("InFlight(%q) = %d after all paired release, want 0 (slot leak)", id, got)
		}
	}
	if len(g.inflight) != 0 {
		t.Fatalf("inflight map not fully reclaimed: %v", g.inflight)
	}
}

// TestAccountDailyBudgetRecordAndCount verifies RecordRequest increments the
// current-day counter and OverDailyBudget crosses at the configured budget.
func TestAccountDailyBudgetRecordAndCount(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewAccountConcurrencyGate(WithGateClock(gateFixedClock(at)))

	for i := 0; i < 3; i++ {
		g.RecordRequest("a")
	}
	if got := g.DailyCount("a"); got != 3 {
		t.Fatalf("DailyCount = %d, want 3", got)
	}
	if g.OverDailyBudget("a", 5) {
		t.Fatalf("OverDailyBudget(5) = true at count 3, want false")
	}
	g.RecordRequest("a")
	g.RecordRequest("a")
	if got := g.DailyCount("a"); got != 5 {
		t.Fatalf("DailyCount = %d, want 5", got)
	}
	if !g.OverDailyBudget("a", 5) {
		t.Fatalf("OverDailyBudget(5) = false at count 5, want true (>= budget)")
	}
}

// TestAccountDailyBudgetUnbounded verifies a non-positive budget is treated as
// unbounded (mature accounts, design §5.1) regardless of count.
func TestAccountDailyBudgetUnbounded(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewAccountConcurrencyGate(WithGateClock(gateFixedClock(at)))
	for i := 0; i < 10; i++ {
		g.RecordRequest("a")
	}
	if g.OverDailyBudget("a", 0) {
		t.Fatalf("OverDailyBudget(0) = true, want false (unbounded)")
	}
	if g.OverDailyBudget("a", -1) {
		t.Fatalf("OverDailyBudget(-1) = true, want false (unbounded)")
	}
}

// TestAccountDailyBudgetUTCDayReset verifies the daily counter resets on a UTC
// day boundary: a request just before midnight and one just after fall in
// different day buckets, and yesterday's count reads as 0 today.
func TestAccountDailyBudgetUTCDayReset(t *testing.T) {
	now := time.Date(2026, 9, 1, 23, 59, 30, 0, time.UTC)
	g := NewAccountConcurrencyGate(WithGateClock(func() time.Time { return now }))

	g.RecordRequest("a")
	g.RecordRequest("a")
	if got := g.DailyCount("a"); got != 2 {
		t.Fatalf("DailyCount day1 = %d, want 2", got)
	}
	if !g.OverDailyBudget("a", 2) {
		t.Fatalf("OverDailyBudget(2) day1 = false, want true")
	}

	// Advance past UTC midnight into the next day.
	now = time.Date(2026, 9, 2, 0, 0, 30, 0, time.UTC)
	if got := g.DailyCount("a"); got != 0 {
		t.Fatalf("DailyCount day2 (pre-record) = %d, want 0 (reset on new UTC day)", got)
	}
	if g.OverDailyBudget("a", 2) {
		t.Fatalf("OverDailyBudget(2) day2 = true, want false (budget reset)")
	}
	g.RecordRequest("a")
	if got := g.DailyCount("a"); got != 1 {
		t.Fatalf("DailyCount day2 (post-record) = %d, want 1", got)
	}
}

// gateClockOpt pins the gate clock to a fixed instant for tests that do not
// exercise the day rollover (keeps concurrency tests free of naked time.Now).
func gateClockOpt() AccountConcurrencyGateOption {
	return WithGateClock(gateFixedClock(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)))
}
