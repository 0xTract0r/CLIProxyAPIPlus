package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// capturingStore records Save calls and the last saved auth snapshot so tests can
// assert both the persist count and the persisted metadata content.
type capturingStore struct {
	mu        sync.Mutex
	saveCount atomic.Int32
	lastSaved *Auth
}

func (s *capturingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *capturingStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saveCount.Add(1)
	s.mu.Lock()
	s.lastSaved = auth
	s.mu.Unlock()
	return "", nil
}

func (s *capturingStore) Delete(context.Context, string) error { return nil }

func highWaterTriple(version string) ClaudeDeviceHighWater {
	return ClaudeDeviceHighWater{
		UserAgent:      "claude-cli/" + version + " (external, cli)",
		Version:        version,
		PackageVersion: "0.80.0",
		RuntimeVersion: "v24.6.0",
		OS:             "MacOS",
		Arch:           "arm64",
		Source:         "observed",
		LastSeenAt:     "2026-06-16T00:00:00Z",
	}
}

func registerClaudeAuth(t *testing.T, mgr *Manager) *Auth {
	t.Helper()
	auth := &Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	return auth
}

func TestRaiseClaudeDeviceHighWater_RaisesAndPersistsOnHigherVersion(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuth(t, mgr)
	baseline := store.saveCount.Load()

	raised, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.5.0"))
	if err != nil {
		t.Fatalf("RaiseClaudeDeviceHighWater returned error: %v", err)
	}
	if !raised {
		t.Fatalf("expected raised=true for first high-water write")
	}
	if got := store.saveCount.Load(); got != baseline+1 {
		t.Fatalf("expected exactly one Save after raise, got %d (baseline %d)", got, baseline)
	}

	stored, ok := mgr.GetByID("claude-auth-1")
	if !ok {
		t.Fatalf("auth not found after raise")
	}
	hw, ok := ClaudeDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok {
		t.Fatalf("persisted high-water not readable from metadata: %#v", stored.Metadata)
	}
	if hw.Version != "2.5.0" || hw.PackageVersion != "0.80.0" || hw.RuntimeVersion != "v24.6.0" {
		t.Fatalf("persisted triple mismatch: %+v", hw)
	}
}

func TestRaiseClaudeDeviceHighWater_NoWriteOnSameOrLowerVersion(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuth(t, mgr)

	if _, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.5.0")); err != nil {
		t.Fatalf("seed raise error: %v", err)
	}
	afterSeed := store.saveCount.Load()

	// Same version must not write.
	raised, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.5.0"))
	if err != nil {
		t.Fatalf("same-version raise error: %v", err)
	}
	if raised {
		t.Fatalf("expected raised=false for same version")
	}

	// Lower version must not write.
	raised, err = mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.4.9"))
	if err != nil {
		t.Fatalf("lower-version raise error: %v", err)
	}
	if raised {
		t.Fatalf("expected raised=false for lower version")
	}

	if got := store.saveCount.Load(); got != afterSeed {
		t.Fatalf("expected no additional Save for same/lower version, got %d (after seed %d)", got, afterSeed)
	}

	stored, _ := mgr.GetByID("claude-auth-1")
	hw, _ := ClaudeDeviceHighWaterFromMetadata(stored.Metadata)
	if hw.Version != "2.5.0" {
		t.Fatalf("high-water should stay at 2.5.0, got %s", hw.Version)
	}
}

