package auth

import (
	"sync"
	"time"
)

// AccountRateLimiter is the per-account outbound rate smoother for the
// adaptive account-scheduling change
// (openspec/changes/add-adaptive-account-scheduling, spec.md "每账号限流平滑（非全局池）",
// design.md D2). It maintains one independent token bucket per upstream
// account (keyed by authID) so that a short burst aimed at a single account is
// smoothed to that account's own tier-aware ceiling BEFORE the request goes
// out -- complementing, not replacing, the existing passive 429 cooldown.
//
// Design intent this type deliberately encodes:
//
//   - Per-account, never a global pool (design D2): each authID has its own
//     bucket, so one client hammering one account can never push the pool's
//     other accounts toward cooldown, and a mature account is never throttled
//     just because the fleet as a whole is busy. There is intentionally no
//     process-wide token ceiling here.
//
//   - Mechanism only, policy stays with the caller (NOCLASH slice boundary):
//     the per-call rpm/burst limits are passed IN by the caller, which derives
//     them from the account's tier + warm-up stage (Phase 1/3 wiring, config
//     package -- NOT this file). This limiter owns only the token-bucket math
//     and its concurrency safety. That decoupling is why Allow takes raw
//     rpm/burst rather than any config struct: limits can change call to call
//     (a tier upgrade/downgrade, or an account crossing a warm-up boundary)
//     and the bucket transparently adopts the new ceiling on the next Allow.
//
//   - In-memory, no persistence (design §6.2): token counts are inherently
//     short-lived and self-refilling. On a process restart every bucket is
//     rebuilt lazily and can never carry more than `burst` accumulated tokens,
//     so a restart is a fail-safe direction (you can never save up an
//     unbounded burst across an idle period + restart), which is exactly why
//     §6.2 classifies this state as "safe to lose on restart, no DB needed".
//
// All exported methods are safe for concurrent use by many goroutines; every
// access to the bucket map and to any bucket's fields is serialized by a
// single mutex (see the concurrency note on AccountRateLimiter.mu).
type AccountRateLimiter struct {
	// mu guards buckets and every field of every *accountBucket it holds, plus
	// the background-loop lifecycle fields (loopRunning/stopCh). A single mutex
	// is used deliberately: correctness/race-freedom is the priority for this
	// slice (design calls it the concurrency-critical piece), and no bucket is
	// ever read or written outside this lock, which makes the type race-free by
	// construction. Sharding for throughput can be layered on later without
	// changing the public contract.
	mu      sync.Mutex
	buckets map[string]*accountBucket

	// now is the clock, injected once at construction and never reassigned, so
	// it is safe to read from multiple goroutines without holding mu. It exists
	// solely so tests can drive refill/reclaim deterministically with a fixed
	// clock instead of asserting against a naked time.Now(); production uses
	// time.Now.
	now func() time.Time

	// idleTTL is how long a bucket may go untouched before ReclaimIdle (and the
	// optional background loop) is allowed to evict it, bounding the map so a
	// churn of short-lived accounts cannot leak memory. Zero disables reclaim.
	idleTTL time.Duration

	loopRunning bool
	stopCh      chan struct{}
}

// accountBucket is one account's token-bucket state. It stores ONLY the
// accumulated token count and two timestamps; the bucket's capacity (burst)
// and refill rate (rpm) are intentionally NOT stored here -- they are supplied
// fresh on every Allow call so a tier/warm-up limit change takes effect
// immediately without any per-bucket reconfiguration step.
type accountBucket struct {
	// tokens is the current fractional token count. One whole token is spent
	// per allowed request.
	tokens float64
	// last is the wall-clock instant tokens were last refilled up to.
	last time.Time
	// lastSeen is the wall-clock instant this bucket was last touched by an
	// Allow call, used only to decide idle reclamation.
	lastSeen time.Time
}

const defaultAccountRateLimiterIdleTTL = 30 * time.Minute

// AccountRateLimiterOption customizes a limiter at construction.
type AccountRateLimiterOption func(*AccountRateLimiter)

// WithClock injects the clock the limiter reads for refill and reclaim math.
// The supplied function MUST be safe to call from multiple goroutines
// concurrently (production passes time.Now, which is; tests must guard a mock
// clock). Passing nil is ignored and leaves the default (time.Now) in place.
func WithClock(now func() time.Time) AccountRateLimiterOption {
	return func(l *AccountRateLimiter) {
		if now != nil {
			l.now = now
		}
	}
}

// WithIdleTTL sets how long a bucket may be untouched before it becomes
// eligible for idle reclamation. A non-positive value disables reclaim (buckets
// live until the limiter is dropped); the default is 30 minutes.
func WithIdleTTL(ttl time.Duration) AccountRateLimiterOption {
	return func(l *AccountRateLimiter) {
		l.idleTTL = ttl
	}
}

