package auth

import (
	"context"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func testLocalFileRecord(name, label string) *coreauth.Auth {
	return &coreauth.Auth{ID: name, FileName: name, Provider: "local-fixture", Metadata: map[string]any{
		"type": "local-fixture", "label": label,
	}}
}

func TestOrdinaryFileStoreViewsShareRootWriteLock(t *testing.T) {
	var root FileTokenStore
	rootDir, otherDir := t.TempDir(), t.TempDir()
	root.SetBaseDir(rootDir)
	peer := root.WithBaseDir(rootDir)
	nested := peer.WithBaseDir(otherDir)
	lock := root.writeMutex()
	lock.Lock()
	for _, store := range []*FileTokenStore{peer, nested} {
		viewLock := store.writeMutex()
		if viewLock.TryLock() {
			viewLock.Unlock()
			t.Error("directory view acquired a lock while the root was writing")
		}
	}
	lock.Unlock()
	nested.SetBaseDir(otherDir)
	for _, item := range []struct {
		store *FileTokenStore
		dir   string
	}{{peer, rootDir}, {nested, otherDir}} {
		got, err := item.store.Save(context.Background(), testLocalFileRecord("same.json", "fixture"))
		if err != nil || got != filepath.Join(item.dir, "same.json") {
			t.Errorf("wrong directory for bound save: %q (%v)", got, err)
		}
	}
	if root.baseDirSnapshot() != rootDir {
		t.Error("view changed the root directory")
	}
}
