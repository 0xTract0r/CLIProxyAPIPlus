package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// These ERR-3 tests cover "failover 只落成熟号": when a request is a failover retry
// (it has already tried and failed at least one credential), the execution loops
// hint the adaptive selector to prefer a MATURE account for the retry so failover
// traffic does not routinely land on warming (养号) accounts -- while preserving the
// red line that an all-warming fleet still serves (the hint falls back to the full
// pool rather than hard-failing). They reuse the fixtures in
// adaptive_selector_test.go / account_gate_selector_test.go (newAdaptiveClaudeAuth,
// constRand, fixedClock, matureFirstProd, warmupFirstProd, authID), which live in
// this same package.

// TestFailoverPrefersMatureOverServableWarming asserts (a): on a failover retry
// (the mature-only hint set) selection routes to the mature account even though a
// warming account with a free in-flight slot and a full token bucket exists and
// would -- WITHOUT the hint -- win the same low weighted draw. The no-hint control
// proves the hint is what changes the routing (i.e. the signal is actually
// threaded into scoreCandidates, not dropped).
func TestFailoverPrefersMatureOverServableWarming(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	// a-warm sorts first by ID, so a constRand(0.0) weighted draw targets it first.
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	newSelector := func() *AdaptiveSelector {
		return NewAdaptiveSelector(
			AdaptiveSelectorConfig{Scheduling: cfg},
			WithAdaptiveClock(fixedClock()),
			WithAdaptiveRand(constRand(0.0)),
		)
	}

	// Control: no failover hint -> first-attempt behavior -> the low draw lands on
	// the servable warming account (a-warm), proving it is otherwise selectable.
	s := newSelector()
	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	s.Stop()
	if err != nil {
		t.Fatalf("control Pick returned error: %v", err)
	}
	if got == nil || got.ID != "a-warm" {
		t.Fatalf("control Pick = %v, want a-warm (warming account is servable on the first attempt)", authID(got))
	}

	// Treatment: failover retry hint set -> prefer the mature account, skipping the
	// otherwise-selectable warming account.
	s = newSelector()
	failoverOpts := withFailoverMatureOnly(cliproxyexecutor.Options{})
	got, err = s.Pick(context.Background(), "claude", "", failoverOpts, auths)
	s.Stop()
	if err != nil {
		t.Fatalf("failover Pick returned error: %v", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("failover Pick = %v, want b-mature (failover retry must prefer the mature account, not the warming one)", authID(got))
	}
}

// TestFailoverAllWarmingFleetStillServes asserts (b): with NO mature account in the
// pool -- a pure warming (养号) fleet -- a failover retry (hint set) still selects a
// servable warming account instead of hard-failing. The mature-only filter yields
// nothing, so it MUST fall back to the full pool (兜底红线); the pick must be a real
// warming account and the call must not return an error.
func TestFailoverAllWarmingFleetStillServes(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warmA := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	warmB := newAdaptiveClaudeAuth("b-warm", "default_claude_max_20x", warmupFirstProd())
	auths := []*Auth{warmA, warmB}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
	)
	defer s.Stop()

	failoverOpts := withFailoverMatureOnly(cliproxyexecutor.Options{})
	got, err := s.Pick(context.Background(), "claude", "", failoverOpts, auths)
	if err != nil {
		t.Fatalf("all-warming failover Pick returned error: %v, want a served warming account (fallback must not hard-fail)", err)
	}
	if got == nil {
		t.Fatalf("all-warming failover Pick = nil, want a servable warming account (fallback must not hard-fail)")
	}
	if got.ID != "a-warm" && got.ID != "b-warm" {
		t.Fatalf("all-warming failover Pick = %s, want one of the warming accounts", got.ID)
	}
}

