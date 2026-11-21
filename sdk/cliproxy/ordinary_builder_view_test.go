package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestOrdinaryBuilderLocalStoreDirectoryIsolation(t *testing.T) {
	previous := sdkAuth.GetTokenStore()
	root := sdkAuth.NewFileTokenStore()
	rootDir := t.TempDir()
	root.SetBaseDir(rootDir)
	sdkAuth.RegisterTokenStore(root)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previous) })

	dirA, dirB := t.TempDir(), t.TempDir()
	build := func(dir string) *Service {
		t.Helper()
		cfg := &config.Config{AuthDir: dir}
		s, err := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(dir, "config.yaml")).Build()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := build(dirA), build(dirB)
	record := func(label string) *coreauth.Auth {
		return &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "local-fixture", Metadata: map[string]any{"type": "local-fixture", "label": label}}
	}
	ctx := context.Background()
	if _, err := a.coreManager.Register(ctx, record("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.coreManager.Register(ctx, record("b")); err != nil {
		t.Fatal(err)
	}
	if got, err := root.Save(ctx, record("root")); err != nil || got != filepath.Join(rootDir, "same.json") {
		t.Errorf("building services changed the global default directory: path=%q err=%v", got, err)
	}
	for label, dir := range map[string]string{"a": dirA, "b": dirB} {
		raw, err := os.ReadFile(filepath.Join(dir, "same.json"))
		var metadata map[string]any
		decodeErr := json.Unmarshal(raw, &metadata)
		if err != nil || decodeErr != nil || metadata["label"] != label {
			t.Errorf("builder %s lost its ordinary record: read=%v decode=%v", label, err, decodeErr)
		}
	}
}
