package cliproxy

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type serviceQuotaStore291 struct {
	*fileauth.FileTokenStore
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *serviceQuotaStore291) Save(c context.Context, a *coreauth.Auth) (string, error) {
	if s.hold.Load() {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.FileTokenStore.Save(c, a)
}
func TestServiceQuotaQuiescence291RetryBeforeCleanup(t *testing.T) {
	st := &serviceQuotaStore291{FileTokenStore: fileauth.NewFileTokenStore(), entered: make(chan struct{}), release: make(chan struct{})}
	st.SetBaseDir(t.TempDir())
	m := coreauth.NewManager(st, nil, nil)
	_, e := m.Register(context.Background(), &coreauth.Auth{ID: "service-quota.json", FileName: "service-quota.json", Provider: "codex", ProxyURL: "direct", Metadata: map[string]any{"type": "codex", "quota_next_refresh_after": "2099-01-01T00:00:00Z", "quota_snapshot": map[string]any{}}})
	if e != nil {
		t.Fatal(e)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	defer release()
	st.hold.Store(true)
	server := api.NewServer(&config.Config{AuthDir: t.TempDir(), Host: "127.0.0.1"}, m, nil, "")
	s := &Service{server: server}
	cleanups := 0
	s.authQueueStop = func() { cleanups++ }
	<-st.entered
	expired, c := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer c()
	err := s.Shutdown(expired)
	if !errors.Is(err, context.DeadlineExceeded) || cleanups != 0 {
		t.Errorf("nonquiescent shutdown err=%v cleanups=%d", err, cleanups)
	}
	release()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls=%d", cleanups)
	}
}
