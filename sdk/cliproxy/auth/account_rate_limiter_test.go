package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockClock is a concurrency-safe controllable clock. It is safe to read via
// now() from a background goroutine while a test advances it, so the limiter's
// injected clock never triggers -race even in the concurrency test.
type mockClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMockClock(start time.Time) *mockClock { return &mockClock{t: start} }

func (c *mockClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mockClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fixedBase is an arbitrary fixed instant so no assertion depends on the real
// wall clock.
var fixedBase = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// countAllows calls Allow n times back to back (clock not advanced by the
// caller) and returns how many were permitted.
func countAllows(l *AccountRateLimiter, authID string, rpm float64, burst, n int) int {
	allowed := 0
	for i := 0; i < n; i++ {
		if l.Allow(authID, rpm, burst) {
			allowed++
		}
	}
	return allowed
}

func TestAccountRateLimiter_SingleAccountBurstThenRefill(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	// Fresh bucket starts full: burst=5, rpm=60 => 1 token/sec refill.
	// Ten instantaneous requests: exactly the 5-token burst passes, the rest
	// are smoothed away.
	if got := countAllows(l, "acct", 60, 5, 10); got != 5 {
		t.Fatalf("initial burst: allowed=%d, want 5 (capacity)", got)
	}

	// No time elapsed => still empty.
	if l.Allow("acct", 60, 5) {
		t.Fatalf("expected deny while bucket empty and clock not advanced")
	}

	// One second refills exactly one token (rpm=60).
	clk.advance(1 * time.Second)
	if !l.Allow("acct", 60, 5) {
		t.Fatalf("expected allow after 1s refill (one token)")
	}
	if l.Allow("acct", 60, 5) {
		t.Fatalf("expected deny after consuming the single refilled token")
	}

	// A long idle refills only up to capacity, never above burst.
	clk.advance(1 * time.Hour)
	if got := countAllows(l, "acct", 60, 5, 10); got != 5 {
		t.Fatalf("post-idle burst: allowed=%d, want 5 (capped at capacity, no overfill)", got)
	}
}

func TestAccountRateLimiter_MultiAccountIsolation(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	// Drain account A completely.
	if got := countAllows(l, "A", 60, 2, 5); got != 2 {
		t.Fatalf("A burst: allowed=%d, want 2", got)
	}
	if l.Allow("A", 60, 2) {
		t.Fatalf("A should be drained")
	}

	// Account B must be entirely unaffected by A's exhaustion.
	if got := countAllows(l, "B", 60, 2, 5); got != 2 {
		t.Fatalf("B burst after A drained: allowed=%d, want 2 (no cross-account leakage)", got)
	}

	if l.Len() != 2 {
		t.Fatalf("expected 2 buckets, got %d", l.Len())
	}
}

func TestAccountRateLimiter_UnlimitedWhenRPMNonPositive(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	for i := 0; i < 1000; i++ {
		if !l.Allow("acct", 0, 5) {
			t.Fatalf("rpm<=0 must always allow (iter %d)", i)
		}
	}
	// No bucket is created for an unlimited account -> no map leak.
	if l.Len() != 0 {
		t.Fatalf("rpm<=0 must not create a bucket, got Len=%d", l.Len())
	}

	// Negative rpm behaves the same.
	if !l.Allow("acct", -5, 5) {
		t.Fatalf("negative rpm must allow")
	}
	if l.Len() != 0 {
		t.Fatalf("negative rpm must not create a bucket, got Len=%d", l.Len())
	}
}

func TestAccountRateLimiter_EmptyAuthIDAllowedNoBucket(t *testing.T) {
	l := NewAccountRateLimiter(WithClock(newMockClock(fixedBase).now))
	for i := 0; i < 100; i++ {
		if !l.Allow("", 1, 1) {
			t.Fatalf("empty authID must always allow (iter %d)", i)
		}
	}
	if l.Len() != 0 {
		t.Fatalf("empty authID must not create a bucket, got Len=%d", l.Len())
	}
}

func TestAccountRateLimiter_BurstClampedToMinimum(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	// burst=0 is clamped to capacity 1: exactly one request passes, then deny.
	if !l.Allow("acct", 60, 0) {
		t.Fatalf("first request must pass even with burst=0 (clamped to 1)")
	}
	if l.Allow("acct", 60, 0) {
		t.Fatalf("second immediate request must be denied (capacity clamped to 1)")
	}

	// Refill works against the clamped capacity.
	clk.advance(1 * time.Second)
	if !l.Allow("acct", 60, 0) {
		t.Fatalf("request must pass after 1s refill")
	}
}

func TestAccountRateLimiter_LowRPMWarmupPacing(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	// Warm-up W1 shape: rpm=3 (one token every 20s), burst=1.
	if !l.Allow("young", 3, 1) {
		t.Fatalf("first warm-up request must pass (full cold start)")
	}
	if l.Allow("young", 3, 1) {
		t.Fatalf("second immediate request must be smoothed (rpm=3, burst=1)")
	}
	// 19s is not enough to refill one token at 3 rpm.
	clk.advance(19 * time.Second)
	if l.Allow("young", 3, 1) {
		t.Fatalf("request at 19s must still be denied (need 20s for one token)")
	}
	// Crossing 20s total refills the token.
	clk.advance(1 * time.Second)
	if !l.Allow("young", 3, 1) {
		t.Fatalf("request at 20s must pass (one token refilled)")
	}
}

func TestAccountRateLimiter_TierDowngradeClampsTokens(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now))

	// Establish a bucket with a large burst but consume nothing beyond one.
	if !l.Allow("acct", 60, 10) {
		t.Fatalf("first request under burst=10 must pass")
	}
	// Bucket now holds ~9 tokens. A downgrade to burst=2 must clamp available
	// tokens down to the new capacity, so at most 2 immediate requests pass.
	if got := countAllows(l, "acct", 60, 2, 10); got != 2 {
		t.Fatalf("after downgrade to burst=2: allowed=%d, want 2 (tokens clamped to new capacity)", got)
	}
}

