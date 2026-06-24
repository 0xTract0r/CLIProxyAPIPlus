package auth

import (
	"context"
	"sync"
	"testing"
)

func codexHighWaterEntry(version string) CodexDeviceHighWater {
	return CodexDeviceHighWater{
		UserAgent:  "codex_cli_rs/" + version + " (Mac OS 15.7.4; arm64) iTerm.app/3.6.8 (codex_cli_rs; " + version + ")",
		Version:    version,
		Originator: "codex_cli_rs",
		Source:     "observed",
		LastSeenAt: "2026-06-16T00:00:00Z",
	}
}

func registerCodexAuth(t *testing.T, mgr *Manager) *Auth {
	t.Helper()
	auth := &Auth{
		ID:       "codex-auth-1",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	return auth
}

func TestRaiseCodexDeviceHighWater_RaisesAndPersistsOnHigherVersion(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerCodexAuth(t, mgr)
	baseline := store.saveCount.Load()

	raised, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.150.0"))
	if err != nil {
		t.Fatalf("RaiseCodexDeviceHighWater returned error: %v", err)
	}
	if !raised {
		t.Fatalf("expected raised=true for first high-water write")
	}
	if got := store.saveCount.Load(); got != baseline+1 {
		t.Fatalf("expected exactly one Save after raise, got %d (baseline %d)", got, baseline)
	}

	stored, ok := mgr.GetByID("codex-auth-1")
	if !ok {
		t.Fatalf("auth not found after raise")
	}
	hw, ok := CodexDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok {
		t.Fatalf("persisted high-water not readable from metadata: %#v", stored.Metadata)
	}
	if hw.Version != "0.150.0" || hw.Originator != "codex_cli_rs" {
		t.Fatalf("persisted entry mismatch: %+v", hw)
	}
}

func TestRaiseCodexDeviceHighWater_NoWriteOnSameOrLowerVersion(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerCodexAuth(t, mgr)

	if _, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.150.0")); err != nil {
		t.Fatalf("seed raise error: %v", err)
	}
	afterSeed := store.saveCount.Load()

	raised, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.150.0"))
	if err != nil {
		t.Fatalf("same-version raise error: %v", err)
	}
	if raised {
		t.Fatalf("expected raised=false for same version")
	}

	raised, err = mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.149.9"))
	if err != nil {
		t.Fatalf("lower-version raise error: %v", err)
	}
	if raised {
		t.Fatalf("expected raised=false for lower version")
	}

	if got := store.saveCount.Load(); got != afterSeed {
		t.Fatalf("expected no additional Save for same/lower version, got %d (after seed %d)", got, afterSeed)
	}

	stored, _ := mgr.GetByID("codex-auth-1")
	hw, _ := CodexDeviceHighWaterFromMetadata(stored.Metadata)
	if hw.Version != "0.150.0" {
		t.Fatalf("high-water should stay at 0.150.0, got %s", hw.Version)
	}
}

func TestRaiseCodexDeviceHighWater_WholeMapReplacementIsolatesEarlierSnapshots(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerCodexAuth(t, mgr)

	if _, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.150.0")); err != nil {
		t.Fatalf("first raise error: %v", err)
	}
	firstSnapshot := func() *Auth {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.lastSaved
	}()
	if firstSnapshot == nil {
		t.Fatalf("no snapshot captured after first raise")
	}
	firstHW, ok := CodexDeviceHighWaterFromMetadata(firstSnapshot.Metadata)
	if !ok || firstHW.Version != "0.150.0" {
		t.Fatalf("first snapshot high-water mismatch: ok=%v %+v", ok, firstHW)
	}

	if _, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry("0.190.0")); err != nil {
		t.Fatalf("second raise error: %v", err)
	}

	// The earlier captured snapshot must NOT have been mutated to 0.190.0 by the
	// second raise (whole-map replacement isolates clones from the live auth).
	stillFirst, ok := CodexDeviceHighWaterFromMetadata(firstSnapshot.Metadata)
	if !ok || stillFirst.Version != "0.150.0" {
		t.Fatalf("earlier snapshot was mutated by a later raise: ok=%v %+v (want 0.150.0)", ok, stillFirst)
	}
}