// NewAccountRateLimiter builds a limiter with no buckets yet. By default it
// reads time.Now and reclaims buckets idle for longer than 30 minutes; both
// are overridable via options. It does NOT start any background goroutine --
// call StartReclaimLoop if periodic automatic reclamation is wanted, otherwise
// call ReclaimIdle on the caller's own cadence.
func NewAccountRateLimiter(opts ...AccountRateLimiterOption) *AccountRateLimiter {
	l := &AccountRateLimiter{
		buckets: make(map[string]*accountBucket),
		now:     time.Now,
		idleTTL: defaultAccountRateLimiterIdleTTL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// Allow reports whether a single request for the given account may proceed
// right now under a token bucket whose capacity is `burst` and whose refill
// rate is `rpm` requests per minute. When it returns true it has consumed one
// token; when it returns false the account is over its instantaneous ceiling
// and the caller should smooth (delay / de-weight / pick another account) --
// this limiter never blocks or sleeps, it only reports.
//
// Contract for the mechanism (policy -- what rpm/burst to pass -- is the
// caller's, see the type doc):
//
//   - authID == "" means "no account identity to key on"; there is nothing to
//     rate-limit, so it returns true and creates no bucket (no leak).
//
//   - rpm <= 0 means "no rpm ceiling configured for this account/stage"; it
//     returns true without touching or creating a bucket. (The default
//     warm-up/mature curves always configure a positive rpm, so this is a
//     defensive path, not a normal one.)
//
//   - burst < 1 is clamped up to a capacity of 1, so a misconfigured zero
//     burst still lets a lone request through and refills normally rather than
//     wedging the account at a permanent deny.
//
//   - A brand-new bucket starts FULL (capacity tokens). `burst` is by
//     definition the safe burst allowance the caller configured for this
//     tier/stage, so allowing up to `burst` immediate requests is within the
//     configured envelope, and the very first request to a freshly seen
//     account is never spuriously denied. Because tokens are capped at
//     capacity, a full cold start can still never exceed the configured burst.
//
// Limit changes across calls are honored immediately: if `burst` shrinks
// between calls (e.g. a tier downgrade) any tokens above the new capacity are
// clamped away; if it grows, the bucket simply refills toward the larger
// capacity from then on.
func (l *AccountRateLimiter) Allow(authID string, rpm float64, burst int) bool {
	if authID == "" || rpm <= 0 {
		return true
	}

	capacity := float64(burst)
	if capacity < 1 {
		capacity = 1
	}
	ratePerSec := rpm / 60.0

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[authID]
	if !ok {
		b = &accountBucket{tokens: capacity, last: now, lastSeen: now}
		l.buckets[authID] = b
	}
	b.lastSeen = now

	// Refill for elapsed real time since the last refill, capped at capacity.
	// A non-advancing or backwards clock (elapsed <= 0) adds nothing and leaves
	// b.last untouched, so time can never run backwards for the bucket.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * ratePerSec
		b.last = now
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}

	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// ReclaimIdle evicts every bucket that has not been touched by an Allow call
// within idleTTL (as measured by the injected clock) and returns how many were
// removed. It is a no-op returning 0 when idleTTL is non-positive. Call it
// periodically (or wire StartReclaimLoop) to keep the bucket map bounded when
// the set of live accounts churns.
func (l *AccountRateLimiter) ReclaimIdle() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idleTTL <= 0 {
		return 0
	}
	now := l.now()
	removed := 0
	for id, b := range l.buckets {
		if now.Sub(b.lastSeen) >= l.idleTTL {
			delete(l.buckets, id)
			removed++
		}
	}
	return removed
}

// StartReclaimLoop starts a background goroutine that calls ReclaimIdle every
// `interval`. It is safe to call concurrently and is a no-op if a loop is
// already running or if interval <= 0. Pair it with Stop to shut the goroutine
// down; a limiter with no loop needs no Stop. The loop uses a real-time ticker
// for its cadence but compares bucket idleness against the injected clock, so
// tests should prefer driving ReclaimIdle directly for determinism.
func (l *AccountRateLimiter) StartReclaimLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	l.mu.Lock()
	if l.loopRunning {
		l.mu.Unlock()
		return
	}
	l.loopRunning = true
	l.stopCh = make(chan struct{})
	stop := l.stopCh
	l.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				l.ReclaimIdle()
			case <-stop:
				return
			}
		}
	}()
}

// Stop halts the background reclaim loop started by StartReclaimLoop. It is
// safe to call multiple times and safe to call when no loop is running.
func (l *AccountRateLimiter) Stop() {
	l.mu.Lock()
	if l.loopRunning {
		close(l.stopCh)
		l.loopRunning = false
	}
	l.mu.Unlock()
}

// Len returns the number of live buckets currently held. It exists for
// observability and tests (e.g. asserting a reclaim actually shrank the map);
// it is not part of the rate-limiting decision path.
func (l *AccountRateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
