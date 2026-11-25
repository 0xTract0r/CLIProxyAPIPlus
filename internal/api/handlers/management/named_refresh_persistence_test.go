package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type namedRefreshFailStore struct {
	coreauth.Store
	fail       atomic.Bool
	beforeSave func()
	storeErr   error
	onSave     func(*coreauth.Auth)
}

func (s *namedRefreshFailStore) Save(ctx context.Context, a *coreauth.Auth) (string, error) {
	if s.onSave != nil {
		s.onSave(a)
	}
	if s.beforeSave != nil {
		s.beforeSave()
	}
	if s.storeErr != nil {
		return "", s.storeErr
	}
	if s.fail.Load() {
		return "", errors.New("synthetic store write refused")
	}
	return s.Store.Save(ctx, a)
}

type namedRefreshExecutor struct {
	refreshRecordExecutor
	before func(*coreauth.Auth)
}

func (e *namedRefreshExecutor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	if e.before != nil {
		e.before(a)
	}
	copy := a.Clone()
	copy.Metadata = make(map[string]any, len(a.Metadata))
	for k, v := range a.Metadata {
		copy.Metadata[k] = v
	}
	return e.refreshRecordExecutor.Refresh(ctx, copy)
}

type namedRefreshFixture struct {
	handler *Handler
	manager *coreauth.Manager
	store   *namedRefreshFailStore
	base    *sdkauth.FileTokenStore
	exec    *namedRefreshExecutor
	dir, id string
}

