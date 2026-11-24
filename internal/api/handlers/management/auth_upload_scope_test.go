package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type scopedUploadTestStore struct {
	memoryAuthStore
	attempts int
	data     []byte
	err      error
	onRemote func()
}

func (s *scopedUploadTestStore) PersistAuthUpload(_ context.Context, _ string, data []byte, install func(func() error) error) error {
	confirmed := false
	err := install(func() error {
		s.attempts++
		s.data = bytes.Clone(data)
		if s.err != nil {
			return coreauth.ErrAuthUploadRemoteUnconfirmed
		}
		confirmed = true
		if s.onRemote != nil {
			s.onRemote()
		}
		return nil
	})
	if err != nil && confirmed {
		return errors.Join(coreauth.ErrAuthUploadLocalPending, err)
	}
	return err
}

func TestAuthUploadsUseScopedAtomicCommit(t *testing.T) {
	for _, endpoint := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		t.Run(endpoint, func(t *testing.T) {
			dir := t.TempDir()
			alias := filepath.Join(t.TempDir(), "credentials")
			if err := os.Symlink(dir, alias); err != nil {
				t.Skipf("directory aliases unavailable: %v", err)
			}
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(alias)
			manager := coreauth.NewManager(store, nil, nil)
			cfg := &config.Config{AuthDir: alias}
			manager.SetConfig(cfg)
			h := NewHandlerWithoutConfigFilePath(cfg, manager)
			h.tokenStore = store
			hooks := 0
			h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { hooks++; return nil }
			router := newV8AndV0AuthRouter(h)
			payload := `{  "type":"qwen", "note":"example", "other":9007199254740993 }`
			post := func(name string, multipart bool) int {
				return performUploadRequest(t, router, endpoint, multipart, map[string]string{name: payload}, []string{name}).Code
			}
			// The link's dangling relative target is inside the temp root.
			// Rejection tests local validation, without any out-of-scope file.
			leaf := filepath.Join(dir, "leaf.json")
			if err := os.Symlink("absent.json", leaf); err != nil {
				t.Fatal(err)
			}
			if code := post("leaf.json", false); code != http.StatusBadRequest {
				t.Fatalf("leaf symlink validation status = %d", code)
			}
			if len(manager.List()) != 0 || hooks != 0 {
				t.Fatal("invalid destination reached registration or hook")
			}
			name := "ordinary.json"
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(`{"type":"qwen"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			old, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if code := post(name, true); code != http.StatusOK {
				t.Fatalf("normal upload status = %d", code)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != payload {
				t.Fatalf("uploaded bytes changed: %v", err)
			}
			now, err := os.Stat(path)
			if err != nil || os.SameFile(old, now) || now.Mode().Perm()&0o077 != 0 {
				t.Fatalf("atomic private replacement failed: %v", err)
			}
			if len(manager.List()) != 1 || hooks != 1 {
				t.Fatal("successful upload did not register and call hook once")
			}
		})
	}
}

func TestAuthUploadBackingFailureKeepsSelectedFileAndCallback(t *testing.T) {
	f := setupSIWCFlow(t, false)
	f.router.POST("/credentials", f.h.UploadAuthFileV8)
	f.router.POST("/legacy", f.h.UploadAuthFile)
	state, flow := f.seedPendingRecord(t)
	before := f.readFixtureBytes(t)
	original, _ := f.manager.GetByID("codex.json")
	backend := &scopedUploadTestStore{err: errors.New("fixture refused persistence")}
	f.h.tokenStore = backend
	hooks := 0
	f.h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { hooks++; return nil }

	for _, path := range []string{"/credentials", "/legacy"} {
		w := siwcRequest(t, f.router, http.MethodPost, path+"?name=codex.json", map[string]any{
			"type": "codex", "note": "replace",
		})
		if w.Code != http.StatusBadGateway {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		current, ok := f.manager.GetByID("codex.json")
		if !ok || current.CredentialVersion != original.CredentialVersion ||
			!bytes.Equal(f.readFixtureBytes(t), before) || hooks != 0 ||
			flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
			t.Fatalf("%s failure changed file or callback", path)
		}
	}
	if backend.attempts != 2 {
		t.Fatalf("backing store attempts = %d", backend.attempts)
	}

	// A path-only backend has no safe contract. It must be refused before
	// either version creates a file or invalidates the callback.
	f.h.tokenStore = struct{ coreauth.Store }{&backend.memoryAuthStore}
	for _, path := range []string{"/credentials", "/legacy"} {
		w := siwcRequest(t, f.router, http.MethodPost, path+"?name=codex.json", map[string]any{"type": "codex"})
		if w.Code != http.StatusNotImplemented || !bytes.Equal(f.readFixtureBytes(t), before) || hooks != 0 ||
			flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
			t.Fatalf("%s unsupported store changed state: status=%d", path, w.Code)
		}
	}

	backend.err = nil
	f.h.tokenStore = backend
	w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", map[string]any{
		"type": "codex", "note": "retry",
	})
	if w.Code != http.StatusOK || hooks != 1 || flow.ctx.Err() == nil || backend.attempts != 3 ||
		!bytes.Equal(f.readFixtureBytes(t), backend.data) {
		t.Fatalf("successful retry did not persist and publish together: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthUploadReloadAfterRemoteCommitDoesNotPublishOldScope(t *testing.T) {
	for _, path := range []string{"/credentials", "/legacy"} {
		t.Run(path, func(t *testing.T) {
			f := setupSIWCFlow(t, false)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			f.router.POST("/legacy", f.h.UploadAuthFile)
			state, flow := f.seedPendingRecord(t)
			before := f.readFixtureBytes(t)
			newDir := t.TempDir()
			called := make(chan struct{})
			proceed := make(chan struct{})
			backend := &scopedUploadTestStore{onRemote: func() { close(called); <-proceed }}
			f.h.tokenStore = backend
			hooks := 0
			f.h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { hooks++; return nil }
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, path+"?name=codex.json",
					bytes.NewBufferString(`{"type":"codex","note":"new"}`))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				f.router.ServeHTTP(w, req)
				result <- w
			}()
			select {
			case <-called:
			case <-time.After(3 * time.Second):
				close(proceed)
				t.Fatal("upload did not reach remote confirmation")
			}
			reloaded := make(chan struct{})
			go func() { f.h.SetConfig(&config.Config{AuthDir: newDir}); close(reloaded) }()
			select {
			case <-reloaded:
				close(proceed)
			case <-time.After(3 * time.Second):
				close(proceed)
				t.Fatal("config reload blocked on remote upload")
			}
			select {
			case w := <-result:
				if w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte("upload_local_pending")) {
					t.Fatalf("reload after remote commit status=%d body=%s", w.Code, w.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request did not finish after reload")
			}
			if !bytes.Equal(before, f.readFixtureBytes(t)) || hooks != 0 ||
				flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
				t.Fatal("old scope upload replaced local data or published after reload")
			}
			root, err := os.OpenRoot(newDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, err := root.Lstat("codex.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("request migrated to new scope: %v", err)
			}
		})
	}
}

func TestAuthUploadKeepsSelectionBeforeBodyRead(t *testing.T) {
	f := setupSIWCFlow(t, false)
	newDir := t.TempDir()
	router := newV8AndV0AuthRouter(f.h)
	body := &reloadBeforeUploadRead{Reader: bytes.NewBufferString(`{"type":"qwen"}`), reload: func() {
		f.h.SetConfig(&config.Config{AuthDir: newDir})
	}}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=new.json", body))
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("upload_scope_changed")) {
		t.Fatalf("scope changed before write: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthMultipartUploadScopeFailureRetainsPerFileStatus(t *testing.T) {
	f := setupSIWCFlow(t, false)
	dir := t.TempDir()
	router := newV8AndV0AuthRouter(f.h)
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	for _, name := range []string{"first.json", "second.json"} {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, `{"type":"qwen"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := &reloadBeforeUploadRead{Reader: &raw, reload: func() {
		f.h.SetConfig(&config.Config{AuthDir: dir})
	}}
	req := httptest.NewRequest(http.MethodPost, "/v8/management/credentials", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var result struct {
		Failed []struct {
			Name   string `json:"name"`
			Code   string `json:"code"`
			Status int    `json:"status"`
		} `json:"failed"`
	}
	if w.Code != http.StatusMultiStatus || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Failed) != 2 {
		t.Fatalf("multipart status=%d body=%s", w.Code, w.Body.String())
	}
	for i, want := range []string{"first.json", "second.json"} {
		failure := result.Failed[i]
		if failure.Name != want || failure.Status != http.StatusConflict || failure.Code != "upload_scope_changed" {
			t.Fatalf("multipart failure entry = %+v", failure)
		}
	}
}

type reloadBeforeUploadRead struct {
	io.Reader
	reload func()
}

func (r *reloadBeforeUploadRead) Read(p []byte) (int, error) {
	if r.reload != nil {
		r.reload()
		r.reload = nil
	}
	return r.Reader.Read(p)
}
