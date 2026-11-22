package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestIndependentLocalStoreAcceptedGeneration(t *testing.T) {
	original := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(original) })
	var root sdkAuth.FileTokenStore
	sdkAuth.RegisterTokenStore(&root)
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	for dir, marker := range map[string]string{a: "a", b: "b", c: "c"} {
		contents := []byte(`{"type":"local-fixture","note":"` + marker + `"}`)
		if err := os.WriteFile(filepath.Join(dir, "same.json"), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	s, err := NewBuilder().WithConfig(&config.Config{AuthDir: a}).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).Build()
	if err != nil {
		t.Fatal(err)
	}
	assertLoad := func(want string) {
		t.Helper()
		if err := s.coreManager.Load(ctx); err != nil {
			t.Fatal(err)
		}
		auth, ok := s.coreManager.GetByID("same.json")
		if !ok || auth.Metadata["note"] != want {
			t.Fatalf("loaded stale directory, expected %s", want)
		}
	}
	assertLoad("a")
	bad := filepath.Join(t.TempDir(), "not-directory")
	if err := os.WriteFile(bad, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.commitConfigUpdateContext(ctx, &config.Config{AuthDir: bad}); got.cfg != nil || s.currentConfig().AuthDir != a {
		t.Fatal("invalid directory became accepted")
	}
	assertLoad("a")
	if got := s.commitConfigUpdateContext(ctx, &config.Config{}); got.cfg != nil || s.currentConfig().AuthDir != a {
		t.Fatal("empty local directory became accepted")
	}
	assertLoad("a")
	canceled, stop := context.WithCancel(ctx)
	stop()
	if got := s.commitConfigUpdateContext(canceled, &config.Config{AuthDir: b}); got.cfg != nil || s.currentConfig().AuthDir != a {
		t.Fatal("canceled config became accepted")
	}
	nonfile := &coreauth.Auth{ID: "memory-only", Provider: "local-fixture"}
	if _, err := s.coreManager.Register(coreauth.WithSkipPersist(ctx), nonfile); err != nil {
		t.Fatal(err)
	}
	held, release, gateDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	paused := pausedGenerationStore{s.localStoreRoot.WithBaseDir(a), held, release}
	if err := s.coreManager.WithLocalStoreRebind(ctx, paused, func(install func()) { install() }); err != nil {
		t.Fatal(err)
	}
	go func() { gateDone <- s.coreManager.Load(ctx) }()
	<-held
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	currentRequest := func() uint64 {
		s.configUpdateMu.Lock()
		defer s.configUpdateMu.Unlock()
		return s.localStoreRequest
	}
	waitRequest := func(want uint64) {
		t.Helper()
		for currentRequest() < want {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	start := currentRequest()
	doneB, doneC := make(chan configCommit, 1), make(chan configCommit, 1)
	go func() { doneB <- s.commitConfigUpdateContext(ctx, &config.Config{AuthDir: b}) }()
	waitRequest(start + 1)
	go func() { doneC <- s.commitConfigUpdateContext(ctx, &config.Config{AuthDir: c}) }()
	waitRequest(start + 2)
	if s.currentConfig().AuthDir != a {
		t.Fatal("published a directory while the old load gate was held")
	}
	close(release)
	if err := <-gateDone; err != nil {
		t.Fatal(err)
	}
	if first, final := <-doneB, <-doneC; first.cfg != nil || final.cfg == nil || s.currentConfig().AuthDir != c {
		t.Fatal("superseded B or C did not finish as expected")
	}
	assertLoad("c")
	if _, err := s.coreManager.Register(coreauth.WithSkipPersist(ctx), nonfile); err != nil {
		t.Fatal(err)
	}
	if got := s.commitConfigUpdateContext(ctx, &config.Config{AuthDir: b}); got.cfg == nil {
		t.Fatal("retry did not accept B")
	}
	if _, ok := s.coreManager.GetByID(nonfile.ID); !ok {
		t.Fatal("rebind cleared a non-file registration")
	}
	assertLoad("b")
}

func TestIndependentLocalStoreExternalManagerUnchanged(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	dir := t.TempDir()
	s, err := NewBuilder().WithConfig(&config.Config{AuthDir: dir}).WithConfigPath(filepath.Join(dir, "config.yaml")).WithCoreAuthManager(m).Build()
	if err != nil {
		t.Fatal(err)
	}
	if s.localStoreRoot != nil || s.coreManager != m {
		t.Fatal("external manager was claimed")
	}
}

type pausedGenerationStore struct {
	coreauth.Store
	held, release chan struct{}
}

func (p pausedGenerationStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	items, err := p.Store.List(ctx)
	close(p.held)
	select {
	case <-p.release:
		return items, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
