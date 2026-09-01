package auth

import (
	"context"
	"net/http"
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

func authID(a *Auth) string {
	if a == nil {
		return "<nil>"
	}
	return a.ID
}