func newNamedRefreshFixture(t *testing.T) *namedRefreshFixture {
	t.Helper()
	dir := t.TempDir()
	const id = "ordinary-named.json"
	if err := os.WriteFile(filepath.Join(dir, id), []byte(`{"type":"antigravity","access_token":"old-private-fixture","refresh_token":"old-refresh-fixture","note":"keep-normal-field"}`), 0600); err != nil {
		t.Fatal("fixture write failed")
	}
	base := sdkauth.NewFileTokenStore().WithBaseDir(dir)
	store := &namedRefreshFailStore{Store: base}
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal("fixture load failed")
	}
	records := manager.List()
	if len(records) != 1 {
		t.Fatal("expected one normal fixture")
	}
	idActual := records[0].ID
	exec := &namedRefreshExecutor{refreshRecordExecutor: refreshRecordExecutor{provider: "antigravity"}}
	manager.RegisterExecutor(exec)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.SetLocalPassword("named-admin-fixture")
	h.tokenStore = base
	return &namedRefreshFixture{h, manager, store, base, exec, dir, idActual}
}
func (f *namedRefreshFixture) request(t *testing.T, v8 bool, form string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	path := "/v0/management/auth-files/refresh"
	if v8 {
		path = "/v8/management/credentials/refresh"
	}
	r.POST(path, f.handler.Middleware(), func(c *gin.Context) { c.Set(ConfigV8ContextKey, v8); f.handler.RefreshAuthFiles(c) })
	body := `{"name":"ordinary-named.json"}`
	url := path
	if form == "query" {
		body = ""
		url += "?name=ordinary-named.json"
	}
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:18000"
	req.Header.Set("Content-Type", "application/json")
	if form == "chunked" {
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
	}
	if authorized {
		req.Header.Set("Authorization", "Bearer named-admin-fixture")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func namedRefreshToken(t *testing.T, records []*coreauth.Auth, id string) string {
	t.Helper()
	for _, a := range records {
		if a != nil && a.ID == id {
			v, _ := a.Metadata["access_token"].(string)
			return v
		}
	}
	t.Fatal("missing ordinary fixture")
	return ""
}
func (f *namedRefreshFixture) tokens(t *testing.T) (string, string) {
	t.Helper()
	cold := sdkauth.NewFileTokenStore().WithBaseDir(f.dir)
	list, err := cold.List(context.Background())
	if err != nil {
		t.Fatal("cold read failed")
	}
	return namedRefreshToken(t, f.manager.List(), f.id), namedRefreshToken(t, list, f.id)
}
func TestNamedRefreshPersistenceAndVersion(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	for _, v8 := range []bool{false, true} {
		name := "v0"
		if v8 {
			name = "v8"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("write_failure", func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				f.store.fail.Store(true)
				w := f.request(t, v8, "json", true)
				ram, disk := f.tokens(t)
				if w.Code != 500 || ram != "refreshed-token" || disk != "old-private-fixture" {
					t.Errorf("manual write refusal: HTTP=%d RAM-updated=%t cold-updated=%t", w.Code, ram != "old-private-fixture", disk != "old-private-fixture")
				}
				if f.exec.refreshCnt.Load() != 1 {
					t.Error("expected exactly one mock refresh")
				}
				f.store.fail.Store(false)
				current, _ := f.manager.GetByID(f.id)
				if _, err := f.manager.Update(context.Background(), current); err != nil {
					t.Fatal("local recovery save failed")
				}
				ram, disk = f.tokens(t)
				if ram != "refreshed-token" || disk != ram || f.exec.refreshCnt.Load() != 1 {
					t.Error("rotated grant not recoverable by local save alone")
				}

			})
			t.Run("replacement", func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				f.exec.before = func(a *coreauth.Auth) {
					replacement := a.Clone()
					replacement.Metadata = map[string]any{"type": "antigravity", "access_token": "replacement-private-fixture", "refresh_token": "replacement-refresh-fixture", "note": "replacement-wins"}
					if _, err := f.manager.Update(context.Background(), replacement); err != nil {
						t.Error("replacement failed")
					}
				}
				w := f.request(t, v8, "json", true)
				ram, disk := f.tokens(t)
				if w.Code != 409 || ram != "replacement-private-fixture" || disk != "replacement-private-fixture" {
					t.Errorf("replaced observed grant: HTTP=%d replacement-RAM=%t replacement-cold=%t", w.Code, ram == "replacement-private-fixture", disk == "replacement-private-fixture")
				}
			})
			t.Run("manager_swap", func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				other := coreauth.NewManager(nil, nil, nil)
				otherExec := &namedRefreshExecutor{refreshRecordExecutor: refreshRecordExecutor{provider: "antigravity"}}
				other.RegisterExecutor(otherExec)
				record, _ := f.manager.GetByID(f.id)
				if _, err := other.Register(coreauth.WithSkipPersist(context.Background()), record); err != nil {
					t.Fatal("other manager fixture failed")
				}
				f.exec.before = func(*coreauth.Auth) { f.handler.SetAuthManager(other) }
				w := f.request(t, v8, "json", true)
				if w.Code != 409 {
					t.Errorf("changed handler manager: HTTP=%d", w.Code)
				}
				if otherExec.refreshCnt.Load() != 0 {
					t.Error("request retargeted to replacement manager")
				}
				if namedRefreshToken(t, other.List(), f.id) != "old-private-fixture" {
					t.Error("replacement manager changed")
				}
			})
			t.Run("manager_shared_file", func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				other := coreauth.NewManager(f.store, nil, nil)
				if err := other.Load(context.Background()); err != nil {
					t.Fatal("replacement load failed")
				}
				f.exec.before = func(*coreauth.Auth) {
					f.handler.SetAuthManager(other)
					replacement, _ := other.GetByID(f.id)
					replacement.Metadata = map[string]any{"type": "antigravity", "access_token": "replacement-private-fixture", "refresh_token": "replacement-refresh-fixture"}
					if _, err := other.Update(context.Background(), replacement); err != nil {
						t.Error("replacement persist failed")
					}
				}
				w := f.request(t, v8, "json", true)
				cold, err := f.base.List(context.Background())
				if err != nil {
					t.Fatal("cold read failed")
				}
				if w.Code != 409 || namedRefreshToken(t, other.List(), f.id) != "replacement-private-fixture" || namedRefreshToken(t, cold, f.id) != "replacement-private-fixture" {
					t.Errorf("shared-file late write: HTTP=%d replacement-cold=%t", w.Code, namedRefreshToken(t, cold, f.id) == "replacement-private-fixture")
				}
			})
			for _, form := range []string{"query", "json", "chunked"} {
				t.Run("success_"+form, func(t *testing.T) {
					f := newNamedRefreshFixture(t)
					w := f.request(t, v8, form, true)
					ram, disk := f.tokens(t)
					if w.Code != 200 || ram != "refreshed-token" || ram != disk {
						t.Errorf("durable success: HTTP=%d RAM-cold-equal=%t", w.Code, ram == disk)
					}
					if v8 && strings.Contains(w.Body.String(), "refreshed-token") {
						t.Error("v8 exposed grant")
					}
					if !v8 && !strings.Contains(w.Body.String(), "refreshed-token") {
						t.Error("legacy v0 response contract changed")
					}
				})
			}
			t.Run("unauthorized", func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				w := f.request(t, v8, "json", false)
				if w.Code != 401 || f.exec.refreshCnt.Load() != 0 {
					t.Error("management authorization bypass")
				}
			})
		})
	}
}

