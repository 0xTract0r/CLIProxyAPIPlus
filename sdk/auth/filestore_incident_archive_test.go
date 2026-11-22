package auth

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileTokenStoreListExcludesIncidentArchives(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		archiveRoot bool
	}{
		{name: "archive descendants excluded"},
		{name: "configured root named incident-archives", archiveRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.archiveRoot {
				dir = filepath.Join(dir, "incident-archives")
			}
			want := []string{filepath.Join("ordinary", "nested.json"), "root.json"}
			files := append(slices.Clone(want),
				filepath.Join("incident-archives", "archived.json"),
				filepath.Join("ordinary", "Incident-Archives", "deep", "mixed.json"),
				filepath.Join("ordinary", "other", "INCIDENT-ARCHIVES", "upper.json"),
				filepath.Join(".history", "historic.json"),
			)
			for _, name := range files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("create fixture directory: %v", err)
				}
				if err := os.WriteFile(path, []byte(`{"type":"test"}`), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			assertLoaded := func(label string, auths []*cliproxyauth.Auth) {
				t.Helper()
				var got []string
				for _, auth := range auths {
					got = append(got, auth.ID)
				}
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("%s loaded %v, want %v", label, got, want)
				}
			}

			store := NewFileTokenStore().WithBaseDir(dir)
			listed, err := store.List(ctx)
			if err != nil {
				t.Fatalf("list file store: %v", err)
			}
			assertLoaded("FileTokenStore.List", listed)

			manager := cliproxyauth.NewManager(NewFileTokenStore().WithBaseDir(dir), nil, nil)
			if err := manager.Load(ctx); err != nil {
				t.Fatalf("cold-load auth manager: %v", err)
			}
			assertLoaded("Manager.Load/List", manager.List())
		})
	}
}
