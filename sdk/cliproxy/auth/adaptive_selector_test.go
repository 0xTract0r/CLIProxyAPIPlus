package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Compile-time contract checks: AdaptiveSelector must satisfy the Selector and
// StoppableSelector interfaces, and expose the InvalidateAuth shape the auth
// Manager asserts on (conductor_lifecycle.go).
var (
	_ Selector                         = (*AdaptiveSelector)(nil)
	_ StoppableSelector                = (*AdaptiveSelector)(nil)
	_ interface{ InvalidateAuth(string) } = (*AdaptiveSelector)(nil)
)

// adaptiveTestNow is a fixed instant used across these tests so token-bucket
// refill and warm-up age math are deterministic (no naked time.Now).
var adaptiveTestNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// newAdaptiveClaudeAuth builds an available Claude *Auth with the given
// fine-grained tier (raw rate_limit_tier string) and first-production anchor.
// A zero firstProd leaves the account un-anchored.
func newAdaptiveClaudeAuth(id, rateLimitTier string, firstProd time.Time) *Auth {
	a := &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{},
	}
	if rateLimitTier != "" {
		a.Metadata["quota_snapshot"] = map[string]any{
			"profile": map[string]any{
				"organization": map[string]any{
					"rate_limit_tier": rateLimitTier,
				},
			},
		}
	}
	if !firstProd.IsZero() {
		a.Metadata[FirstProductionAtMetadataKey] = firstProd.UTC().Format(time.RFC3339)
	}
	return a
}

// constRand returns an rng function that always yields v (in [0,1)).
func constRand(v float64) func() float64 {
	return func() float64 { return v }
}

// fixedClock returns a clock function pinned to adaptiveTestNow.
func fixedClock() func() time.Time {
	return func() time.Time { return adaptiveTestNow }
}

func matureFirstProd() time.Time { return adaptiveTestNow.Add(-90 * 24 * time.Hour) }
func warmupFirstProd() time.Time { return adaptiveTestNow.Add(-2 * 24 * time.Hour) }

// TestAdaptiveSelectorWeightedByTier verifies distribution follows tier capacity
// weight: a Max 20x account (base 20) vs a Max 5x account (base 5), both mature
// so freshness == 1. With a low rng draw the pick lands in the larger (20x)
// weight bucket; with a high rng draw it lands in the smaller (5x) bucket.
func TestAdaptiveSelectorWeightedByTier(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	max20 := newAdaptiveClaudeAuth("a-20x", "default_claude_max_20x", matureFirstProd())
	max5 := newAdaptiveClaudeAuth("b-5x", "default_claude_max_5x", matureFirstProd())
	auths := []*Auth{max20, max5}

	// weights: a-20x = 20*0.5*1 = 10, b-5x = 5*0.5*1 = 2.5, total 12.5.
	// rng 0.1 -> target 1.25 < 10 -> a-20x. rng 0.95 -> target 11.875,
	// crosses into b-5x (accumulated 10 then 12.5).
	cases := []struct {
		name string
		rng  float64
		want string
	}{
		{"low draw picks 20x", 0.1, "a-20x"},
		{"high draw picks 5x", 0.95, "b-5x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewAdaptiveSelector(
				AdaptiveSelectorConfig{Scheduling: cfg},
				WithAdaptiveClock(fixedClock()),
				WithAdaptiveRand(constRand(tc.rng)),
			)
			defer s.Stop()
			got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("Pick returned error: %v", err)
			}
			if got == nil || got.ID != tc.want {
				t.Fatalf("Pick = %v, want %s", authID(got), tc.want)
			}
		})
	}
}