func TestAccountRateLimiter_ReclaimIdle(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now), WithIdleTTL(10*time.Minute))

	// Create buckets A and B at t0.
	l.Allow("A", 60, 5)
	l.Allow("B", 60, 5)
	if l.Len() != 2 {
		t.Fatalf("expected 2 buckets, got %d", l.Len())
	}

	// At t0+5m, touch A only (refreshes A.lastSeen, leaves B idle since t0).
	clk.advance(5 * time.Minute)
	l.Allow("A", 60, 5)

	// At t0+11m: A idle 6m (< 10m, kept), B idle 11m (>= 10m, evicted).
	clk.advance(6 * time.Minute)
	if removed := l.ReclaimIdle(); removed != 1 {
		t.Fatalf("ReclaimIdle removed=%d, want 1 (only B)", removed)
	}
	if l.Len() != 1 {
		t.Fatalf("expected 1 bucket after reclaim, got %d", l.Len())
	}
	// A must still function (proving the survivor is intact, not resurrected).
	if !l.Allow("A", 60, 5) {
		t.Fatalf("survivor A must still allow")
	}

	// Advance well past idleTTL with no touches: everything reclaimed.
	clk.advance(30 * time.Minute)
	if removed := l.ReclaimIdle(); removed != 1 {
		t.Fatalf("second ReclaimIdle removed=%d, want 1 (A)", removed)
	}
	if l.Len() != 0 {
		t.Fatalf("expected 0 buckets, got %d", l.Len())
	}
}

func TestAccountRateLimiter_ReclaimDisabledWhenTTLNonPositive(t *testing.T) {
	clk := newMockClock(fixedBase)
	l := NewAccountRateLimiter(WithClock(clk.now), WithIdleTTL(0))
	l.Allow("A", 60, 5)
	clk.advance(24 * time.Hour)
	if removed := l.ReclaimIdle(); removed != 0 {
		t.Fatalf("reclaim must be a no-op when idleTTL<=0, removed=%d", removed)
	}
	if l.Len() != 1 {
		t.Fatalf("bucket must survive with reclaim disabled, Len=%d", l.Len())
	}
}

func TestAccountRateLimiter_StopIsSafeWithoutLoop(t *testing.T) {
	l := NewAccountRateLimiter()
	// Stop with no loop running, and StartReclaimLoop with a non-positive
	// interval, must both be safe no-ops.
	l.Stop()
	l.StartReclaimLoop(0)
	l.Stop()
}

// TestAccountRateLimiter_ConcurrentNoRace is the -race gate. Many goroutines
// hammer Allow across a small account set while a background reclaim loop and
// direct ReclaimIdle/Len calls run concurrently. It asserts only race-freedom
// and liveness (some requests pass), never exact token math, so it does not
// depend on real-time timing.
func TestAccountRateLimiter_ConcurrentNoRace(t *testing.T) {
	l := NewAccountRateLimiter(WithIdleTTL(50 * time.Millisecond))
	l.StartReclaimLoop(1 * time.Millisecond)
	defer l.Stop()

	const (
		goroutines = 32
		perG       = 500
		accounts   = 4
	)
	authIDs := []string{"acct-0", "acct-1", "acct-2", "acct-3"}

	var allowed int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				id := authIDs[(g+i)%accounts]
				if l.Allow(id, 600, 10) {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}(g)
	}

	// Concurrent reader/reclaimer to exercise the map under contention.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < perG; i++ {
			_ = l.Len()
			l.ReclaimIdle()
		}
	}()

	wg.Wait()

	if atomic.LoadInt64(&allowed) == 0 {
		t.Fatalf("expected some requests to be allowed under concurrency")
	}
}
