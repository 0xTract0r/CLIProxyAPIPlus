package auth

import (
	"context"
	"errors"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type refresh269Shutdown interface{ ShutdownAutoRefresh(context.Context) error }

func shutdown269(m *Manager, ctx context.Context) error {
	if q, ok := any(m).(refresh269Shutdown); ok {
		return q.ShutdownAutoRefresh(ctx)
	}
	m.StopAutoRefresh()
	return nil
}

type refresh269Executor struct {
	ProviderExecutor
	started, cancelled, release chan struct{}
}

func (*refresh269Executor) Identifier() string { return "refresh269" }
func (e *refresh269Executor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	close(e.started)
	<-ctx.Done()
	close(e.cancelled)
	<-e.release
	a.Metadata["access_token"] = "rotated269"
	return a, nil
}

type refresh269Evaluator struct{}

func (refresh269Evaluator) ShouldRefresh(time.Time, *Auth) bool { return true }

type refresh269Store struct {
	Store
	path             string
	started, release chan struct{}
}

func (s *refresh269Store) Save(_ context.Context, a *Auth) (string, error) {
	if a.Metadata["access_token"] == "rotated269" {
		close(s.started)
		<-s.release
	}
	return s.path, os.WriteFile(s.path, []byte(a.Metadata["access_token"].(string)), 0600)
}

type refresh269Hook struct {
	NoopHook
	manager                  *Manager
	started, release, exited chan struct{}
	selfErr                  chan error
}

func (h *refresh269Hook) OnAuthUpdated(ctx context.Context, _ *Auth) {
	h.manager.StopAutoRefresh() // The existing asynchronous API remains callback-safe.
	if q, ok := any(h.manager).(refresh269Shutdown); ok {
		h.selfErr <- q.ShutdownAutoRefresh(ctx)
	} else {
		h.selfErr <- nil
	}
	close(h.started)
	<-h.release
	close(h.exited)
}
func TestAutoRefresh269WaitOwnsRetiredWorkersPersistenceAndHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	synctest.Test(t, func(t *testing.T) {
		exec := &refresh269Executor{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
		store := &refresh269Store{path: path, started: make(chan struct{}), release: make(chan struct{})}
		hook := &refresh269Hook{started: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}), selfErr: make(chan error, 1)}
		m := NewManager(store, nil, hook)
		hook.manager = m
		m.RegisterExecutor(exec)
		a, err := m.Register(context.Background(), &Auth{ID: "refresh269", Provider: "refresh269", Metadata: map[string]any{"access_token": "old269"}, Runtime: refresh269Evaluator{}})
		if err != nil {
			t.Fatal(err)
		}
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		m.StartAutoRefresh(parent, time.Second)
		<-exec.started
		stopped := make(chan struct{})
		go func() { m.StopAutoRefresh(); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Error("legacy StopAutoRefresh blocked on its worker")
		}
		<-exec.cancelled
		// A replacement generation must not hide the old, already cancelled worker.
		m.StartAutoRefresh(parent, time.Second)
		deadline, expire := context.WithCancel(context.Background())
		wait := make(chan error, 1)
		go func() { wait <- shutdown269(m, deadline) }()
		synctest.Wait()
		premature := false
		select {
		case err := <-wait:
			t.Errorf("shutdown returned before rotated-token persistence: %v", err)
			premature = true
		default:
		}
		expire()
		synctest.Wait()
		if !premature {
			if err := <-wait; !errors.Is(err, context.Canceled) {
				t.Errorf("bounded wait=%v, want canceled", err)
			}
		}
		complete := make(chan error, 1)
		go func() { complete <- shutdown269(m, context.Background()) }()
		close(exec.release)
		<-store.started
		synctest.Wait()
		completed := false
		select {
		case err := <-complete:
			t.Errorf("shutdown returned during store Save: %v", err)
			completed = true
		default:
		}
		close(store.release)
		<-hook.started
		synctest.Wait()
		if err := <-hook.selfErr; err == nil {
			t.Error("refresh callback could join its own worker")
		}
		if !completed {
			select {
			case err := <-complete:
				t.Errorf("shutdown returned during OnAuthUpdated: %v", err)
				completed = true
			default:
			}
		}
		close(hook.release)
		<-hook.exited
		synctest.Wait()
		if !completed {
			if err := <-complete; err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "rotated269" {
			t.Errorf("persisted token=%q err=%v", got, err)
		}
		current, ok := m.GetByID(a.ID)
		if !ok || current.RegistrationEpoch != a.RegistrationEpoch || current.Metadata["access_token"] != "rotated269" {
			t.Errorf("successful cancelled refresh lost token/epoch: %#v", current)
		}
		cancel()
		synctest.Wait()
	})
}
func TestAutoRefresh269ConcurrentStartStopAndClosedAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls sync.WaitGroup
		for i := 0; i < 32; i++ {
			calls.Add(1)
			go func() { defer calls.Done(); m.StartAutoRefresh(parent, time.Second); m.StopAutoRefresh() }()
		}
		calls.Wait()
		if err := shutdown269(m, context.Background()); err != nil {
			t.Fatal(err)
		}
		m.StartAutoRefresh(parent, time.Second)
		m.mu.RLock()
		late := m.refreshLoop != nil
		m.mu.RUnlock()
		if late {
			t.Error("terminal auto-refresh shutdown admitted a new generation")
		}
		cancel()
		m.StopAutoRefresh()
		synctest.Wait()
	})
}

func TestAutoRefresh269CancellationReleasesQueuedBeforeRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		m.runtimeConfig.Store(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
		exec := &refresh269Executor{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
		m.RegisterExecutor(exec)
		running, err := m.Register(context.Background(), &Auth{ID: "running269", Provider: "refresh269", Metadata: map[string]any{"access_token": "old269"}, Runtime: refresh269Evaluator{}})
		if err != nil {
			t.Fatal(err)
		}
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		m.StartAutoRefresh(parent, time.Second)
		<-exec.started
		queued, err := m.Register(context.Background(), &Auth{ID: "queued269", Provider: "refresh269", Metadata: map[string]any{"access_token": "queued269"}})
		if err != nil {
			t.Fatal(err)
		}
		m.mu.RLock()
		loop := m.refreshLoop
		m.mu.RUnlock()
		job := m.markRefreshPending(loop, queued.ID, queued.RegistrationEpoch, time.Now())
		if job == nil {
			t.Fatal("queued job was not admitted")
		}
		loop.jobs <- job
		m.StopAutoRefresh()
		<-exec.cancelled
		synctest.Wait()
		m.mu.RLock()
		queuedRetained := m.refreshJobs[queued.ID] != nil
		runningOwned := m.refreshJobs[running.ID] != nil && m.refreshJobs[running.ID].running
		pending := m.auths[queued.ID].NextRefreshAfter
		m.mu.RUnlock()
		if queuedRetained || !pending.IsZero() {
			t.Errorf("cancelled queued job retained behind running worker: retained=%t pending=%v", queuedRetained, pending)
		}
		if !runningOwned {
			t.Error("running worker lost ownership before completion")
		}
		close(exec.release)
		if err := shutdown269(m, context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
	})
}