func TestRaiseCodexDeviceHighWater_ConcurrentRaisesKeepHighest(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerCodexAuth(t, mgr)

	versions := []string{"0.141.0", "0.150.0", "0.145.0", "0.180.0", "0.160.0"}
	var wg sync.WaitGroup
	for _, v := range versions {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			if _, err := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", codexHighWaterEntry(version)); err != nil {
				t.Errorf("concurrent raise error: %v", err)
			}
		}(v)
	}
	wg.Wait()

	stored, _ := mgr.GetByID("codex-auth-1")
	hw, ok := CodexDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok || hw.Version != "0.180.0" {
		t.Fatalf("concurrent raises did not converge to highest: ok=%v %q (want 0.180.0)", ok, hw.Version)
	}
}

func TestRaiseCodexDeviceHighWater_MissingAuthOrInvalidEntryIsNoop(t *testing.T) {
	store := &capturingStore{}
	mgr := NewManager(store, nil, nil)
	registerCodexAuth(t, mgr)

	if raised, _ := mgr.RaiseCodexDeviceHighWater(context.Background(), "no-such-auth", codexHighWaterEntry("0.150.0")); raised {
		t.Fatalf("expected no-op for missing auth")
	}
	if raised, _ := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", CodexDeviceHighWater{}); raised {
		t.Fatalf("expected no-op for empty/invalid entry")
	}
	if raised, _ := mgr.RaiseCodexDeviceHighWater(context.Background(), "codex-auth-1", CodexDeviceHighWater{UserAgent: "not-a-codex-ua"}); raised {
		t.Fatalf("expected no-op for unparseable version")
	}
}

func TestCodexDeviceHighWaterFromMetadata_RoundTripShapes(t *testing.T) {
	want := codexHighWaterEntry("0.150.0")

	// in-process map[string]any shape
	metaAny := map[string]any{
		CodexDeviceHighWaterMetadataKey: codexDeviceHighWaterToMetadataMap(want),
	}
	if hw, ok := CodexDeviceHighWaterFromMetadata(metaAny); !ok || hw.Version != "0.150.0" || hw.Originator != "codex_cli_rs" {
		t.Fatalf("map[string]any round trip mismatch: ok=%v %+v", ok, hw)
	}

	// decoded-from-JSON map[string]string shape (restart round trip)
	metaStr := map[string]any{
		CodexDeviceHighWaterMetadataKey: map[string]string{
			"user_agent": want.UserAgent,
			"version":    want.Version,
			"originator": want.Originator,
			"source":     want.Source,
		},
	}
	if hw, ok := CodexDeviceHighWaterFromMetadata(metaStr); !ok || hw.Version != "0.150.0" {
		t.Fatalf("map[string]string round trip mismatch: ok=%v %+v", ok, hw)
	}

	// typed value shape
	metaTyped := map[string]any{CodexDeviceHighWaterMetadataKey: want}
	if hw, ok := CodexDeviceHighWaterFromMetadata(metaTyped); !ok || hw.Version != "0.150.0" {
		t.Fatalf("typed value round trip mismatch: ok=%v %+v", ok, hw)
	}

	// version-only entry (UA absent) must still parse from Version
	versionOnly := map[string]any{CodexDeviceHighWaterMetadataKey: map[string]any{"version": "0.150.0"}}
	if hw, ok := CodexDeviceHighWaterFromMetadata(versionOnly); !ok || hw.Version != "0.150.0" {
		t.Fatalf("version-only round trip mismatch: ok=%v %+v", ok, hw)
	}

	// empty / missing
	if _, ok := CodexDeviceHighWaterFromMetadata(nil); ok {
		t.Fatalf("nil metadata should not yield a high-water")
	}
	if _, ok := CodexDeviceHighWaterFromMetadata(map[string]any{"other": "x"}); ok {
		t.Fatalf("metadata without the key should not yield a high-water")
	}
}