// TestAdaptiveSelectorSkipsRateLimitedAccount verifies a candidate whose token
// bucket is exhausted is skipped in favour of the next weighted candidate
// during selection (design D2 / task 2.2: rate-limited accounts are de-weighted/
// skipped at selection time, not handed the request to 429 afterwards).
func TestAdaptiveSelectorSkipsRateLimitedAccount(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	drained := newAdaptiveClaudeAuth("a", "default_claude_max_20x", matureFirstProd())
	fresh := newAdaptiveClaudeAuth("b", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{drained, fresh}

	limiter := NewAccountRateLimiter(WithClock(fixedClock()))
	// Drain "a" fully at the fixed instant (mature cap = burst 10).
	for i := 0; i < cfg.MatureLimits.Burst; i++ {
		if !limiter.Allow("a", float64(cfg.MatureLimits.RPMLimit), cfg.MatureLimits.Burst) {
			t.Fatalf("pre-drain Allow #%d unexpectedly denied", i)
		}
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // would target the first (a) bucket
		WithAdaptiveRateLimiter(limiter),
	)
	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if got == nil || got.ID != "b" {
		t.Fatalf("Pick = %v, want b (a is rate-limited)", authID(got))
	}
}

// TestAdaptiveSelectorFloodRoutesToMature verifies a burst against a warming
// account is smoothed onto a mature account: the warming account (w1, rpm 3,
// burst 1) serves at most its single bucketed request, after which every pick
// routes to the mature account (design D4 / task 3.2 -- flood routing to mature
// falls out of low warm-up weight + a tiny warm-up rpm ceiling, no explicit
// flood detector).
func TestAdaptiveSelectorFloodRoutesToMature(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // always targets the first (a-warm) bucket
	)
	defer s.Stop()

	const picks = 6
	counts := map[string]int{}
	for i := 0; i < picks; i++ {
		got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick #%d returned error: %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick #%d returned nil", i)
		}
		counts[got.ID]++
		if i > 0 && got.ID != "b-mature" {
			t.Fatalf("Pick #%d = %s, want b-mature (warm bucket already drained)", i, got.ID)
		}
	}
	if counts["a-warm"] != 1 {
		t.Fatalf("warm account served %d requests, want exactly 1 (its bucketed allowance)", counts["a-warm"])
	}
	if counts["b-mature"] != picks-1 {
		t.Fatalf("mature account served %d requests, want %d", counts["b-mature"], picks-1)
	}
}

// TestAdaptiveSelectorNonAdaptiveProviderFallsBack verifies a provider this
// scheduler has no tier weight for (e.g. gemini) yields no weighted candidate
// and is served by the wrapped fallback selector unchanged (design D7 backward
// compatibility).
func TestAdaptiveSelectorNonAdaptiveProviderFallsBack(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	g1 := &Auth{ID: "g1", Provider: "gemini", Status: StatusActive}
	g2 := &Auth{ID: "g2", Provider: "gemini", Status: StatusActive}
	auths := []*Auth{g1, g2}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if got == nil || (got.ID != "g1" && got.ID != "g2") {
		t.Fatalf("Pick = %v, want one of the gemini accounts via fallback", authID(got))
	}
}

// TestAdaptiveSelectorStickyMatureKeepsBinding verifies a session bound to a
// mature account within its soft ceiling keeps that account across turns
// (spec.md "成熟号软上限内保持粘性").
func TestAdaptiveSelectorStickyMatureKeepsBinding(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := newAdaptiveClaudeAuth("a", "default_claude_max_20x", matureFirstProd())
	b := newAdaptiveClaudeAuth("b", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{a, b}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // first bind targets "a"
	)
	defer s.Stop()

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"s1"}}}
	first, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("first Pick error: %v", err)
	}
	if first.ID != "a" {
		t.Fatalf("first Pick = %s, want a", first.ID)
	}
	second, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("second Pick error: %v", err)
	}
	if second.ID != "a" {
		t.Fatalf("second Pick = %s, want a (sticky mature within soft ceiling)", second.ID)
	}
}

// TestAdaptiveSelectorStickyWarmupBreaksToMature verifies a session that lands on
// a warming account has its stickiness broken and re-routed to a mature account,
// and that the mature account then holds the session (spec.md "养号号打破粘性改
// 路由成熟号").
func TestAdaptiveSelectorStickyWarmupBreaksToMature(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // first bind targets "a-warm"
	)
	defer s.Stop()

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"s1"}}}
	first, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("first Pick error: %v", err)
	}
	if first.ID != "a-warm" {
		t.Fatalf("first Pick = %s, want a-warm", first.ID)
	}
	second, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("second Pick error: %v", err)
	}
	if second.ID != "b-mature" {
		t.Fatalf("second Pick = %s, want b-mature (warm sticky target broken)", second.ID)
	}
	third, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("third Pick error: %v", err)
	}
	if third.ID != "b-mature" {
		t.Fatalf("third Pick = %s, want b-mature (session now sticky to mature)", third.ID)
	}
}

