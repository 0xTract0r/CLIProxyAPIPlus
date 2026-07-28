package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestResolveFarmPinAuthID covers resolving a farm pin value (auth ID or account
// name) to a unique auth ID, and the ambiguous/unknown fail-safe behaviour.
func TestResolveFarmPinAuthID(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	ctx := context.Background()
	if _, err := mgr.Register(ctx, &Auth{
		ID:       "auth-id-alpha",
		Provider: "claude",
		Label:    "farm-alpha",
		FileName: "/data/auths/daylenaldmin193.json",
		Metadata: map[string]any{"email": "alpha@example.com"},
	}); err != nil {
		t.Fatalf("Register(alpha) error = %v", err)
	}
	if _, err := mgr.Register(ctx, &Auth{
		ID:       "auth-id-beta",
		Provider: "claude",
		Label:    "farm-beta",
	}); err != nil {
		t.Fatalf("Register(beta) error = %v", err)
	}

	cases := []struct {
		name   string
		value  string
		wantID string
		wantOK bool
	}{
		{name: "exact auth id", value: "auth-id-alpha", wantID: "auth-id-alpha", wantOK: true},
		{name: "label", value: "farm-alpha", wantID: "auth-id-alpha", wantOK: true},
		{name: "label case-insensitive", value: "FARM-BETA", wantID: "auth-id-beta", wantOK: true},
		{name: "email", value: "alpha@example.com", wantID: "auth-id-alpha", wantOK: true},
		{name: "email local-part", value: "alpha", wantID: "auth-id-alpha", wantOK: true},
		{name: "auth file base name", value: "daylenaldmin193", wantID: "auth-id-alpha", wantOK: true},
		{name: "unknown", value: "no-such-account", wantID: "", wantOK: false},
		{name: "empty", value: "   ", wantID: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := mgr.ResolveFarmPinAuthID(tc.value)
			if gotID != tc.wantID || gotOK != tc.wantOK {
				t.Fatalf("ResolveFarmPinAuthID(%q) = (%q, %v), want (%q, %v)", tc.value, gotID, gotOK, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestResolveFarmPinAuthID_AmbiguousNameFailsSafe ensures a name shared by two
// accounts resolves to nothing, so the caller fail-closes rather than guessing.
func TestResolveFarmPinAuthID_AmbiguousNameFailsSafe(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	ctx := context.Background()
	if _, err := mgr.Register(ctx, &Auth{ID: "dup-1", Provider: "claude", Label: "shared-label"}); err != nil {
		t.Fatalf("Register(dup-1) error = %v", err)
	}
	if _, err := mgr.Register(ctx, &Auth{ID: "dup-2", Provider: "claude", Label: "shared-label"}); err != nil {
		t.Fatalf("Register(dup-2) error = %v", err)
	}
	if gotID, gotOK := mgr.ResolveFarmPinAuthID("shared-label"); gotOK || gotID != "" {
		t.Fatalf("ResolveFarmPinAuthID(ambiguous) = (%q, %v), want (\"\", false)", gotID, gotOK)
	}
}

// TestSchedulerFarmPin_UsesPinnedAuthWhenAvailable locks the fail-closed
// primitive: with a pin set, the pinned account is always chosen and the other
// live account is never rotated in.
func TestSchedulerFarmPin_UsesPinnedAuthWhenAvailable(t *testing.T) {
	t.Parallel()

	provider, model := "claude", "claude-farm-pin-available"
	pinnedID, otherID := "farm-pinned-live", "farm-other-live"
	registerSchedulerModels(t, provider, model, pinnedID, otherID)
	scheduler := newSchedulerForTest(&RoundRobinSelector{},
		&Auth{ID: pinnedID, Provider: provider},
		&Auth{ID: otherID, Provider: provider},
	)

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: pinnedID}}
	for i := 0; i < 5; i++ {
		picked, err := scheduler.pickSingle(context.Background(), provider, model, opts, nil)
		if err != nil {
			t.Fatalf("pickSingle() iteration %d error = %v", i, err)
		}
		if picked == nil || picked.ID != pinnedID {
			t.Fatalf("pickSingle() iteration %d = %v, want pinned %s (never %s)", i, picked, pinnedID, otherID)
		}
	}
}

// TestSchedulerFarmPin_FailsClosedWhenPinnedUnavailable is the core
// "串号止血" assertion: when the pinned account is quarantined, the request must
// fail and MUST NOT fall back to the other live account.
func TestSchedulerFarmPin_FailsClosedWhenPinnedUnavailable(t *testing.T) {
	t.Parallel()

	provider, model := "claude", "claude-farm-pin-failclosed"
	pinnedID, otherID := "farm-pinned-dead", "farm-other-live"
	registerSchedulerModels(t, provider, model, pinnedID, otherID)
	scheduler := newSchedulerForTest(&RoundRobinSelector{},
		&Auth{ID: pinnedID, Provider: provider, AutoQuarantined: true, Status: StatusQuarantined},
		&Auth{ID: otherID, Provider: provider},
	)

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: pinnedID}}
	picked, err := scheduler.pickSingle(context.Background(), provider, model, opts, nil)
	if err == nil {
		t.Fatalf("pickSingle() error = nil, want fail-closed error for unavailable pinned account")
	}
	if picked != nil {
		t.Fatalf("pickSingle() = %v, want nil: fail-closed must never fall back to the live account %s", picked, otherID)
	}

	// Sanity: the other account IS live and would be selected absent a pin, so
	// the failure above is purely the fail-closed pin behaviour, not an empty pool.
	pickedNoPin, errNoPin := scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if errNoPin != nil || pickedNoPin == nil || pickedNoPin.ID != otherID {
		t.Fatalf("no-pin pick = (%v, %v), want live account %s", pickedNoPin, errNoPin, otherID)
	}
}

// TestSchedulerNoPin_SelectsReadyAuth guards zero-regression: without a pin the
// scheduler still returns a ready auth via its normal path.
func TestSchedulerNoPin_SelectsReadyAuth(t *testing.T) {
	t.Parallel()

	provider, model := "claude", "claude-farm-nopin"
	firstID, secondID := "farm-nopin-a", "farm-nopin-b"
	registerSchedulerModels(t, provider, model, firstID, secondID)
	scheduler := newSchedulerForTest(&RoundRobinSelector{},
		&Auth{ID: firstID, Provider: provider},
		&Auth{ID: secondID, Provider: provider},
	)

	picked, err := scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle() no-pin error = %v", err)
	}
	if picked == nil || (picked.ID != firstID && picked.ID != secondID) {
		t.Fatalf("pickSingle() no-pin = %v, want one of the two ready auths", picked)
	}
}
