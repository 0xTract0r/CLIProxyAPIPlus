package auth

import (
	"context"
	"testing"
	"time"
)

// This file covers openspec/changes/add-adaptive-account-scheduling G1: the
// first-production freshness anchor is minted (and write-through persisted) on
// an account's first real serving success, is append-only across later
// successes, and is never minted by a failing result. The anchor is the sole
// freshness signal adaptive scheduling ages accounts by; without it every
// account's AccountAgeDays stays ok=false, is judged "cold", and is pinned to
// the warm-up cold cap forever (account_freshness.go / account_weight.go).

func registerClaudeAuthForMint(t *testing.T, mgr *Manager, id string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register(%s) error: %v", id, err)
	}
}

func TestMarkResultMintsFirstProductionAnchorOnFirstSuccess(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuthForMint(t, mgr, "claude-mint-1")

	// Precondition: register must not mint an anchor -- only a real serving
	// success does.
	pre, ok := mgr.GetByID("claude-mint-1")
	if !ok {
		t.Fatalf("auth missing after register")
	}
	if anchor, anchored := AuthFirstProductionAt(pre); anchored {
		t.Fatalf("anchor %v unexpectedly present before first success", anchor)
	}

	saveBaseline := store.saveCount.Load()
	notBefore := time.Now().Add(-2 * time.Second)
	mgr.MarkResult(context.Background(), Result{AuthID: "claude-mint-1", Provider: "claude", Success: true})
	notAfter := time.Now().Add(2 * time.Second)

	// (1) anchor minted, readable from the record, stamped near "now".
	stored, ok := mgr.GetByID("claude-mint-1")
	if !ok {
		t.Fatalf("auth missing after first success")
	}
	anchor, anchored := AuthFirstProductionAt(stored)
	if !anchored {
		t.Fatalf("first-production anchor not minted on first success; metadata=%#v", stored.Metadata)
	}
	if anchor.Before(notBefore) || anchor.After(notAfter) {
		t.Fatalf("anchor %v not stamped near now [%v, %v]", anchor, notBefore, notAfter)
	}

	// (2) write-through: the same MarkResult call persisted the record, and the
	// persisted snapshot carries the anchor (not just the in-memory copy).
	if got := store.saveCount.Load(); got <= saveBaseline {
		t.Fatalf("expected a Save (write-through) on first success; saveCount %d <= baseline %d", got, saveBaseline)
	}
	store.mu.Lock()
	saved := store.lastSaved
	store.mu.Unlock()
	if saved == nil {
		t.Fatalf("store captured no saved auth")
	}
	if _, persisted := AuthFirstProductionAt(saved); !persisted {
		t.Fatalf("saved record does not carry the first-production anchor: %#v", saved.Metadata)
	}

	// (3) append-only idempotency: a later success returns the existing anchor
	// unchanged, never re-stamping it.
	mgr.MarkResult(context.Background(), Result{AuthID: "claude-mint-1", Provider: "claude", Success: true})
	after, ok := mgr.GetByID("claude-mint-1")
	if !ok {
		t.Fatalf("auth missing after second success")
	}
	anchor2, anchored2 := AuthFirstProductionAt(after)
	if !anchored2 {
		t.Fatalf("anchor lost after second success")
	}
	if !anchor2.Equal(anchor) {
		t.Fatalf("anchor moved on second success: %v -> %v (must be append-only)", anchor, anchor2)
	}
}

func TestMarkResultFailureDoesNotMintFirstProductionAnchor(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuthForMint(t, mgr, "claude-mint-fail")

	mgr.MarkResult(context.Background(), Result{
		AuthID:   "claude-mint-fail",
		Provider: "claude",
		Success:  false,
		Error:    &Error{Code: "boom", Message: "boom", HTTPStatus: 500},
	})

	stored, ok := mgr.GetByID("claude-mint-fail")
	if !ok {
		t.Fatalf("auth missing after failure")
	}
	if anchor, anchored := AuthFirstProductionAt(stored); anchored {
		t.Fatalf("failure minted a first-production anchor %v; want none (anchor is tied to serving success)", anchor)
	}
}
