package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// These tests exercise the Phase 2 selection-side gates the AdaptiveSelector now
// applies via its AccountConcurrencyGate: a warming account already at its
// in-flight concurrency ceiling, or already past its warm-up UTC-daily budget,
// is skipped in favour of a mature account (which has a higher concurrency
// ceiling and no daily cap). They reuse the helpers in adaptive_selector_test.go
// (newAdaptiveClaudeAuth, constRand, fixedClock, matureFirstProd,
// warmupFirstProd, adaptiveTestNow), which live in this same package.

// TestAdaptiveSelectorSkipsConcurrencyFullAccount verifies that when the weighted
// draw lands on a warming account that has no free in-flight slot, selection
// drops it and routes to the mature account instead; and, as a control, that the
// same account IS selected once a slot is released.
func TestAdaptiveSelectorSkipsConcurrencyFullAccount(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	// warmupFirstProd -> ~2 days old -> w1 stage: ConcurrencyLimit 1.
	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	// Fill a-warm's single slot so it has no headroom.
	if ok := gate.Acquire("a-warm", 1); !ok {
		t.Fatalf("pre-fill Acquire = false, want true")
	}

	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)), // would target the first (a-warm) bucket
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("Pick = %v, want b-mature (a-warm is concurrency-full)", authID(got))
	}

	// Control: release the slot -> a-warm regains headroom and the same low draw
	// now lands on it (its rate bucket is full, so Allow passes on the first pick).
	gate.Release("a-warm")
	got, err = s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick (after release) returned error: %v", err)
	}
	if got == nil || got.ID != "a-warm" {
		t.Fatalf("Pick (after release) = %v, want a-warm (headroom restored)", authID(got))
	}
}

// TestAdaptiveSelectorSkipsOverDailyBudgetAccount verifies that a warming account
// which has spent its warm-up UTC-daily budget is dropped from selection in
// favour of the mature account (whose DailyBudget is 0 = unbounded), and that it
// is selectable again while still one request under budget.
func TestAdaptiveSelectorSkipsOverDailyBudgetAccount(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	warm := newAdaptiveClaudeAuth("a-warm", "default_claude_max_20x", warmupFirstProd())
	mature := newAdaptiveClaudeAuth("b-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{warm, mature}

	// w1 stage DailyBudget is 200.
	const w1DailyBudget = 200
	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))

	// One request under budget: a-warm is still selectable with a low draw.
	for i := 0; i < w1DailyBudget-1; i++ {
		gate.RecordRequest("a-warm")
	}
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick (under budget) returned error: %v", err)
	}
	if got == nil || got.ID != "a-warm" {
		t.Fatalf("Pick (under budget) = %v, want a-warm (still one under its daily budget)", authID(got))
	}

	// Cross the budget: a-warm is now skipped, routing to the mature account.
	gate.RecordRequest("a-warm") // now at 200 == budget
	if !gate.OverDailyBudget("a-warm", w1DailyBudget) {
		t.Fatalf("precondition: a-warm not over budget at %d records", w1DailyBudget)
	}
	got, err = s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick (over budget) returned error: %v", err)
	}
	if got == nil || got.ID != "b-mature" {
		t.Fatalf("Pick (over budget) = %v, want b-mature (a-warm over daily budget)", authID(got))
	}
}

// TestAdaptiveSelectorMatureAccountNotDailyBudgetGated verifies a mature account
// (DailyBudget 0 = unbounded) is never dropped for daily budget regardless of how
// many requests it has recorded -- only warming accounts are budget-gated.
func TestAdaptiveSelectorMatureAccountNotDailyBudgetGated(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	mature := newAdaptiveClaudeAuth("only-mature", "default_claude_max_20x", matureFirstProd())
	auths := []*Auth{mature}

	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	for i := 0; i < 10000; i++ {
		gate.RecordRequest("only-mature")
	}
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveRand(constRand(0.0)),
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick returned error: %v", err)
	}
	if got == nil || got.ID != "only-mature" {
		t.Fatalf("Pick = %v, want only-mature (mature has no daily budget)", authID(got))
	}
}
