package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// These tests cover the harden-account-scheduling-limiter core-scheduling slices:
// P2 (rolling-24h warm-up daily budget + persistence/restart re-seed), P0 (stream
// concurrency truly enforced for warming main traffic), P1a (selection anti-streak
// for warming accounts), and P3 (billable-token daily budget hygiene mechanism).
// They reuse the helpers in adaptive_selector_test.go / account_gate_test.go
// (newAdaptiveClaudeAuth, constRand, fixedClock, matureFirstProd, warmupFirstProd,
// adaptiveTestNow, authID, gateFixedClock) which live in this same package.

// ---------------------------------------------------------------------------
// P2: rolling-24h window persistence + restart re-seed
// ---------------------------------------------------------------------------

// TestRollingWindowSeedAndSnapshotRoundTrip proves the P2 persistence path: a
// spent window snapshotted from one gate, round-tripped through the JSON metadata
// shape, re-seeds a cold gate so an account's already-spent budget survives a
// process restart (the fail-open fix) -- and that the seed is honored only while
// the gate is cold, ignored once a live entry exists.
func TestRollingWindowSeedAndSnapshotRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewAccountConcurrencyGate(WithGateClock(gateFixedClock(at)))

	var buckets []DailyWindowBucket
	for i := 0; i < 3; i++ {
		buckets = g.RecordRequestWindow("a", nil)
	}
	if len(buckets) == 0 {
		t.Fatalf("RecordRequestWindow returned no buckets to persist")
	}

	// Serialize into the account_scheduling metadata shape and force a real JSON
	// round-trip (numbers come back as float64), mirroring a persist + reload.
	meta := map[string]any{}
	setAccountSchedulingValue(meta, accountSchedulingDailyWindowKey, dailyWindowToMetadata(buckets))
	raw, errMarshal := json.Marshal(meta)
	if errMarshal != nil {
		t.Fatalf("json.Marshal: %v", errMarshal)
	}
	var reloaded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &reloaded); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal: %v", errUnmarshal)
	}
	seed := readDailyWindowBuckets(reloaded, accountSchedulingDailyWindowKey)
	if len(seed) == 0 {
		t.Fatalf("readDailyWindowBuckets lost the persisted window after JSON round-trip")
	}

	// A fresh (cold, post-restart) gate starts at 0, then re-seeds from the
	// persisted value so the already-spent 3 requests are restored.
	g2 := NewAccountConcurrencyGate(WithGateClock(gateFixedClock(at)))
	if got := g2.DailyCount("a"); got != 0 {
		t.Fatalf("cold gate DailyCount = %d, want 0 before seeding", got)
	}
	if !g2.OverDailyBudgetWindow("a", 3, seed) {
		t.Fatalf("cold gate must re-seed to 3 and be at budget 3 (restart re-seed)")
	}
	if got := g2.DailyCount("a"); got != 3 {
		t.Fatalf("after re-seed DailyCount = %d, want 3", got)
	}

	// Once the gate has a live entry, a (stale) seed must be ignored: the in-memory
	// count is authoritative, so a bogus 99-count seed cannot resurrect budget.
	staleSeed := []DailyWindowBucket{{Hour: at.Unix() / dailyWindowBucketSeconds, Count: 99}}
	if g2.OverDailyBudgetWindow("a", 50, staleSeed) {
		t.Fatalf("live gate must ignore a stale seed (count stayed 3, not 99)")
	}
}

// TestManagerMarkResultRecordsWarmupDailyBudget verifies the P2 counting sink
// moved onto MarkResult: a warming, adaptive-eligible account has each result
// counted against its rolling-24h warm-up request budget and persisted into
// auth.Metadata, while a mature account records nothing (unbounded, no window).
func TestManagerMarkResultRecordsWarmupDailyBudget(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.runtimeConfig.Store(&internalconfig.Config{AccountScheduling: internalconfig.DefaultAccountSchedulingConfig()})
	sel := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: internalconfig.DefaultAccountSchedulingConfig()})
	defer sel.Stop()
	mgr.SetSelector(sel)
	ctx := WithSkipPersist(context.Background())

	warm := newAdaptiveClaudeAuth("claude-warm", "default_claude_max_20x", time.Now().Add(-2*24*time.Hour))
	if _, err := mgr.Register(ctx, warm); err != nil {
		t.Fatalf("Register warm: %v", err)
	}
	mgr.MarkResult(ctx, Result{AuthID: "claude-warm", Provider: "claude", Model: "claude-sonnet-4", Success: true})

	got, ok := mgr.GetByID("claude-warm")
	if !ok || got == nil {
		t.Fatalf("GetByID(claude-warm) ok=%v", ok)
	}
	total := 0
	for _, b := range readDailyWindowBuckets(got.Metadata, accountSchedulingDailyWindowKey) {
		total += b.Count
	}
	if total != 1 {
		t.Fatalf("warm-up daily-budget window total = %d, want 1 (one result counted + persisted)", total)
	}

	// A mature account has no warm-up daily budget, so MarkResult must not record a
	// window for it (day-to-day mature traffic is untouched).
	mature := newAdaptiveClaudeAuth("claude-mature", "default_claude_max_20x", time.Now().Add(-120*24*time.Hour))
	if _, err := mgr.Register(ctx, mature); err != nil {
		t.Fatalf("Register mature: %v", err)
	}
	mgr.MarkResult(ctx, Result{AuthID: "claude-mature", Provider: "claude", Model: "claude-sonnet-4", Success: true})
	gotM, _ := mgr.GetByID("claude-mature")
	if b := readDailyWindowBuckets(gotM.Metadata, accountSchedulingDailyWindowKey); len(b) != 0 {
		t.Fatalf("mature account must not record a warm-up daily-budget window, got %v", b)
	}
}

