package management

import (
	"context"
	"errors"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type heldQuota291 struct {
	*fileauth.FileTokenStore
	hold    atomic.Bool
	entered chan context.Context
	release chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (s *heldQuota291) Save(ctx context.Context, a *coreauth.Auth) (string, error) {
	if s.hold.Load() {
		s.entered <- ctx
		<-s.release
		p, e := s.FileTokenStore.Save(ctx, a)
		s.once.Do(func() { close(s.exited) })
		return p, e
	}
	return s.FileTokenStore.Save(ctx, a)
}
func joinQuota291(h *Handler, c context.Context) error {
	if api, ok := any(h).(interface{ ShutdownQuotaSnapshotAutoRefresh(context.Context) error }); ok {
		return api.ShutdownQuotaSnapshotAutoRefresh(c)
	}
	return errors.New("missing quota join")
}
func TestQuotaQuiescence291HeldSaveRetiredRetry(t *testing.T) {
	st := &heldQuota291{FileTokenStore: fileauth.NewFileTokenStore(), entered: make(chan context.Context, 4), release: make(chan struct{}), exited: make(chan struct{})}
	st.SetBaseDir(t.TempDir())
	m := coreauth.NewManager(st, nil, nil)
	_, e := m.Register(context.Background(), &coreauth.Auth{ID: "held.json", FileName: "held.json", Provider: "codex", ProxyURL: "direct", Metadata: map[string]any{"type": "codex", "quota_next_refresh_after": "2099-01-01T00:00:00Z", "quota_snapshot": map[string]any{}}})
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandlerWithoutConfigFilePath(nil, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	defer release()
	st.hold.Store(true)
	policy := QuotaSnapshotRefreshPolicy{Enabled: true, Interval: time.Hour, Jitter: time.Minute}
	h.StartQuotaSnapshotAutoRefresh(ctx, policy)
	workerCtx := <-st.entered
	h.StartQuotaSnapshotAutoRefresh(ctx, QuotaSnapshotRefreshPolicy{Enabled: false})
	h.StartQuotaSnapshotAutoRefresh(ctx, policy)
	h.mu.Lock()
	v := reflect.ValueOf(h).Elem().FieldByName("quotaRefreshRuns")
	tracked := 0
	if v.IsValid() {
		tracked = v.Len()
	}
	h.mu.Unlock()
	if tracked < 2 {
		t.Errorf("active and retired generations not tracked: %d", tracked)
	}
	expired, c := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer c()
	if err := joinQuota291(h, expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("held Save join=%v", err)
	}
	if err := joinQuota291(h, workerCtx); err == nil || !strings.Contains(err.Error(), "own generation") {
		t.Errorf("self join not rejected: %v", err)
	}
	h.StartQuotaSnapshotAutoRefresh(ctx, policy)
	release()
	<-st.exited
	if err := joinQuota291(h, context.Background()); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	v = reflect.ValueOf(h).Elem().FieldByName("quotaRefreshRuns")
	if v.IsValid() && v.Len() != 0 {
		t.Error("owners retained after join")
	}
	closed := reflect.ValueOf(h).Elem().FieldByName("quotaRefreshClosed")
	if !closed.IsValid() || !closed.Bool() {
		t.Error("late admission not closed")
	}
	h.mu.Unlock()
	h.StartQuotaSnapshotAutoRefresh(ctx, policy)
	if err := joinQuota291(h, context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Separator
}