func TestRaiseClaudeDeviceHighWater_WholeMapReplacementIsolatesEarlierSnapshots(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuth(t, mgr)

	// First raise: capture the snapshot persisted by the store (produced via
	// auth.Clone(), which shallow-copies Metadata top-level entries).
	if _, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.5.0")); err != nil {
		t.Fatalf("first raise error: %v", err)
	}
	store.mu.Lock()
	firstSnapshot := store.lastSaved
	store.mu.Unlock()
	if firstSnapshot == nil {
		t.Fatalf("store captured no saved auth on first raise")
	}

	// Second raise to a higher version. Because the write replaces the nested map
	// wholesale (new map[string]any) instead of mutating the old one in place, the
	// earlier snapshot must still see 2.5.0 — never a half-written or overwritten
	// triple. This is the precise hazard Auth.Clone's shallow Metadata copy creates
	// when nested maps are mutated in place.
	if _, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple("2.9.0")); err != nil {
		t.Fatalf("second raise error: %v", err)
	}

	firstHW, ok := ClaudeDeviceHighWaterFromMetadata(firstSnapshot.Metadata)
	if !ok {
		t.Fatalf("first snapshot lost its high-water")
	}
	if firstHW.Version != "2.5.0" {
		t.Fatalf("first snapshot was mutated by a later raise: got %s want 2.5.0 (in-place mutation hazard)", firstHW.Version)
	}

	liveStored, _ := mgr.GetByID("claude-auth-1")
	liveHW, _ := ClaudeDeviceHighWaterFromMetadata(liveStored.Metadata)
	if liveHW.Version != "2.9.0" {
		t.Fatalf("live high-water should be 2.9.0, got %s", liveHW.Version)
	}
}

func TestRaiseClaudeDeviceHighWater_ConcurrentRaisesKeepHighest(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuth(t, mgr)

	versions := []string{"2.1.63", "2.2.0", "2.9.1", "2.5.4", "3.0.0", "2.8.0", "2.9.1", "2.0.0"}
	var wg sync.WaitGroup
	for _, v := range versions {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			if _, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", highWaterTriple(version)); err != nil {
				t.Errorf("concurrent raise error: %v", err)
			}
		}(v)
	}
	wg.Wait()

	stored, _ := mgr.GetByID("claude-auth-1")
	hw, ok := ClaudeDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok {
		t.Fatalf("high-water missing after concurrent raises")
	}
	if hw.Version != "3.0.0" {
		t.Fatalf("expected highest version 3.0.0 after concurrent raises, got %s", hw.Version)
	}
}

func TestRaiseClaudeDeviceHighWater_MissingAuthOrInvalidTripleIsNoop(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerClaudeAuth(t, mgr)
	baseline := store.saveCount.Load()

	// Unknown auth ID.
	if raised, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "nope", highWaterTriple("9.0.0")); raised || err != nil {
		t.Fatalf("expected noop for unknown auth, got raised=%v err=%v", raised, err)
	}
	// Invalid triple (no parseable UA).
	bad := ClaudeDeviceHighWater{UserAgent: "not-a-claude-ua", Version: ""}
	if raised, err := mgr.RaiseClaudeDeviceHighWater(context.Background(), "claude-auth-1", bad); raised || err != nil {
		t.Fatalf("expected noop for invalid triple, got raised=%v err=%v", raised, err)
	}
	if got := store.saveCount.Load(); got != baseline {
		t.Fatalf("expected no additional Save for noop cases, got %d (baseline %d)", got, baseline)
	}
}

func TestClaudeDeviceHighWaterFromMetadata_RoundTripShapes(t *testing.T) {
	hw := highWaterTriple("2.7.3")
	asMap := claudeDeviceHighWaterToMetadataMap(hw)

	// map[string]any shape (just-written this run).
	got, ok := ClaudeDeviceHighWaterFromMetadata(map[string]any{ClaudeDeviceHighWaterMetadataKey: asMap})
	if !ok || got.Version != "2.7.3" || got.PackageVersion != "0.80.0" {
		t.Fatalf("map[string]any round-trip failed: ok=%v got=%+v", ok, got)
	}

	// map[string]string shape (token store may round-trip string values).
	strMap := map[string]string{
		"user_agent":      hw.UserAgent,
		"version":         hw.Version,
		"package_version": hw.PackageVersion,
		"runtime_version": hw.RuntimeVersion,
	}
	got, ok = ClaudeDeviceHighWaterFromMetadata(map[string]any{ClaudeDeviceHighWaterMetadataKey: strMap})
	if !ok || got.Version != "2.7.3" {
		t.Fatalf("map[string]string round-trip failed: ok=%v got=%+v", ok, got)
	}

	// Absent key.
	if _, ok := ClaudeDeviceHighWaterFromMetadata(map[string]any{"other": 1}); ok {
		t.Fatalf("expected false for absent high-water key")
	}
	// Empty metadata.
	if _, ok := ClaudeDeviceHighWaterFromMetadata(nil); ok {
		t.Fatalf("expected false for nil metadata")
	}
}