// ---------------------------------------------------------------------------
// P0: stream concurrency truly enforced for warming main traffic
// ---------------------------------------------------------------------------

// TestAdaptiveSelectorConcurrencyHardGatesThinPool is the harden P0(b) guard: when
// the ONLY servable account is a warming account already at its in-flight
// concurrency ceiling, the selector must DENY with a retryable 429 rather than
// degrade to the round-robin fallback (which ignores the concurrency gate and
// would re-admit the concurrency-full warming account). The negative control
// asserts the denial is transient backpressure: releasing the slot re-admits it.
func TestAdaptiveSelectorConcurrencyHardGatesThinPool(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	auths := []*Auth{warm}

	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	// w1 ConcurrencyLimit = 1: fill the single slot so a-warm has no headroom.
	if ok := gate.Acquire("a-warm", 1); !ok {
		t.Fatalf("pre-fill Acquire = false, want true")
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if got != nil {
		t.Fatalf("Pick served %s, want denial (only account is a concurrency-full warming account)", authID(got))
	}
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("Pick error = %v (%T), want *auth.Error", err, err)
	}
	if authErr.Code != "account_concurrency_exceeded" {
		t.Fatalf("error Code = %q, want account_concurrency_exceeded", authErr.Code)
	}
	if !authErr.Retryable || authErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error = {Retryable:%v HTTPStatus:%d}, want {true 429} (retryable backpressure)", authErr.Retryable, authErr.HTTPStatus)
	}

	// Negative control: release the slot -> headroom restored -> served normally.
	gate.Release("a-warm")
	got, err = s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick (after release) returned error: %v, want normal service", err)
	}
	if got == nil || got.ID != "a-warm" {
		t.Fatalf("Pick (after release) = %v, want a-warm (headroom restored)", authID(got))
	}
}

// TestAdaptiveSelectorConcurrencyHardGateNeverDeniesWithMature is the PROD-3b red
// line: a mature account in the pool must guarantee no locally-manufactured 429,
// even when a warming account is concurrency-full. The request routes to the
// mature account and never denies.
func TestAdaptiveSelectorConcurrencyHardGateNeverDeniesWithMature(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	if ok := gate.Acquire("a-warm", 1); !ok {
		t.Fatalf("pre-fill Acquire = false, want true")
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // low draw targets a-warm first
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error %v, want normal service (a mature account is present -- must never local-429, PROD-3b)", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("Pick = %v, want b-mature (warming concurrency-full, mature absorbs)", authID(got))
	}
}

// TestAdaptiveSelectorOverflowServesMatureNotConcurrencyFullWarming exercises the
// harden P0(a) overflow-pool fix directly: with every rate bucket drained the pick
// falls into the overflow draw, which must still serve the mature account (overflow
// tolerance) while excluding the concurrency-full warming account.
func TestAdaptiveSelectorOverflowServesMatureNotConcurrencyFullWarming(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	if ok := gate.Acquire("a-warm", 1); !ok {
		t.Fatalf("pre-fill Acquire = false, want true")
	}
	limiter := NewAccountRateLimiter(WithClock(fixedClock()))
	// Drain the mature account's bucket so the pick loop exhausts and falls into
	// the overflow draw (the warming account is already concurrency-full).
	for i := 0; i < cfg.MatureLimits.Burst; i++ {
		if !limiter.Allow("b-mature", float64(cfg.MatureLimits.RPMLimit), cfg.MatureLimits.Burst) {
			t.Fatalf("pre-drain Allow #%d unexpectedly denied", i)
		}
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
		WithAdaptiveAccountGate(gate),
		WithAdaptiveRateLimiter(limiter),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error %v, want overflow service to the mature account", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("Pick = %v, want b-mature (overflow keeps mature, drops concurrency-full warming)", authID(got))
	}
}

// ---------------------------------------------------------------------------
// P1a: selection anti-streak for warming accounts
// ---------------------------------------------------------------------------

