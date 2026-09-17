package auth

import (
	"context"
	"testing"
)

func TestWarmupExecutionAllWarmingStickyReceipt(t *testing.T) {
	s, _, _, auths := servingFixture(t)
	var warming *Auth
	for _, a := range auths {
		if a.ID == "b-cold" {
			warming = a
		}
	}
	if warming == nil {
		t.Fatal("fixture lacks b-cold")
	}
	ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
	opts := servingOptions("receipt-root", "", "parent instructions", "Task")
	if _, err := s.Pick(ctx, "claude", "", opts, []*Auth{warming}); err != nil {
		t.Fatal(err)
	}
	if !warmupRateChargeAvailable(ctx, warming.ID) {
		t.Fatal("first pick lacks receipt")
	}
	// Refill one token while the same logical request still holds its unspent
	// selection receipt (e.g. it lost the execution concurrency race).
	s.limiter.mu.Lock()
	bucket := s.limiter.buckets[warming.ID]
	bucket.tokens = 1
	bucket.last = s.limiter.now()
	s.limiter.mu.Unlock()
	if _, err := s.Pick(ctx, "claude", "", opts, []*Auth{warming}); err != nil {
		t.Fatal(err)
	}
	s.limiter.mu.Lock()
	remaining := s.limiter.buckets[warming.ID].tokens
	s.limiter.mu.Unlock()
	if remaining < 0.9 {
		t.Fatalf("same request spent another token despite outstanding receipt: remaining=%v", remaining)
	}
}
