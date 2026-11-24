package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type delayedRefreshTransport func(*http.Request) (*http.Response, error)

func (rt delayedRefreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt(req)
}

func TestOrdinaryCredentialMutationRejectsStaleOfficialRefresh(t *testing.T) {
	for _, operation := range []string{"replace", "delete", "delete-all", "delete-batch"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "codex.json")
			cfg := &config.Config{AuthDir: dir}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			h := &Handler{cfg: cfg, authManager: manager}
			router := gin.New()
			router.POST("/credentials", h.UploadAuthFileV8)
			router.DELETE("/credentials", h.DeleteAuthFileV8)
			original := []byte(`{"type":"codex","access_token":"first-access","refresh_token":"first-grant","account_id":"fixture","note":"original"}`)
			original = []byte(strings.Replace(string(original), "first-grant", "first-grant-"+operation, 1))
			replacement := []byte(`{"type":"codex","access_token":"second-access","refresh_token":"second-grant","account_id":"fixture","note":"replacement"}`)
			if err := h.writeAuthFile(t.Context(), "codex.json", original); err != nil {
				t.Fatal(err)
			}
			old, ok := manager.GetByID("codex.json")
			if !ok {
				t.Fatal("ordinary credential was not registered")
			}
			old = old.Clone()
			if old.Attributes[coreauth.AttributePath] == "" {
				t.Fatal("fixture credential was not bound to a file")
			}

			var response *httptest.ResponseRecorder
			switch operation {
			case "replace":
				response = siwcRequest(t, router, http.MethodPost, "/credentials?name=codex.json", json.RawMessage(replacement))
			case "delete":
				response = siwcRequest(t, router, http.MethodDelete, "/credentials?name=codex.json", nil)
			case "delete-all":
				if err := h.writeAuthFile(t.Context(), "other.json", original); err != nil {
					t.Fatal(err)
				}
				response = siwcRequest(t, router, http.MethodDelete, "/credentials?all=true", nil)
			case "delete-batch":
				if err := h.writeAuthFile(t.Context(), "other.json", original); err != nil {
					t.Fatal(err)
				}
				response = siwcRequest(t, router, http.MethodDelete, "/credentials", []string{"codex.json", "other.json"})
			}
			if response.Code != http.StatusOK {
				t.Fatalf("mutation failed: %d %s", response.Code, response.Body.String())
			}

			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", delayedRefreshTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network attempt")
			}))
			fresh, err := executor.NewCodexExecutor(cfg).Refresh(ctx, old)
			if err == nil || fresh != nil || calls != 0 {
				t.Fatalf("stale refresh: unexpectedly available=%t calls=%d error=%v", fresh != nil, calls, err)
			}

			after, readErr := os.ReadFile(path)
			if operation == "replace" {
				if readErr != nil || string(after) != string(replacement) {
					t.Fatalf("stale write changed replacement: %v", readErr)
				}
				selected, ok := manager.GetByID("codex.json")
				if !ok || selected.Metadata["refresh_token"] != "second-grant" {
					t.Fatal("selected credential registration changed")
				}
			} else {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("deleted credential reappeared: %v", readErr)
				}
				if _, ok := manager.GetByID("codex.json"); ok {
					t.Fatal("deleted credential registration reappeared")
				}
			}
		})
	}
}

func TestSIWCFileMutationScopeAndValidation(t *testing.T) {
	f := setupSIWCFlow(t, false)
	f.router.POST("/credentials", f.h.UploadAuthFileV8)
	f.router.POST("/legacy", f.h.UploadAuthFile)
	state, flow := f.seedPendingRecord(t)
	before := f.readFixtureBytes(t)
	for _, test := range []struct {
		path   string
		body   any
		status int
	}{
		{"/credentials?name=codex.json", map[string]any{"excluded_models": 123}, http.StatusBadRequest},
		{"/credentials?name=other.json", f.auth.Metadata, http.StatusOK},
		{"/legacy?name=codex.json", f.auth.Metadata, http.StatusOK},
	} {
		w := siwcRequest(t, f.router, http.MethodPost, test.path, test.body)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
		if flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
			t.Fatalf("%s invalidated unrelated or legacy binding", test.path)
		}
		if test.status == http.StatusBadRequest && string(f.readFixtureBytes(t)) != string(before) {
			t.Fatal("invalid upload rewrote the selected credential")
		}
	}
	f.h.postAuthPersistHook = func(_ context.Context, _ *coreauth.Auth) error {
		if f.h.siwcCommitMu.TryLock() {
			f.h.siwcCommitMu.Unlock()
			t.Error("commit gate released before publication hook")
		}
		return nil
	}
	w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", f.auth.Metadata)
	if w.Code != http.StatusOK || flow.ctx.Err() == nil {
		t.Fatalf("reupload failed to invalidate binding: %d", w.Code)
	}
	if f.service.calls.Load() != 0 {
		t.Fatal("file mutations initiated an exchange")
	}
}

func TestSIWCMutationPathAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !sameSIWCMutationPath(alias, path) || sameSIWCMutationPath(path, filepath.Join(dir, "other.json")) {
		t.Fatal("incorrect canonical path matching")
	}
}