// TestAdaptiveSelectorStickyMatureNearThresholdReselects verifies a session
// bound to a mature account that has hit its ceiling (near the risk hard
// threshold) is re-routed to another account (spec.md "近风控硬阈值才改选").
func TestAdaptiveSelectorStickyMatureNearThresholdReselects(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := newAdaptiveClaudeAuth("a", "default_claude_max_20x", matureFirstProd())
	b := newAdaptiveClaudeAuth("b", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{a, b}

	limiter := NewAccountRateLimiter(WithClock(fixedClock()))
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // first bind targets "a"
		WithAdaptiveRateLimiter(limiter),
	)
	defer s.Stop()

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"s1"}}}
	first, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("first Pick error: %v", err)
	}
	if first.ID != "a" {
		t.Fatalf("first Pick = %s, want a", first.ID)
	}
	// Exhaust the remainder of "a"'s bucket (first Pick already spent 1 token).
	for i := 0; i < cfg.MatureLimits.Burst-1; i++ {
		limiter.Allow("a", float64(cfg.MatureLimits.RPMLimit), cfg.MatureLimits.Burst)
	}
	second, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatalf("second Pick error: %v", err)
	}
	if second.ID != "b" {
		t.Fatalf("second Pick = %s, want b (bound mature account at its ceiling)", second.ID)
	}
}

// TestAdaptiveSelectorNoSessionDoesNotBind verifies that with session affinity
// enabled but no extractable session identity, selection stays purely weighted
// (no stickiness locks the first pick in).
func TestAdaptiveSelectorNoSessionDoesNotBind(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	max20 := newAdaptiveClaudeAuth("a-20x", "default_claude_max_20x", matureFirstProd())
	max5 := newAdaptiveClaudeAuth("b-5x", "default_claude_max_5x", matureFirstProd())
	auths := []*Auth{max20, max5}

	draws := []float64{0.1, 0.95}
	i := 0
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(func() float64 {
			v := draws[i%len(draws)]
			i++
			return v
		}),
	)
	defer s.Stop()

	first, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("first Pick error: %v", err)
	}
	second, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("second Pick error: %v", err)
	}
	if first.ID != "a-20x" {
		t.Fatalf("first Pick = %s, want a-20x (low draw)", first.ID)
	}
	if second.ID != "b-5x" {
		t.Fatalf("second Pick = %s, want b-5x (high draw, no stickiness)", second.ID)
	}
}

// TestAdaptiveSelectorStopIsIdempotent verifies Stop releases resources and can
// be called repeatedly without panicking.
func TestAdaptiveSelectorStopIsIdempotent(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
	)
	s.Stop()
	s.Stop()
}

// TestAdaptiveSelectorPropagatesUnavailableError verifies that when there are no
// available credentials at all, the underlying getAvailableAuths error is
// surfaced rather than swallowed.
func TestAdaptiveSelectorPropagatesUnavailableError(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
	)
	defer s.Stop()
	if _, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); err == nil {
		t.Fatal("Pick with no auths should return an error")
	}
}

// TestAdaptiveSelectorStickyInheritsIntoPrimaryKey is the regression test for the
// first-turn fallback-key inheritance re-bind bug (G3): resolveSticky's two
// "keep the bound account" branches -- non-adaptive target (!adaptiveEligible) and
// mature-within-soft-ceiling target -- must persist the binding under the
// PRIMARY/full session cache key, not only leave it under the short-hash fallback
// key it was inherited from.
//
// White-box rationale: the returned auth alone cannot distinguish the bug from
// the fix in a fast test, because turn 2 re-inherits the same account from the
// still-live fallback key regardless. The observable defect is specifically that
// the primary session key is never populated -- so a long conversation's binding
// lifetime is pinned to the fallback key's original (never-refreshed) TTL and
// jumps accounts once that expires mid-session (spec D5 "成熟号软上限内保持粘性"
// regression). This test therefore asserts directly on the internal cache: after
// the inheritance turn the primary key MUST be bound.
func TestAdaptiveSelectorStickyInheritsIntoPrimaryKey(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()

	// Turn 1: user message only -> extractMessageHashIDs returns (shortHash, "")
	// so the first binding is stored under the short-hash key.
	turn1 := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	// Turn 2+: the same conversation now carries an assistant reply ->
	// (fullHash, shortHash); the fullHash primary key starts unbound and the
	// binding is inherited from the shortHash fallback key.
	turn2 := []byte(`{"messages":[{"role":"user","content":"hello world"},{"role":"assistant","content":"hi there"},{"role":"user","content":"continue"}]}`)

	primary2, fallback2 := extractSessionIDs(nil, turn2, nil)
	if primary2 == "" || fallback2 == "" || primary2 == fallback2 {
		t.Fatalf("test payloads do not exercise the inheritance path: primary=%q fallback=%q", primary2, fallback2)
	}

	cases := []struct {
		name     string
		provider string
		auths    []*Auth
	}{
		{
			// Exercises the mature-within-soft-ceiling branch (adaptiveEligible +
			// isMature + limiter.Allow).
			name:     "mature adaptive target",
			provider: "claude",
			auths:    []*Auth{newAdaptiveClaudeAuth("a", "default_claude_max_20x", matureFirstProd())},
		},
		{
			// Exercises the non-adaptive branch (!adaptiveEligible): a provider
			// with no tier weight binds via the fallback selector and must still be
			// pinned to the primary key on inheritance.
			name:     "non-adaptive target",
			provider: "gemini",
			auths:    []*Auth{{ID: "g1", Provider: "gemini", Status: StatusActive}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primaryKey := tc.provider + "::" + primary2 + "::"
			s := NewAdaptiveSelector(
				AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
				WithAdaptiveClock(fixedClock()),
				WithAdaptiveRand(constRand(0.0)),
			)
			defer s.Stop()

			opts1 := cliproxyexecutor.Options{OriginalRequest: turn1}
			first, err := s.Pick(context.Background(), tc.provider, "", opts1, tc.auths)
			if err != nil {
				t.Fatalf("turn 1 Pick error: %v", err)
			}

			opts2 := cliproxyexecutor.Options{OriginalRequest: turn2}
			second, err := s.Pick(context.Background(), tc.provider, "", opts2, tc.auths)
			if err != nil {
				t.Fatalf("turn 2 Pick error: %v", err)
			}
			if second.ID != first.ID {
				t.Fatalf("turn 2 = %s, want inherited %s", second.ID, first.ID)
			}

			boundID, ok := s.cache.Get(primaryKey)
			if !ok {
				t.Fatalf("primary session key %q was not bound after inheritance (G3 regression)", primaryKey)
			}
			if boundID != first.ID {
				t.Fatalf("primary key bound to %s, want %s", boundID, first.ID)
			}
		})
	}
}

