package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestOrdinaryPersistedDirectoryFence(t *testing.T) {
	ctx := context.Background()
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := func(dir string) *config.Config { return &config.Config{AuthDir: dir} }
	build := func(dir string) (*Service, string) {
		t.Helper()
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		service, err := NewBuilder().WithConfig(cfg(dir)).WithConfigPath(configPath).Build()
		if err != nil {
			t.Fatal(err)
		}
		return service, configPath
	}
	persisted := func(dir, id, note string) *coreauth.Auth {
		t.Helper()
		path := filepath.Join(dir, id)
		raw := []byte(fmt.Sprintf(`{"type":"local-fixture","note":%q}`, note))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		auth := &coreauth.Auth{ID: id, Provider: "local-fixture", Attributes: map[string]string{coreauth.AttributePath: path}, Metadata: map[string]any{"note": note}}
		auth.BindFileSnapshot(path, raw)
		return auth
	}
	post := func(s *Service, a *coreauth.Auth) {
		t.Helper()
		if err := s.runtimeAuthSyncHook()(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	assertNote := func(s *Service, id, want string) {
		t.Helper()
		auth, ok := s.coreManager.GetByID(id)
		if !ok || auth.Metadata["note"] != want {
			t.Fatalf("%s did not retain its current note (%t)", id, ok)
		}
	}
	const id = "same.json"
	a, configPath := build(dirA)
	b, _ := build(dirB)
	old := persisted(dirA, id, "a")
	post(b, persisted(dirB, id, "b"))
	a.commitConfigUpdate(cfg(dirC))
	post(a, persisted(dirC, id, "c"))
	post(a, old)
	assertNote(a, id, "c")
	oldWatcherEvent := watcher.AuthUpdate{Action: watcher.AuthUpdateActionModify, ID: id, Auth: old}
	oldWatcherEvent.SetRevision(3000)
	a.handleAuthUpdate(coreauth.WithSkipPersist(ctx), oldWatcherEvent)
	assertNote(a, id, "c")
	assertNote(b, id, "b")
	if !old.FileSnapshotMatchesDisk() {
		t.Fatal("retired callback rewrote its original file")
	}

	w, err := defaultWatcherFactory(configPath, dirC, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	w.SetConfig(cfg(dirC))
	a.setWatcher(w)
	updates := make(chan watcher.AuthUpdate, 4)
	w.SetAuthUpdateQueue(updates)
	realDispatch := w.dispatchPersistedAuthWithRev
	dispatched := 0
	w.dispatchPersistedAuthWithRev = func(update *watcher.AuthUpdate) (bool, uint64) {
		dispatched++
		a.commitConfigUpdate(cfg(dirA))
		ok, rev := realDispatch(update)
		if ok || rev != 0 {
			t.Error("watcher admitted retired publication")
		}
		return ok, rev
	}
	rejected := persisted(dirC, "retired-before-dispatch.json", "old-c")
	post(a, rejected)
	if dispatched != 1 || len(updates) != 0 {
		t.Fatal("retired update entered real watcher queue")
	}
	if _, ok := a.coreManager.GetByID(rejected.ID); ok {
		t.Fatal("retired update was registered inline")
	}
	// The first case changes the Service directly; synchronize the watcher for the next case.
	w.SetConfig(cfg(dirA))
	w.dispatchPersistedAuthWithRev = func(update *watcher.AuthUpdate) (bool, uint64) {
		ok, rev := realDispatch(update)
		a.commitConfigUpdate(cfg(dirB))
		a.commitConfigUpdate(cfg(dirA))
		return ok, rev
	}
	queued := persisted(dirA, "queued-before-reload.json", "old-a")
	post(a, queued)
	select {
	case update := <-updates:
		a.handleAuthUpdate(coreauth.WithSkipPersist(ctx), update)
	case <-time.After(3 * time.Second):
		t.Fatal("no real watcher update")
	}
	if _, ok := a.coreManager.GetByID(queued.ID); ok {
		t.Fatal("queued prior generation registered after directory round trip")
	}
	if !queued.FileSnapshotMatchesDisk() {
		t.Fatal("stale queued event rewrote local file")
	}

	w.dispatchPersistedAuthWithRev = realDispatch
	post(a, persisted(dirA, id, "current-a"))
	assertNote(a, id, "current-a")
	post(a, &coreauth.Auth{ID: "runtime", Provider: "local-fixture", Metadata: map[string]any{"note": "runtime"}})
	assertNote(a, "runtime", "runtime")

	// Capture the source while A is active and attempt the manager publication
	// only after C owns the same name. This reaches Update's final fence even
	// though the watcher's earlier precheck would already reject the event.
	const sharedID = "direct-update.json"
	staleUpdate := persisted(dirA, sharedID, "retired-update")
	post(a, staleUpdate)
	staleUpdateCtx := coreauth.WithAuthPublicationFence(coreauth.WithSkipPersist(ctx), a.persistedFilePublicationFence(staleUpdate))
	a.commitConfigUpdate(cfg(dirC))
	freshUpdate := persisted(dirC, sharedID, "current-update")
	post(a, freshUpdate)
	assertNote(a, sharedID, "current-update")
	if _, err := a.coreManager.Update(staleUpdateCtx, staleUpdate); !errors.Is(err, coreauth.ErrAuthPublicationOutOfScope) {
		t.Fatalf("retired same-name Update passed final publication: %v", err)
	}
	assertNote(a, sharedID, "current-update")
	if !staleUpdate.FileSnapshotMatchesDisk() || !freshUpdate.FileSnapshotMatchesDisk() {
		t.Fatal("retired Update rewrote one of the two source files")
	}
	a.commitConfigUpdate(cfg(dirA))

	publication := persisted(dirA, "direct-publication.json", "retired")
	publicationCtx := coreauth.WithAuthPublicationFence(coreauth.WithSkipPersist(ctx), a.persistedFilePublicationFence(publication))
	a.commitConfigUpdate(cfg(dirC))
	if _, err := a.coreManager.Register(publicationCtx, publication); !errors.Is(err, coreauth.ErrAuthPublicationOutOfScope) {
		t.Fatalf("retired publication passed final manager boundary: %v", err)
	}
	if _, ok := a.coreManager.GetByID(publication.ID); ok || !publication.FileSnapshotMatchesDisk() {
		t.Fatal("manager published retired credentials or modified their file")
	}
}

func TestOrdinaryAcceptedDirectoryAliasSource(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	aliasDir := filepath.Join(root, "alias")
	outside := filepath.Join(root, "real-outside")
	for _, dir := range []string{realDir, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: aliasDir}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("auth-dir: %q\n", aliasDir)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := defaultWatcherFactory(path, aliasDir, func(next *config.Config) { s.applyWatcherConfigUpdate(next) })
	if err != nil {
		t.Fatal(err)
	}
	s.setWatcher(w)
	s.ensureAuthUpdateQueue(ctx)
	w.SetAuthUpdateQueue(s.authUpdates)
	w.SetConfig(cfg)
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	write := func(dir, name string) *coreauth.Auth {
		t.Helper()
		p := filepath.Join(dir, name)
		raw := []byte(`{"type":"local-fixture","note":"synthetic"}`)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		record := &coreauth.Auth{ID: name, Provider: "local-fixture", Attributes: map[string]string{coreauth.AttributePath: p}, Metadata: map[string]any{"note": "synthetic"}}
		record.BindFileSnapshot(p, raw)
		return record
	}
	has := func(id string) bool {
		_, ok := s.coreManager.GetByID(id)
		return ok
	}
	write(realDir, "watcher-alias.json")
	deadline := time.Now().Add(3 * time.Second)
	for !has("watcher-alias.json") && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !has("watcher-alias.json") {
		t.Error("native watcher did not admit the real directory event behind the alias")
	}
	for _, item := range []struct{ dir, name string }{
		{realDir, "canonical-callback.json"},
		{outside, "outside-callback.json"},
	} {
		record := write(item.dir, item.name)
		fence := s.persistedFilePublicationFence(record)
		unlock, eligible := fence.Begin()
		if unlock != nil {
			unlock()
		}
		if eligible != (item.dir == realDir) {
			t.Errorf("canonical source check for %s returned %t", item.name, eligible)
		}
		if err := s.runtimeAuthSyncHook()(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if has("outside-callback.json") {
		t.Error("sibling outside the accepted directory registered")
	}
	outsideLink := filepath.Join(realDir, "linked-outside.json")
	if err := os.Symlink(filepath.Join(outside, "outside-callback.json"), outsideLink); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outsideLink)
	if err != nil {
		t.Fatal(err)
	}
	escape := &coreauth.Auth{ID: "linked-outside.json", Provider: "local-fixture",
		Attributes: map[string]string{coreauth.AttributePath: outsideLink}}
	escape.BindFileSnapshot(outsideLink, raw)
	unlock, eligible := s.persistedFilePublicationFence(escape).Begin()
	if unlock != nil {
		unlock()
	}
	if eligible {
		t.Error("a source symlink escaping the accepted directory was admitted")
	}
}
