package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUnregisteredLocalFileThroughGinDelete(t *testing.T) {
	for _, route := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		t.Run(route, func(t *testing.T) {
			dir := t.TempDir()
			m := coreauth.NewManager(nil, nil, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
			h.tokenStore = sdkauth.NewFileTokenStore()
			r := gin.New()
			r.DELETE("/v0/management/auth-files", h.DeleteAuthFile)
			r.DELETE("/v8/management/credentials", h.DeleteAuthFileV8)
			do := func(name string, want int) {
				t.Helper()
				out := httptest.NewRecorder()
				r.ServeHTTP(out, httptest.NewRequest(http.MethodDelete, route+"?name="+name, nil))
				if out.Code != want {
					t.Fatalf("delete %s: HTTP %d want %d: %s", name, out.Code, want, out.Body)
				}
			}
			path, other := filepath.Join(dir, "selected.json"), filepath.Join(dir, "other.json")
			ordinary := []byte(`{"note":"fixture"}`)
			for _, file := range []string{path, other} {
				if err := os.WriteFile(file, ordinary, 0600); err != nil {
					t.Fatal(err)
				}
			}
			release, err := m.BeginFileRemoval(path)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			do("selected.json", http.StatusConflict)
			do("other.json", http.StatusOK)
			release()
			do("selected.json", http.StatusOK)
			do("selected.json", http.StatusNotFound)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			do("selected.json", http.StatusConflict)
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				t.Fatal("directory was removed")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, ordinary, 0600); err != nil {
				t.Fatal(err)
			}
			replacement := &coreauth.Auth{ID: "selected.json", Attributes: map[string]string{coreauth.AttributePath: path}}
			replacement.BindFileSnapshot(path, ordinary)
			if got, err := m.Register(coreauth.WithSkipPersist(context.Background()), replacement); got == nil || err != nil {
				t.Fatalf("replacement registration: %v", err)
			}
		})
	}
}

