package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryLifecycleReopensLegacyStateAndUnknownJSON(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, tc := range []struct {
		id, fixture string
		status      coreauth.Status
	}{
		{"disabled.json", `{"type":"synthetic","disabled":true,"reauth_required":true,"custom":{"n":42}}`, coreauth.StatusDisabled},
		{"terminal.json", `{"type":"synthetic","reauth_required":true,"custom":{"n":42}}`, coreauth.StatusError},
	} {
		path := filepath.Join(dir, tc.id)
		if err := os.WriteFile(path, []byte(tc.fixture), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     string
		status coreauth.Status
	}{
		{"disabled.json", coreauth.StatusDisabled},
		{"terminal.json", coreauth.StatusError},
	} {
		a, found := manager.GetByID(tc.id)
		if !found || a.Status != tc.status {
			t.Fatalf("initial state of %s was not restored", tc.id)
		}
		if _, ok, err := manager.ModifyPersisted(ctx, tc.id, a.RegistrationEpoch, func(a *coreauth.Auth) error {
			a.Metadata["note"] = "fixture edit"
			return nil
		}); err != nil || !ok {
			t.Fatalf("save %s: %v", tc.id, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, tc.id))
		var saved struct {
			Custom map[string]int
			Note   string
		}
		if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Custom["n"] != 42 || saved.Note != "fixture edit" {
			t.Fatalf("normal edit did not preserve unknown JSON for %s", tc.id)
		}
	}
	cold := coreauth.NewManager(store, nil, nil)
	if err := cold.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]coreauth.Status{"disabled.json": coreauth.StatusDisabled, "terminal.json": coreauth.StatusError} {
		a, found := cold.GetByID(id)
		if !found || a.Status != status || a.Metadata["note"] != "fixture edit" {
			t.Fatalf("cold state of %s did not survive edit", id)
		}
	}
}