// TestStickyFailoverPrefersMatureOverServableWarming asserts the sticky-reselection
// twin of (a): the ERR-3 failover mature-only hint must be threaded through the
// session-sticky reselection path (selectAndBind), not just the non-sticky Pick.
//
// Construction: session affinity is on and the session's sticky cache entry points
// at a credential that is NOT in the available pool -- exactly the failover-retry
// shape, where the previously-bound account was already tried and dropped from the
// pool. That drives pickWithAffinity -> resolveSticky (bound==nil) -> selectAndBind,
// the sticky reselection that reads the failover hint. The control (same setup, no
// hint) lands on the servable warming account, proving the hint -- not an unrelated
// bias -- is what re-routes the sticky retry to the mature account.
func TestStickyFailoverPrefersMatureOverServableWarming(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	// a-warm sorts first by ID, so a constRand(0.0) weighted draw targets it first.
	// b-mature is the account a failover retry must prefer over the servable warming one.
	baseOpts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"s1"}}}
	// Compute the sticky cache key exactly as pickWithAffinity does (provider :: primary
	// session id :: model); model is "" in the Pick calls below.
	primaryID, _ := extractSessionIDs(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	cacheKey := "claude::" + primaryID + "::"

	newSelectorBoundToTried := func() *AdaptiveSelector {
		s := NewAdaptiveSelector(
			AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
			WithAdaptiveClock(fixedClock()),
			WithAdaptiveRand(constRand(0.0)),
		)
		// Seed the sticky binding to an already-tried credential that is absent from
		// the pool passed to Pick, so resolveSticky finds bound==nil and reselects
		// via selectAndBind (the failover-retry sticky shape).
		s.cache.Set(cacheKey, "z-already-tried")
		return s
	}

	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	// Control: sticky reselection WITHOUT the failover hint -> full-pool weighted pick
	// -> the low draw lands on the servable warming account, proving it is otherwise
	// selectable on this exact (selectAndBind) path.
	s := newSelectorBoundToTried()
	got, err := s.Pick(context.Background(), "claude", "", baseOpts, auths)
	s.Stop()
	if err != nil {
		t.Fatalf("control sticky Pick returned error: %v", err)
	}
	if got == nil || got.ID != "a-warm" {
		t.Fatalf("control sticky Pick = %v, want a-warm (warming account is selectable on the sticky reselection path without the hint)", authID(got))
	}

	// Treatment: sticky reselection WITH the failover hint -> prefer the mature
	// account, skipping the otherwise-selectable warming account. This is the
	// coverage the batch-3 change adds: the hint reaches selectAndBind, not only the
	// non-sticky Pick.
	s = newSelectorBoundToTried()
	failoverOpts := withFailoverMatureOnly(baseOpts)
	got, err = s.Pick(context.Background(), "claude", "", failoverOpts, auths)
	s.Stop()
	if err != nil {
		t.Fatalf("sticky failover Pick returned error: %v", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("sticky failover Pick = %v, want b-mature (sticky failover reselection must prefer the mature account, not the warming one)", authID(got))
	}
}

// TestStickyFailoverAllWarmingFleetStillServes asserts the sticky-reselection twin
// of (b): on a failover retry through the sticky path (selectAndBind) with NO mature
// account in the pool -- a pure warming (养号) fleet -- selection must still return a
// servable warming account instead of hard-failing. The mature-only filter yields
// nothing, so scoreFailoverCandidates MUST fall back to the full pool (兜底红线) on
// this path too; the pick must be a real warming account and the call must not error.
func TestStickyFailoverAllWarmingFleetStillServes(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	baseOpts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"s1"}}}
	primaryID, _ := extractSessionIDs(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	cacheKey := "claude::" + primaryID + "::"

	warmA := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	warmB := newAdaptiveClaudeAuth("b-warm", "default_claude_max_20x", warmupFirstProd())
	auths := []*Auth{warmA, warmB}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
	)
	defer s.Stop()
	// Sticky binding to an already-tried credential absent from the pool -> resolveSticky
	// bound==nil -> selectAndBind reselection.
	s.cache.Set(cacheKey, "z-already-tried")

	failoverOpts := withFailoverMatureOnly(baseOpts)
	got, err := s.Pick(context.Background(), "claude", "", failoverOpts, auths)
	if err != nil {
		t.Fatalf("all-warming sticky failover Pick returned error: %v, want a served warming account (sticky fallback must not hard-fail)", err)
	}
	if got == nil {
		t.Fatalf("all-warming sticky failover Pick = nil, want a servable warming account (sticky fallback must not hard-fail)")
	}
	if got.ID != "a-warm" && got.ID != "b-warm" {
		t.Fatalf("all-warming sticky failover Pick = %s, want one of the warming accounts", got.ID)
	}
}

// TestFailoverTransientErrorsDoNotFeedFailureCluster asserts (c): upstream-capacity
// / transport-transient failures a failover retry rides over -- a 529 overloaded or
// a bare transport/connection error (no HTTP status) -- must NOT feed the health
// failure cluster, so retrying across warming accounts never demotes them for an
// upstream blip. This pins the second-batch ERR classification
// (resultFeedsFailureCluster, conductor_cooldown.go) for exactly the 529 /
// connection-error cases; the complementary demotion / auto-quarantine paths are
// already covered by TestManagerMarkResult_HealthGateTransient429DoesNotDemote and
// TestManagerMarkResult_TransientFailuresNeverAutoQuarantine, so they are not
// duplicated here.
func TestFailoverTransientErrorsDoNotFeedFailureCluster(t *testing.T) {
	overloaded := &Error{Code: "server_overloaded", HTTPStatus: 529}
	// A real transport/connection error resolves to HTTPStatus 0 (no HTTP response)
	// via resultErrorFromError, which resultFeedsFailureCluster treats as retry-only.
	transport := &Error{Code: "connection_error", Message: "read tcp 10.0.0.1:5000->1.2.3.4:443: connection reset by peer"}

	if resultFeedsFailureCluster(Result{AuthID: "x", Provider: "claude", Model: "claude-sonnet-4", Success: false, Error: overloaded}) {
		t.Fatal("529 overloaded must be retry-only and NOT feed the failure cluster")
	}
	if resultFeedsFailureCluster(Result{AuthID: "x", Provider: "claude", Model: "claude-sonnet-4", Success: false, Error: transport}) {
		t.Fatal("transport/connection error (no HTTP status) must be retry-only and NOT feed the failure cluster")
	}

	// Controls: genuine account-level symptoms still feed the cluster, so the
	// transient carve-out is scoped, not a blanket disable.
	forbidden := &Error{Code: "forbidden", HTTPStatus: http.StatusForbidden}
	if !resultFeedsFailureCluster(Result{AuthID: "x", Provider: "claude", Model: "claude-sonnet-4", Success: false, Error: forbidden}) {
		t.Fatal("403 is an account symptom and MUST feed the failure cluster")
	}
}
