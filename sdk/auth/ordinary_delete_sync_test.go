package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryFileTokenStoreDeleteSharesCanonicalWriteLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	original := filepath.Join(dir, "models.json")
	if err := os.WriteFile(original, []byte("{\"type\":\"synthetic\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "directory-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writer := NewFileTokenStore().WithBaseDir(dir)
	deleter := NewFileTokenStore().WithBaseDir(alias)

	entered, proceed, locked := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		locked <- WithExistingAuthFile(original, func(os.FileInfo) error {
			close(entered)
			<-proceed
			return nil
		})
	}()
	waitOrdinaryFileGate(t, entered, locked)
	done := make(chan error, 1)
	go func() { done <- deleter.Delete(ctx, "models.json") }()
	select {
	case err := <-done:
		close(proceed)
		t.Fatalf("a separately bound Delete escaped the held pathname lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	if err := waitOrdinaryFileOperation(t, locked); err != nil {
		t.Fatal(err)
	}
	if err := waitOrdinaryFileOperation(t, done); err != nil {
		t.Fatal(err)
	}
	if err := deleter.Delete(ctx, "models.json"); err != nil {
		t.Fatalf("idempotent deletion: %v", err)
	}

	manager := coreauth.NewManager(writer, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID: "synthetic", FileName: "models.json", Provider: "synthetic",
		Metadata: map[string]any{"type": "synthetic", "note": "before"},
	}); err != nil {
		t.Fatal(err)
	}
	assertOrdinaryEditCannotRestore(t, manager, original)
}

func TestOrdinaryFileTokenStoreDeleteUnlinksLeafAndRejectsWaiter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	path := filepath.Join(dir, "link.json")
	original := []byte("{\"type\":\"synthetic\",\"note\":\"before\"}")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writer := NewFileTokenStore().WithBaseDir(dir)
	deleter := NewFileTokenStore().WithBaseDir(dir)
	manager := coreauth.NewManager(writer, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID: "synthetic", FileName: "link.json", Provider: "synthetic",
		Metadata: map[string]any{"type": "synthetic", "note": "before"},
	}); err != nil {
		t.Fatal(err)
	}

	// Hold the same deletion section used by Delete, then remove the name while
	// a separate store has a pending explicit edit of that name.
	entered, proceed, removed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		removed <- WithExistingAuthFile(path, func(info os.FileInfo) error {
			if info.Mode()&os.ModeSymlink == 0 {
				return errors.New("deletion did not observe the link itself")
			}
			close(entered)
			<-proceed
			return os.Remove(path)
		})
	}()
	waitOrdinaryFileGate(t, entered, removed)
	editStarted := make(chan struct{})
	edited := make(chan error, 1)
	go func() {
		_, _, err := manager.ModifyPersisted(ctx, "synthetic", 0, func(a *coreauth.Auth) error {
			a.Metadata["note"] = "in flight"
			close(editStarted)
			return nil
		})
		edited <- err
	}()
	waitOrdinaryFileGate(t, editStarted, edited)
	select {
	case err := <-edited:
		close(proceed)
		t.Fatalf("explicit edit escaped the pathname lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	if err := waitOrdinaryFileOperation(t, removed); err != nil {
		t.Fatal(err)
	}
	if err := waitOrdinaryFileOperation(t, edited); !errors.Is(err, coreauth.ErrCredentialFileRemoved) {
		t.Fatalf("waiting edit = %v, want missing credential", err)
	}
	assertOrdinaryEditCannotRestore(t, manager, path)
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("deletion or pending edit changed symlink target: %v", err)
	}

	for _, linkTarget := range []string{target, filepath.Join(dir, "dangling.json")} {
		if err := os.Symlink(linkTarget, path); err != nil {
			t.Fatal(err)
		}
		if err := deleter.Delete(ctx, "link.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("Delete did not remove the link entry: %v", err)
		}
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("Delete changed symlink target: %v", err)
	}
}

func assertOrdinaryEditCannotRestore(t *testing.T, manager *coreauth.Manager, path string) {
	t.Helper()
	_, committed, err := manager.ModifyPersisted(context.Background(), "synthetic", 0, func(a *coreauth.Auth) error {
		a.Metadata["note"] = "after deletion"
		return nil
	})
	if !errors.Is(err, coreauth.ErrCredentialFileRemoved) || committed {
		t.Fatalf("deleted file edit: committed=%v, err=%v", committed, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("credential path restored: %v", err)
	}
	if auth, ok := manager.GetByID("synthetic"); !ok || auth.Metadata["note"] != "before" {
		t.Fatal("failed edit published metadata")
	}
}

func waitOrdinaryFileGate(t *testing.T, entered <-chan struct{}, result <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("operation ended before reaching the gate: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach the gate")
	}
}

func waitOrdinaryFileOperation(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}