func TestObservedLocalDeleteRejectsReplacementAndCancel(t *testing.T) {
	dir := t.TempDir()
	m := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
	name := "selected.json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"note":"first"}`), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "replacement")
	body := `{"note":"second"}`
	if err := os.WriteFile(second, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(second, path); err != nil {
		t.Fatal(err)
	}
	code, err := h.deleteObservedLocalAuth(context.Background(), dir, path, old, nil)
	if code != http.StatusConflict || err == nil {
		t.Fatalf("replacement: %d %v", code, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err = h.deleteObservedLocalAuth(ctx, dir, path, current, nil)
	if code != http.StatusRequestTimeout || err == nil {
		t.Fatalf("canceled: %d %v", code, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Fatalf("replacement lost: %s, %v", got, err)
	}
	newlyRegistered := &coreauth.Auth{ID: name, Attributes: map[string]string{coreauth.AttributePath: path}}
	if _, err := m.Register(coreauth.WithSkipPersist(context.Background()), newlyRegistered); err != nil {
		t.Fatal(err)
	}
	code, err = h.deleteObservedLocalAuth(context.Background(), dir, path, current, nil)
	if code != http.StatusConflict || !errors.Is(err, coreauth.ErrFileRemovalBusy) {
		t.Fatalf("newly indexed before reservation: %d %v", code, err)
	}
}

type pausedDeleteContext struct {
	context.Context
	entered chan struct{}
	resume  chan struct{}
	signal  sync.Once
}

func (c *pausedDeleteContext) Err() error {
	c.signal.Do(func() { close(c.entered) })
	<-c.resume
	return c.Context.Err()
}

func TestLocalGinDeleteKeepsReplacementWhileWaiting(t *testing.T) {
	dir := t.TempDir()
	m := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
	h.tokenStore = sdkauth.NewFileTokenStore()
	g := gin.New()
	g.DELETE("/v8/management/credentials", h.DeleteAuthFileV8)
	path := filepath.Join(dir, "selected.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := &pausedDeleteContext{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(ctx.resume) })
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(out, httptest.NewRequestWithContext(ctx, http.MethodDelete, "/v8/management/credentials?name=selected.json", nil))
	}()
	defer func() { unblock(); <-done }()
	select {
	case <-ctx.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not reach its pre-unlink check")
	}
	if release, err := m.BeginFileRemoval(path); !errors.Is(err, coreauth.ErrFileRemovalBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("route had no active source fence: %v", err)
	}
	replacement := filepath.Join(dir, "replacement")
	want := `{"note":"replacement"}`
	if err := os.WriteFile(replacement, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	unblock()
	<-done
	if out.Code != http.StatusConflict {
		t.Fatalf("replacement delete status=%d body=%s", out.Code, out.Body)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("replacement lost: %q, %v", data, err)
	}
}

func TestRegisteredSharedLocalDeleteThroughGin(t *testing.T) {
	for _, route := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		t.Run(route, func(t *testing.T) {
			dir := t.TempDir()
			m := coreauth.NewManager(nil, nil, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
			h.tokenStore = sdkauth.NewFileTokenStore()
			g := gin.New()
			g.DELETE("/v0/management/auth-files", h.DeleteAuthFile)
			g.DELETE("/v8/management/credentials", h.DeleteAuthFileV8)
			path, other := filepath.Join(dir, "selected.json"), filepath.Join(dir, "other.json")
			for _, f := range []string{path, other} {
				if err := os.WriteFile(f, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			register := func(id, file string) {
				t.Helper()
				if _, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Attributes: map[string]string{coreauth.AttributePath: file}}); err != nil {
					t.Fatal(err)
				}
			}
			ids := []string{"selected.json", "shared-member"}
			for _, id := range ids {
				register(id, path)
			}
			register("other.json", other)
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, route+"?name=selected.json", nil))
			if rec.Code != 200 {
				t.Fatalf("shared delete: %d %s", rec.Code, rec.Body)
			}
			for _, id := range ids {
				if _, ok := m.GetByID(id); ok {
					t.Fatalf("member %q survived", id)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source removal: %v", err)
			}
			if _, ok := m.GetByID("other.json"); !ok {
				t.Fatal("unrelated row retired")
			}
			if got, err := os.ReadFile(other); err != nil || string(got) != "{}" {
				t.Fatalf("other path: %q %v", got, err)
			}
		})
	}
}

func TestRegisteredLocalGinReplacementPreservesRuntime(t *testing.T) {
	dir := t.TempDir()
	m := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
	h.tokenStore = sdkauth.NewFileTokenStore()
	g := gin.New()
	g.DELETE("/v0/management/auth-files", h.DeleteAuthFile)
	path := filepath.Join(dir, "selected.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "{}")
	write(filepath.Join(dir, "other.json"), "{}")
	id := "selected.json"
	old, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Attributes: map[string]string{coreauth.AttributePath: path}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &pausedDeleteContext{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(ctx.resume) })
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(out, httptest.NewRequestWithContext(ctx, http.MethodDelete, "/v0/management/auth-files?name=selected.json", nil))
	}()
	defer func() { unblock(); <-done }()
	select {
	case <-ctx.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("registered delete never acquired source reservation")
	}
	other := httptest.NewRecorder()
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		g.ServeHTTP(other, httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name=other.json", nil))
	}()
	select {
	case <-otherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("delete of other path blocked")
	}
	if other.Code != http.StatusOK {
		t.Fatalf("other delete: %d", other.Code)
	}
	replacement := filepath.Join(dir, "next")
	want := `{"note":"replacement"}`
	write(replacement, want)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("registered delete did not finish")
	}
	if out.Code != http.StatusConflict {
		t.Fatalf("replacement delete %d %s", out.Code, out.Body)
	}
	if got, ok := m.GetByID(id); !ok || got.RegistrationEpoch != old.RegistrationEpoch {
		t.Fatal("failed disk commit changed runtime")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("replacement lost: %q %v", got, err)
	}
}
