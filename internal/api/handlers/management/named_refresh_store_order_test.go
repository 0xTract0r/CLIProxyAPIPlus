package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Mirrors the common Git/S3/PG mutex topology, but all bytes stay in a temp
// FileTokenStore. No external client or backend is constructed.
type orderedRefreshUploadStore struct {
	coreauth.Store
	mu             sync.Mutex
	uploadLocked   chan struct{}
	installAllowed chan struct{}
	saveWaiting    chan struct{}
	remoteCalls    int
}

func (s *orderedRefreshUploadStore) Save(ctx context.Context, a *coreauth.Auth) (string, error) {
	close(s.saveWaiting)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.Save(ctx, a)
}
func (s *orderedRefreshUploadStore) PersistAuthUpload(_ context.Context, _ string, _ []byte, install func(func() error) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.uploadLocked)
	<-s.installAllowed
	return install(func() error { s.remoteCalls++; return nil })
}
func TestNamedRefreshUploadStoreOrder(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	f := newNamedRefreshFixture(t)
	s := &orderedRefreshUploadStore{Store: f.base, uploadLocked: make(chan struct{}), installAllowed: make(chan struct{}), saveWaiting: make(chan struct{})}
	manager := coreauth.NewManager(s, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal("load")
	}
	manager.RegisterExecutor(f.exec)
	f.manager = manager
	f.handler.SetAuthManager(manager)
	f.handler.tokenStore = s
	other := coreauth.NewManager(f.base, nil, nil)
	if err := other.Load(context.Background()); err != nil {
		t.Fatal("other load")
	}
	const name = "ordinary-upload.json"
	path := filepath.Join(f.dir, name)
	sentinel := []byte(`{"type":"antigravity","note":"preserve-upload-before-image"}`)
	if err := os.WriteFile(path, sentinel, 0600); err != nil {
		t.Fatal("sentinel")
	}
	ctx := f.handler.withAuthUploadSelection(context.Background())
	target, err := openAuthUploadTarget(f.dir, name)
	if err != nil {
		t.Fatal("target")
	}
	defer target.root.Close()
	target.selection = ctx.Value(authUploadSelectionKey{}).(authUploadSelection)
	uploaded := make(chan error, 1)
	go func() {
		uploaded <- f.handler.commitUploadedAuth(ctx, target, []byte(`{"type":"antigravity","note":"new-upload"}`), false)
	}()
	select {
	case <-s.uploadLocked:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not acquire store")
	}
	refreshed := make(chan int, 1)
	go func() { refreshed <- f.request(t, true, "json", true).Code }()
	select {
	case <-s.saveWaiting:
	case <-time.After(2 * time.Second):
		close(s.installAllowed)
		t.Fatal("refresh did not reach store Save")
	}
	// The refresh now owns its scope reader and waits for the upload's store.
	reloadDone := make(chan struct{})
	go func() { f.handler.SetAuthManager(other); close(reloadDone) }()
	deadline := time.Now().Add(time.Second)
	pending := false
	for time.Now().Before(deadline) {
		if !f.handler.authUploadScopeMu.TryRLock() {
			pending = true
			break
		}
		f.handler.authUploadScopeMu.RUnlock()
		time.Sleep(time.Millisecond)
	}
	if !pending {
		close(s.installAllowed)
		t.Fatal("reload not queued on scope writer")
	}
	close(s.installAllowed)
	select {
	case err := <-uploaded:
		if !errors.Is(err, coreauth.ErrAuthPublicationOutOfScope) {
			t.Error("upload did not reject busy scope")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store->scope/upload deadlock")
	}
	select {
	case <-reloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reload deadlock")
	}
	select {
	case code := <-refreshed:
		if code != 409 {
			t.Errorf("refresh after reload HTTP=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh deadlock")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(sentinel) {
		t.Error("rejected upload changed before-image")
	}
	if s.remoteCalls != 0 {
		t.Error("scope refusal reached remote commit")
	}
	// A locked installer must also return promptly when the scope writer owns the lock.
	f.handler.authUploadScopeMu.Lock()
	done := make(chan bool, 1)
	go func() {
		release, ok := target.selection.tryBegin()
		if ok {
			release()
		}
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("busy selection admitted")
		}
	case <-time.After(time.Second):
		f.handler.authUploadScopeMu.Unlock()
		t.Fatal("selection callback blocked")
	}
	f.handler.authUploadScopeMu.Unlock()
}

// A remotely confirmed install rejected at the final scope gate stays pending;
// it is not reported as an unconfirmed remote failure or as success.
func TestUploadConfirmedBusyScopeKeepsPending(t *testing.T) {
	f := newNamedRefreshFixture(t)
	const name = "ordinary-confirmed.json"
	path := filepath.Join(f.dir, name)
	before := []byte(`{"type":"antigravity","note":"before"}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal("before")
	}
	ctx := f.handler.withAuthUploadSelection(context.Background())
	target, err := openAuthUploadTarget(f.dir, name)
	if err != nil {
		t.Fatal("target")
	}
	defer target.root.Close()
	target.selection = ctx.Value(authUploadSelectionKey{}).(authUploadSelection)
	locked := make(chan struct{})
	backend := &scopedUploadTestStore{onRemote: func() { f.handler.authUploadScopeMu.Lock(); close(locked) }}
	f.handler.tokenStore = backend
	done := make(chan error, 1)
	go func() {
		done <- f.handler.commitUploadedAuth(ctx, target, []byte(`{"type":"antigravity","note":"confirmed"}`), false)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("remote confirmation not reached")
	}
	var result error
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		f.handler.authUploadScopeMu.Unlock()
		t.Fatal("final publication waited on scope writer")
	}
	f.handler.authUploadScopeMu.Unlock()
	if !errors.Is(result, coreauth.ErrAuthUploadLocalPending) || !errors.Is(result, coreauth.ErrAuthPublicationOutOfScope) {
		t.Error("confirmed upload lost local-pending/scope cause")
	}
	code, _ := authUploadFailure(result)
	if code != 503 {
		t.Errorf("pending HTTP=%d", code)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || backend.attempts != 1 {
		t.Error("pending upload changed old local image or remote commit count")
	}
}

func TestUploadUnlockedChecksWaitSameGeneration(t *testing.T) {
	f := newNamedRefreshFixture(t)
	ctx := f.handler.withAuthUploadSelection(context.Background())
	selection := ctx.Value(authUploadSelectionKey{}).(authUploadSelection)
	f.handler.authUploadScopeMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- selection.check() }()
	<-started
	select {
	case <-done:
		f.handler.authUploadScopeMu.Unlock()
		t.Fatal("unlocked check rejected same-generation writer")
	case <-time.After(20 * time.Millisecond):
	}
	f.handler.authUploadScopeMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Error("same generation did not resume")
		}
	case <-time.After(time.Second):
		t.Fatal("unlocked check did not resume")
	}
}

func TestUploadPlainUnlockedInstallerWaits(t *testing.T) {
	f := newNamedRefreshFixture(t)
	ctx := f.handler.withAuthUploadSelection(context.Background())
	target, err := openAuthUploadTarget(f.dir, "ordinary-unlocked.json")
	if err != nil {
		t.Fatal("target")
	}
	defer target.root.Close()
	target.selection = ctx.Value(authUploadSelectionKey{}).(authUploadSelection)
	payload := []byte(`{"type":"antigravity","note":"same-generation-file-store"}`)
	f.handler.authUploadScopeMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- f.handler.commitUploadedAuth(ctx, target, payload, false) }()
	<-started
	select {
	case <-done:
		f.handler.authUploadScopeMu.Unlock()
		t.Fatal("plain unlocked installer did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	f.handler.authUploadScopeMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Error("plain installer rejected same-generation reload")
		}
	case <-time.After(time.Second):
		t.Fatal("plain installer did not resume")
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "ordinary-unlocked.json"))
	if err != nil || string(after) != string(payload) {
		t.Error("plain installer bytes differ")
	}
}
