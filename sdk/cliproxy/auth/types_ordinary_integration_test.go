package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOrdinaryAuthCredentialJSONAndCloneIsolation(t *testing.T) {
	var old Auth
	const fixture = `{"id":"old","provider":"claude","label":"saved label","status":"disabled","disabled":true,"generation":3,"registration_epoch":2,"credential_version":7,"last_refreshed_at":"2025-01-02T03:04:05Z","metadata":{"quota_snapshot":{"profile":{"subscription":{"has_claude_max":true}}},"opaque_legacy":{"nested":[{"flag":"original"}]},"refresh_token":"fixture"}}`
	if err := json.Unmarshal([]byte(fixture), &old); err != nil {
		t.Fatal(err)
	}
	old.Metadata["provider_map"] = map[string]string{"name": "saved"}
	cloned := old.Clone()
	cloned.Metadata["provider_map"].(map[string]string)["name"] = "mutated"
	vals := cloned.Metadata["opaque_legacy"].(map[string]any)["nested"].([]any)
	vals[0].(map[string]any)["flag"] = "mutated"
	if old.Metadata["provider_map"].(map[string]string)["name"] != "saved" {
		t.Fatal("typed metadata map shared across snapshots")
	}
	data, err := json.Marshal(&old)
	if err != nil {
		t.Fatal(err)
	}
	var reopened Auth
	if err := json.Unmarshal(data, &reopened); err != nil {
		t.Fatal(err)
	}
	if reopened.Label != "saved label" || !reopened.Disabled || reopened.Status != StatusDisabled || reopened.SubscriptionPlanType() != "max" || reopened.Generation != 3 || reopened.RegistrationEpoch != 2 || reopened.CredentialVersion != 7 || !reopened.LastRefreshedAt.Equal(old.LastRefreshedAt) {
		t.Fatal("old credential display or version changed after reopen")
	}
	if got := reopened.Metadata["opaque_legacy"].(map[string]any)["nested"].([]any)[0].(map[string]any)["flag"]; got != "original" {
		t.Fatal("opaque JSON changed during clone")
	}
}

func TestOrdinaryAuthFileSnapshotAndRuntimeVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	data := []byte(`{"id":"fixture","disabled":true}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{ID: "fixture"}
	a.BindFileSnapshot(path, data)
	if !a.HasFileSnapshot() || !a.FileSnapshotMatchesDisk() || !a.SameFileSnapshot(a.Clone()) {
		t.Fatal("file snapshot not bound")
	}
	if err := os.WriteFile(path, []byte(`{ "disabled": true, "id": "fixture" }`), 0600); err != nil {
		t.Fatal(err)
	}
	if !a.FileSnapshotMatchesDisk() {
		t.Fatal("format-only file change invalidated snapshot")
	}
	if err := os.WriteFile(path, []byte(`{"id":"fixture","disabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if a.FileSnapshotMatchesDisk() {
		t.Fatal("file snapshot accepted a semantic change")
	}
	runtime := a.Clone()
	runtime.Generation = 1
	runtime.RegistrationEpoch = 1
	if runtime.HasFileSnapshot() {
		t.Fatal("registered snapshot still treated as unversioned")
	}
}

func TestOrdinaryAuthSameCredentialRotationCASIgnoresWallClock(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(newMemoryAuthTestStore(), nil, nil)
	future := time.Now().Add(24 * time.Hour).UTC()
	_, err := mgr.Register(ctx, &Auth{ID: "fixture-cas", Provider: "claude", Status: StatusActive, LastRefreshedAt: future, Metadata: map[string]any{"access_token": "original", "refresh_token": "same-credential", "operator_note": "original note"}})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := mgr.GetByID("fixture-cas")
	concurrent := base.Clone()
	concurrent.Metadata["operator_note"] = "new note"
	if _, err := mgr.Update(ctx, concurrent); err != nil {
		t.Fatal(err)
	}
	fresh := base.Clone()
	fresh.LastRefreshedAt = time.Now().UTC()
	fresh.Metadata["refresh_token"] = "rotated"
	fresh.Metadata["access_token"] = "new access"
	if tokenMaterialSnapshot(fresh) == tokenMaterialSnapshot(base) {
		t.Fatal("material snapshot missed rotation")
	}
	clockOnly := base.Clone()
	clockOnly.LastRefreshedAt = time.Now().UTC()
	if tokenMaterialSnapshot(clockOnly) != tokenMaterialSnapshot(base) {
		t.Fatal("material snapshot depends on wall clock")
	}
	if _, err := mgr.UpdateRefreshedAuth(ctx, base, fresh); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.GetByID(base.ID)
	if got.Metadata["operator_note"] != "new note" || got.Metadata["refresh_token"] != "rotated" {
		t.Fatal("valid same-credential result lost or overwrote concurrent note")
	}
	stale := base.Clone()
	stale.Metadata["refresh_token"] = "late-result"
	if _, err := mgr.UpdateRefreshedAuth(ctx, base, stale); err != nil {
		t.Fatal(err)
	}
	got, _ = mgr.GetByID(base.ID)
	if got.Metadata["refresh_token"] != "rotated" {
		t.Fatal("stale result overwrote committed rotation")
	}
}