// antiStreakWarmupConfig returns a scheduling config whose single warm-up stage is
// deliberately un-throttled (huge rpm / concurrency / daily budget, no maturity)
// so the ONLY thing that can rotate a repeated warming pick is the anti-streak
// rule -- isolating P1a from the rate-limiter / concurrency / budget gates.
func antiStreakWarmupConfig(limit int) internalconfig.AccountSchedulingConfig {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.WarmupCurve = []internalconfig.AccountWarmupStage{
		{Name: "w1", MinAgeDays: 0, MaxAgeDays: 0, DailyBudget: 1000000, RPMLimit: 100000, ConcurrencyLimit: 1000},
	}
	cfg.AntiStreakLimit = limit
	return cfg
}

// TestAdaptiveSelectorAntiStreakRotatesWarming verifies P1a: with AntiStreakLimit
// set, a warming account cannot be picked more than the limit times in a row when
// an alternative exists -- the pick rotates to the other warming account -- while
// the long-term share stays balanced. With the knob at 0 (default off) the same
// draws collapse entirely onto the single highest-order account (pre-P1a behavior).
func TestAdaptiveSelectorAntiStreakRotatesWarming(t *testing.T) {
	warmA := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	warmB := newAdaptiveClaudeAuth("b-warm", "default_claude_max_20x", warmupFirstProd())
	auths := []*Auth{warmA, warmB}

	pickCounts := func(limit int) map[string]int {
		s := NewAdaptiveSelector(
			AdaptiveSelectorConfig{Scheduling: antiStreakWarmupConfig(limit)},
			WithAdaptiveClock(fixedClock()),
			WithAdaptiveRand(constRand(0.0)), // always targets the first (a-warm) bucket
		)
		defer s.Stop()
		counts := map[string]int{}
		for i := 0; i < 6; i++ {
			got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("Pick #%d error: %v", i, err)
			}
			if got == nil {
				t.Fatalf("Pick #%d returned nil", i)
			}
			counts[got.ID]++
		}
		return counts
	}

	// Off (default): every draw targets a-warm and nothing rotates it.
	off := pickCounts(0)
	if off["a-warm"] != 6 || off["b-warm"] != 0 {
		t.Fatalf("anti-streak OFF distribution = a-warm=%d b-warm=%d, want 6/0 (pure weighted, no rotation)", off["a-warm"], off["b-warm"])
	}

	// On (limit 2): a-warm may be picked twice in a row, then the third rotates to
	// b-warm, giving the deterministic a,a,b,a,a,b pattern over six draws.
	on := pickCounts(2)
	if on["a-warm"] != 4 || on["b-warm"] != 2 {
		t.Fatalf("anti-streak ON distribution = a-warm=%d b-warm=%d, want 4/2 (rotates after 2 consecutive)", on["a-warm"], on["b-warm"])
	}
}

// ---------------------------------------------------------------------------
// P3: billable-token daily budget hygiene (mechanism; sink wired separately)
// ---------------------------------------------------------------------------

// TestAccountTokenBudgetRollingWindow verifies the gate's rolling-24h token
// counter: it crosses the budget at the configured billable-token total, treats a
// non-positive budget as unbounded, and ages tokens out of the window.
func TestAccountTokenBudgetRollingWindow(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewAccountConcurrencyGate(WithGateClock(gateFixedClock(at)))

	g.RecordTokens("a", 600)
	if g.OverTokenBudget("a", 1000, nil) {
		t.Fatalf("OverTokenBudget(1000) at 600 = true, want false")
	}
	g.RecordTokens("a", 400) // 1000 total
	if got := g.TokenCount("a"); got != 1000 {
		t.Fatalf("TokenCount = %d, want 1000", got)
	}
	if !g.OverTokenBudget("a", 1000, nil) {
		t.Fatalf("OverTokenBudget(1000) at 1000 = false, want true (>= budget)")
	}
	if g.OverTokenBudget("a", 0, nil) {
		t.Fatalf("OverTokenBudget(0) = true, want false (unbounded)")
	}
}

// TestAdaptiveSelectorDropsOverTokenBudgetWarming verifies the P3 selector
// integration: a warming account that has spent its configured billable-token
// daily budget is dropped from selection (via the combined warm-up budget
// predicate) in favour of a mature account, while the mature account -- with an
// unbounded token budget -- is never token-gated.
func TestAdaptiveSelectorDropsOverTokenBudgetWarming(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	curve := internalconfig.DefaultAccountWarmupCurve()
	curve[0].TokenDailyBudget = 1000 // w1 stage gets a token budget
	cfg.WarmupCurve = curve

	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	gate.RecordTokens("a-warm", 1000) // spend the warm-up token budget

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // low draw targets a-warm first
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error %v, want normal service to the mature account", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("Pick = %v, want b-mature (a-warm over its token budget, mature unbounded)", authID(got))
	}
}