// TestAdaptiveSelectorConcurrentPickRaceFree drives many goroutines through Pick
// (and concurrent InvalidateAuth) against one selector so the -race detector can
// prove the selector's shared mutable state is safe for concurrent use: the
// per-account token bucket (AccountRateLimiter, which design.md D2 calls the
// concurrency-critical piece), the session stickiness cache (SessionCache) and
// the weighted-pick rng. It asserts only liveness (every Pick returns a usable
// auth, never an error) -- the value of the test is the race detector, not a
// deterministic distribution -- so it uses the production concurrency-safe rng
// (rand.Float64, by not injecting WithAdaptiveRand) rather than a single-goroutine
// constRand, and a fixed clock (a pure constant, safe to read concurrently) to
// keep warm-up/age math stable without a data race on the clock.
func TestAdaptiveSelectorConcurrentPickRaceFree(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	// A mix of mature and warming Claude accounts exercises both the sticky-mature
	// keep path and the warming-breaks-to-mature reselect path, and spreads token
	// buckets across several keys.
	auths := []*Auth{
		newAdaptiveClaudeAuth("m1", "default_claude_max_20x", matureFirstProd()),
		newAdaptiveClaudeAuth("m2", "default_claude_max_5x", matureFirstProd()),
		newAdaptiveClaudeAuth("w1", "default_claude_max_20x", warmupFirstProd()),
		newAdaptiveClaudeAuth("w2", "default_claude_max_5x", warmupFirstProd()),
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
	)
	defer s.Stop()

	const (
		goroutines = 24
		perG       = 60
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				var opts cliproxyexecutor.Options
				switch g % 3 {
				case 0:
					// Shared session key: concurrent GetAndRefresh/Set on one entry.
					opts = cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"shared"}}}
				case 1:
					// Per-goroutine session key: concurrent distinct-key cache writes.
					opts = cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"sess-" + strconv.Itoa(g)}}}
				default:
					// No session: pure weighted path (rng + token bucket only).
					opts = cliproxyexecutor.Options{}
				}
				got, err := s.Pick(context.Background(), "claude", "", opts, auths)
				if err != nil {
					t.Errorf("goroutine %d Pick #%d error: %v", g, i, err)
					return
				}
				if got == nil {
					t.Errorf("goroutine %d Pick #%d returned nil", g, i)
					return
				}
			}
		}(g)
	}

	// Concurrently mutate the sticky cache to stress its write path
	// (InvalidateAuth) against the readers/writers above.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < perG; i++ {
			s.InvalidateAuth("m1")
			s.InvalidateAuth("w1")
		}
	}()

	wg.Wait()
}

func authID(a *Auth) string {
	if a == nil {
		return "<nil>"
	}
	return a.ID
}