// The replacement manager uses the same physical store. A publisher change
// must complete while the mock provider is paused, then fence its late result.
func TestNamedRefreshCommitScope(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	for _, v8 := range []bool{false, true} {
		name := "v0"
		if v8 {
			name = "v8"
		}
		t.Run(name, func(t *testing.T) {
			for _, sub := range []string{"shared_store", "manager_ABA", "directory_ABA"} {
				t.Run(sub, func(t *testing.T) {
					f := newNamedRefreshFixture(t)
					other := coreauth.NewManager(f.store, nil, nil)
					if err := other.Load(context.Background()); err != nil {
						t.Fatal("load")
					}
					entered, release := make(chan struct{}), make(chan struct{})
					f.exec.before = func(*coreauth.Auth) { close(entered); <-release }
					done := make(chan *httptest.ResponseRecorder, 1)
					go func() { done <- f.request(t, v8, "json", true) }()
					select {
					case <-entered:
					case <-time.After(2 * time.Second):
						close(release)
						t.Fatal("mock provider did not start")
					}
					changed := make(chan struct{})
					otherDir := t.TempDir()
					go func() {
						if sub == "directory_ABA" {
							f.handler.SetConfig(&config.Config{AuthDir: otherDir})
							f.handler.SetConfig(&config.Config{AuthDir: f.dir})
						} else {
							f.handler.SetAuthManager(other)
							if sub == "manager_ABA" {
								f.handler.SetAuthManager(f.manager)
							}
						}
						close(changed)
					}()
					select {
					case <-changed:
					case <-time.After(time.Second):
						close(release)
						t.Fatal("manager setter blocked by provider I/O")
					}
					replacement, _ := other.GetByID(f.id)
					replacement.Metadata = map[string]any{"type": "antigravity", "access_token": "replacement-private-fixture", "refresh_token": "replacement-refresh-fixture"}
					if _, err := other.Update(context.Background(), replacement); err != nil {
						close(release)
						t.Fatal("replacement write")
					}
					close(release)
					select {
					case w := <-done:
						if w.Code != 409 {
							t.Errorf("changed manager HTTP=%d", w.Code)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("late completion blocked")
					}
					cold, err := f.base.List(context.Background())
					if err != nil {
						t.Fatal("cold read")
					}
					if namedRefreshToken(t, cold, f.id) != "replacement-private-fixture" {
						t.Error("late result overwrote replacement file")
					}
				})
			}
		})
	}
}

func TestNamedRefreshStoreLease(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	f := newNamedRefreshFixture(t)
	other := coreauth.NewManager(f.store, nil, nil)
	if err := other.Load(context.Background()); err != nil {
		t.Fatal("load")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	f.store.beforeSave = func() { close(entered); <-release }
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.request(t, true, "json", true) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("save not reached")
	}
	started, changed := make(chan struct{}), make(chan struct{})
	go func() { close(started); f.handler.SetAuthManager(other); close(changed) }()
	<-started
	select {
	case <-changed:
		close(release)
		t.Fatal("manager changed during in-flight store commit")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("setter stayed blocked after store commit")
	}
	select {
	case w := <-done:
		if w.Code != 200 && w.Code != 409 {
			t.Errorf("unexpected completion HTTP=%d", w.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request stuck")
	}
	f.store.beforeSave = nil
	replacement, _ := other.GetByID(f.id)
	replacement.Metadata = map[string]any{"type": "antigravity", "access_token": "replacement-private-fixture", "refresh_token": "replacement-refresh-fixture"}
	if _, err := other.Update(context.Background(), replacement); err != nil {
		t.Fatal("replacement")
	}
	cold, err := f.base.List(context.Background())
	if err != nil {
		t.Fatal("cold")
	}
	if namedRefreshToken(t, cold, f.id) != "replacement-private-fixture" {
		t.Error("replacement file lost")
	}
}

func TestNamedRefreshFinalBoundaries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	conflicts := []struct {
		name string
		err  error
	}{
		{"removed", coreauth.ErrCredentialFileRemoved}, {"removal_busy", coreauth.ErrFileRemovalBusy},
		{"publication_scope", coreauth.ErrAuthPublicationOutOfScope}, {"payload_owner", coreauth.ErrAuthUploadPayloadConflict},
	}
	for _, v8 := range []bool{false, true} {
		version := "v0"
		if v8 {
			version = "v8"
		}
		for _, conflict := range conflicts {
			t.Run(version+"_"+conflict.name, func(t *testing.T) {
				f := newNamedRefreshFixture(t)
				before, err := os.ReadFile(filepath.Join(f.dir, "ordinary-named.json"))
				if err != nil {
					t.Fatal("before read")
				}
				f.store.storeErr = fmt.Errorf("synthetic wrapped conflict: %w", conflict.err)
				w := f.request(t, v8, "json", true)
				ram, cold := f.tokens(t)
				after, err := os.ReadFile(filepath.Join(f.dir, "ordinary-named.json"))
				if err != nil {
					t.Fatal("after read")
				}
				if w.Code != 409 || ram != "old-private-fixture" || cold != "old-private-fixture" || string(before) != string(after) {
					t.Errorf("typed conflict HTTP=%d stale-RAM=%t file-unchanged=%t", w.Code, ram != "old-private-fixture", string(before) == string(after))
				}
				if f.exec.refreshCnt.Load() != 1 {
					t.Error("unexpected issuer call count")
				}
			})
		}
	}
	t.Run("nil_context", func(t *testing.T) {
		f := newNamedRefreshFixture(t)
		observed, _ := f.manager.GetByID(f.id)
		result, err := f.manager.ForceRefreshObservedAuth(nil, observed, func() (func(), bool) { return func() {}, true })
		if err != nil || result == nil {
			t.Fatal("nil context refresh failed")
		}
		ram, cold := f.tokens(t)
		if ram != "refreshed-token" || cold != ram {
			t.Error("nil context not persisted")
		}
	})
	t.Run("ordinary_io_keeps_rotated_ram", func(t *testing.T) {
		f := newNamedRefreshFixture(t)
		f.store.fail.Store(true)
		w := f.request(t, true, "json", true)
		ram, cold := f.tokens(t)
		if w.Code != 500 || ram != "refreshed-token" || cold != "old-private-fixture" {
			t.Errorf("ordinary I/O HTTP=%d new-RAM=%t old-cold=%t", w.Code, ram == "refreshed-token", cold == "old-private-fixture")
		}
	})
}

func TestNamedRefreshNoLockCycle(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "named-admin-fixture")
	f := newNamedRefreshFixture(t)
	second, err := f.manager.Register(context.Background(), &coreauth.Auth{ID: "ordinary-second.json", FileName: "ordinary-second.json", Provider: "antigravity", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "antigravity", "access_token": "second-old-fixture", "refresh_token": "second-refresh-fixture"}})
	if err != nil {
		t.Fatal("second registration")
	}
	other := coreauth.NewManager(f.store, nil, nil)
	if err := other.Load(context.Background()); err != nil {
		t.Fatal("other load")
	}
	aSaving, aRelease := make(chan struct{}), make(chan struct{})
	f.store.onSave = func(a *coreauth.Auth) {
		if a.ID == f.id {
			close(aSaving)
			<-aRelease
		}
	}
	aDone := make(chan int, 1)
	go func() { aDone <- f.request(t, true, "json", true).Code }()
	select {
	case <-aSaving:
	case <-time.After(2 * time.Second):
		close(aRelease)
		t.Fatal("A save not reached")
	}
	cRemote, cRelease, cBegin := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.exec.before = func(a *coreauth.Auth) {
		if a.ID == second.ID {
			close(cRemote)
			<-cRelease
		}
	}
	generation := f.handler.authManagerGeneration
	begin := func() (func(), bool) {
		close(cBegin)
		f.handler.authUploadScopeMu.RLock()
		if f.handler.authManager != f.manager || f.handler.authManagerGeneration != generation {
			f.handler.authUploadScopeMu.RUnlock()
			return nil, false
		}
		return f.handler.authUploadScopeMu.RUnlock, true
	}
	publication := &coreauth.AuthPublicationFence{BeginPublication: func() (func(), bool) {
		if !f.handler.authUploadScopeMu.TryRLock() {
			return nil, false
		}
		if f.handler.authManager != f.manager || f.handler.authManagerGeneration != generation {
			f.handler.authUploadScopeMu.RUnlock()
			return nil, false
		}
		return f.handler.authUploadScopeMu.RUnlock, true
	}}
	cDone := make(chan error, 1)
	go func() {
		_, e := f.manager.ForceRefreshObservedAuth(coreauth.WithAuthPublicationFence(context.Background(), publication), second, begin)
		cDone <- e
	}()
	select {
	case <-cRemote:
	case <-time.After(2 * time.Second):
		close(cRelease)
		close(aRelease)
		t.Fatal("second provider not reached")
	}
	reloadDone := make(chan struct{})
	go func() { f.handler.SetAuthManager(other); close(reloadDone) }()
	deadline := time.Now().Add(time.Second)
	writerWaiting := false
	for time.Now().Before(deadline) {
		if !f.handler.mu.TryLock() {
			writerWaiting = true
			break
		}
		f.handler.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if !writerWaiting {
		close(cRelease)
		close(aRelease)
		t.Fatal("reload did not hold handler mutex")
	}
	close(cRelease)
	select {
	case <-cBegin:
	case <-time.After(2 * time.Second):
		close(aRelease)
		t.Fatal("second commit boundary not reached")
	}
	close(aRelease)
	select {
	case <-reloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reload deadlocked")
	}
	select {
	case e := <-cDone:
		if !errors.Is(e, coreauth.ErrSelectedRefreshChanged) {
			t.Error("second refresh not rejected after reload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second refresh deadlocked")
	}
	select {
	case code := <-aDone:
		if code != 409 {
			t.Errorf("A after reload HTTP=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh deadlocked")
	}
	f.store.onSave = nil
	replacement, _ := other.GetByID(f.id)
	replacement.Metadata = map[string]any{"type": "antigravity", "access_token": "replacement-private-fixture", "refresh_token": "replacement-refresh-fixture"}
	if _, err := other.Update(context.Background(), replacement); err != nil {
		t.Fatal("replacement save")
	}
	cold, err := f.base.List(context.Background())
	if err != nil {
		t.Fatal("cold read")
	}
	if namedRefreshToken(t, cold, f.id) != "replacement-private-fixture" || namedRefreshToken(t, cold, second.ID) != "second-old-fixture" {
		t.Error("late refresh changed replacement or second credential")
	}
}

type refreshScopeChangingBody struct {
	io.Reader
	beforeRead func()
}

func (b *refreshScopeChangingBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		b.beforeRead()
		b.beforeRead = nil
	}
	return b.Reader.Read(p)
}

func TestBulkRefreshDoesNotDispatchAnObsoleteScope(t *testing.T) {
	f := newNamedRefreshFixture(t)
	other := coreauth.NewManager(nil, nil, nil)
	body := &refreshScopeChangingBody{
		Reader:     strings.NewReader(`{"all":true}`),
		beforeRead: func() { f.handler.SetAuthManager(other) },
	}
	router := gin.New()
	router.POST("/refresh", f.handler.RefreshAuthFiles)
	req := httptest.NewRequest(http.MethodPost, "/refresh", body)
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != http.StatusConflict || f.exec.refreshCnt.Load() != 0 {
		t.Fatalf("superseded bulk refresh: status=%d calls=%d", out.Code, f.exec.refreshCnt.Load())
	}
}
