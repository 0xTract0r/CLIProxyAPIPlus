package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryOAuthCommitGuardsAlreadyReceivedOldCallback(t *testing.T) {
	ctx := context.Background()
	dir, moved := t.TempDir(), t.TempDir()
	cfg := &config.Config{AuthDir: dir}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	state := "received-old-callback"
	if err := h.RegisterFileOAuthSession(state, "devin", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CompleteOAuthSession(state) })
	if _, err := h.WriteFileOAuthCallback("devin", state, "fake-code", ""); err != nil {
		t.Fatal(err)
	}
	record := &coreauth.Auth{ID: "fake.json", FileName: "fake.json", Provider: "devin", Metadata: map[string]any{"type": "devin"}}
	h.SetConfig(&config.Config{AuthDir: moved})
	if _, ok := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "devin", "", record, "fixture save", false); ok {
		t.Fatal("an old worker committed after AuthDir changed")
	}
	h.SetConfig(cfg)
	if _, ok := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "devin", "", record, "fixture save", false); ok {
		t.Fatal("switching back made an old worker valid")
	}
	for _, path := range []string{filepath.Join(dir, record.ID), filepath.Join(moved, record.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected credential at %s: %v", path, err)
		}
	}

	fresh := "received-fresh-callback"
	if err := h.RegisterFileOAuthSession(fresh, "devin", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CompleteOAuthSession(fresh) })
	if _, err := h.WriteFileOAuthCallback("devin", fresh, "fake-code", ""); err != nil {
		t.Fatal(err)
	}
	if saved, ok := h.commitAndCompleteBuiltinOAuthSession(ctx, fresh, "devin", "", record, "fixture save", false); !ok || saved != filepath.Join(dir, record.ID) {
		t.Fatalf("fresh-scoped save = %q, %v", saved, ok)
	}
}

func TestOrdinaryOAuthCommitRejectsRepointedAuthDir(t *testing.T) {
	ctx := context.Background()
	first, second := t.TempDir(), t.TempDir()
	link := filepath.Join(t.TempDir(), "auth")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: link}, nil)
	const state = "oauth-symlink-fixture"
	if err := h.RegisterFileOAuthSession(state, "devin", link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CompleteOAuthSession(state) })
	if _, err := h.WriteFileOAuthCallback("devin", state, "fixture", ""); err != nil {
		t.Fatal(err)
	}
	next := link + "-next"
	if err := os.Symlink(second, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatal(err)
	}
	record := &coreauth.Auth{ID: "repointed.json", FileName: "repointed.json", Provider: "devin", Metadata: map[string]any{"type": "devin"}}
	if _, ok := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "devin", "", record, "fixture save", false); ok {
		t.Fatal("an old callback committed after its open AuthDir link was repointed")
	}
	for _, dir := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, record.ID)); !os.IsNotExist(err) {
			t.Fatalf("unexpected credential after directory repoint: %v", err)
		}
	}
}
