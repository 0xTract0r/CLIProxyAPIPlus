package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type siwcExchangeInput struct {
	code, clientID, redirect, deviceID string
	pkce                               *codex.PKCECodes
}

type testSIWCOAuth struct {
	calls   atomic.Int32
	inputs  chan siwcExchangeInput
	release chan struct{}
	err     error
}

func (s *testSIWCOAuth) GenerateSIWCAuthURL(state, nonce, deviceID, redirect string, pkce *codex.PKCECodes) (string, error) {
	return codex.NewCodexAuth(nil).GenerateSIWCAuthURL(state, nonce, deviceID, redirect, pkce)
}

func (s *testSIWCOAuth) ExchangeSIWCCodeForTokens(ctx context.Context, code, clientID, redirect, deviceID string, pkce *codex.PKCECodes) (*codex.CodexSIWCTokenData, error) {
	s.calls.Add(1)
	s.inputs <- siwcExchangeInput{code, clientID, redirect, deviceID, pkce}
	if s.release != nil {
		// Deliberately ignore cancellation to prove the commit gate rejects a
		// successful but late upstream result after cancellation or expiration.
		<-s.release
	}
	if s.err != nil {
		return nil, s.err
	}
	return &codex.CodexSIWCTokenData{AccessToken: "new-siwc", RefreshToken: "new-siwc-refresh", ClientID: clientID, DeviceID: deviceID, Expire: "2035-01-01T00:00:00Z", ExpiresAt: "2035-01-01T00:00:00Z"}, nil
}

type siwcFixture struct {
	h       *Handler
	router  *gin.Engine
	manager *coreauth.Manager
	auth    *coreauth.Auth
	path    string
	service *testSIWCOAuth
	store   *oauthSessionStore
	unblock func()
}

func setupSIWCFlow(t *testing.T, blocked bool) *siwcFixture {
	t.Helper()
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	metadata := map[string]any{"type": "codex", "access_token": "outer", "refresh_token": "outer-refresh", "account_id": "account", "note": "keep", "siwc": map[string]any{"device_id": "e61bbe28-07ef-466d-8e5d-a344f94ab305"}}
	if errWrite := codex.WriteJSONFileAtomic(path, metadata); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{AuthDir: dir}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "codex.json", FileName: "codex.json", Provider: "codex", Metadata: metadata, Attributes: map[string]string{"path": path}, Status: coreauth.StatusActive})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	service := &testSIWCOAuth{inputs: make(chan siwcExchangeInput, 4)}
	if blocked {
		service.release = make(chan struct{})
	}
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			if service.release != nil {
				close(service.release)
			}
		})
	}
	h := &Handler{cfg: cfg, authManager: manager, siwcOAuthFactory: func(*config.Config, string) siwcOAuthService { return service }}
	router := gin.New()
	router.POST("/bind", h.StartSIWCBind)
	router.GET("/siwc-status", h.GetSIWCStatus)
	router.POST("/callback", h.PostOAuthCallbackV8)
	router.GET("/callback", h.GetOAuthCallbackV8)
	router.POST("/legacy-callback", h.PostOAuthCallback)
	router.GET("/status", h.GetAuthStatus)
	router.DELETE("/session", h.CancelAuthSession)
	t.Cleanup(func() {
		unblock()
		store.mu.Lock()
		var flows []*siwcBindSession
		for _, session := range store.sessions {
			if session.SIWC != nil {
				flows = append(flows, session.SIWC)
			}
		}
		store.mu.Unlock()
		for _, flow := range flows {
			flow.cancel()
			flow.mu.Lock()
			done := flow.exchangeDone
			flow.mu.Unlock()
			if done != nil {
				<-done
			}
		}
	})
	return &siwcFixture{h, router, manager, auth, path, service, store, unblock}
}

func siwcRequest(t *testing.T, router http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, errJSON := json.Marshal(body)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// These rejection tests need a pending record, not an authorization URL or token exchange.
func (f *siwcFixture) seedPendingRecord(t *testing.T) (string, *siwcBindSession) {
	t.Helper()
	const state = "fixture-pending"
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	flow := &siwcBindSession{
		owner: f.h, manager: f.manager, service: f.service, targetID: f.auth.ID,
		path: f.path, authDir: filepath.Dir(f.path), redirect: codex.SIWCRedirectURI,
		ctx: ctx, cancel: cancel,
	}
	if !f.store.registerSIWC(state, flow) {
		t.Fatal("cannot seed pending test record")
	}
	return state, flow
}

func (f *siwcFixture) readFixtureBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func siwcCallbackURL(state string) string {
	return codex.SIWCRedirectURI + "?" + url.Values{"state": {state}, "code": {"issued-code"}, "client_id": {"oaiapp_issued"}}.Encode()
}

func waitSIWCExchange(t *testing.T, flow *siwcBindSession) {
	t.Helper()
	flow.mu.Lock()
	done := flow.exchangeDone
	flow.mu.Unlock()
	if done == nil {
		t.Fatal("exchange did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not finish")
	}
}

func TestSIWCBindRejectsInvalidCallbacksWithoutExchange(t *testing.T) {
	f := setupSIWCFlow(t, false)
	state, _ := f.seedPendingRecord(t)
	before := f.readFixtureBytes(t)
	cases := []map[string]any{
		{"state": state, "code": "code"},
		{"state": state, "client_id": "client"},
		{"state": state, "code": "code", "client_id": "client", "provider": "claude"},
		{"state": state, "redirect_url": siwcCallbackURL("other-state")},
		{"state": state, "redirect_url": strings.Replace(siwcCallbackURL(state), "127.0.0.1", "evil.example", 1)},
		{"redirect_url": siwcCallbackURL(state) + "&client_id=other"},
		{"state": state, "client_id": "other", "redirect_url": siwcCallbackURL(state)},
	}
	for _, body := range cases {
		w := siwcRequest(t, f.router, http.MethodPost, "/callback", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid callback: %d %s", w.Code, w.Body.String())
		}
	}
	w := siwcRequest(t, f.router, http.MethodGet, "/callback?state="+state+"&code=code&client_id=first&client_id=second", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatal("duplicate GET callback parameters accepted")
	}
	if f.service.calls.Load() != 0 || !f.store.IsPending(state, "codex") {
		t.Fatal("invalid callback consumed pending session")
	}
	if string(f.readFixtureBytes(t)) != string(before) {
		t.Fatal("rejected callback rewrote credential")
	}
}

func TestSIWCBindOAuthErrorAndEligibility(t *testing.T) {
	f := setupSIWCFlow(t, false)
	for _, kind := range []string{"config", "runtime", "api-key", "missing-outer", "other-provider"} {
		candidate := f.auth.Clone()
		switch kind {
		case "config":
			candidate.Attributes["source_backend"] = "config"
		case "runtime":
			candidate.Attributes["runtime_only"] = "true"
		case "api-key":
			candidate.Attributes["api_key"] = "key"
		case "missing-outer":
			delete(candidate.Metadata, "refresh_token")
		case "other-provider":
			candidate.Provider = "claude"
		}
		if siwcBindEligible(candidate) {
			t.Fatalf("%s unexpectedly eligible", kind)
		}
	}
	state, _ := f.seedPendingRecord(t)
	before := f.readFixtureBytes(t)
	w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"state": state, "error": "access_denied: secret-data"})
	if w.Code != http.StatusOK || f.service.calls.Load() != 0 {
		t.Fatalf("error callback triggered exchange: %d", w.Code)
	}
	if status := performOAuthStatusRequest(t, f.router, state); status.Status != "error" || strings.Contains(status.Error, "secret-data") {
		t.Fatalf("unsafe OAuth error: %#v", status)
	}
	if string(f.readFixtureBytes(t)) != string(before) {
		t.Fatal("rejected callback rewrote credential")
	}
}

func TestSIWCBindRejectsCredentialSymlinkAtStart(t *testing.T) {
	f := setupSIWCFlow(t, false)
	target := filepath.Join(filepath.Dir(f.path), "target.json")
	if err := os.Rename(f.path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	w := siwcRequest(t, f.router, http.MethodPost, "/bind", map[string]any{"name": f.auth.FileName})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("symlink accepted: %d %s", w.Code, w.Body.String())
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if len(f.store.sessions) != 0 || f.service.calls.Load() != 0 {
		t.Fatal("symlink rejection created an OAuth session or exchanged tokens")
	}
}

func TestSIWCBindTargetValidationAndLegacyCallback(t *testing.T) {
	f := setupSIWCFlow(t, false)
	for _, name := range []string{"", "../codex.json", "missing.json", f.path} {
		w := siwcRequest(t, f.router, http.MethodPost, "/bind", map[string]any{"name": name})
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Fatalf("invalid name accepted: %q %d", name, w.Code)
		}
	}
	if err := f.h.RegisterFileOAuthSession("ordinary", "codex", filepath.Dir(f.path)); err != nil {
		t.Fatal(err)
	}
	w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"state": "ordinary", "code": "ordinary-code"})
	if w.Code != http.StatusOK {
		t.Fatalf("ordinary v8 callback regressed: %s", w.Body.String())
	}
	data, errRead := os.ReadFile(filepath.Join(filepath.Dir(f.path), ".oauth-codex-ordinary.oauth"))
	if errRead != nil || !strings.Contains(string(data), "ordinary-code") {
		t.Fatal("ordinary callback file missing")
	}
}

type siwcUploadProgressContext struct {
	context.Context
	valueOnce sync.Once
	valueCh   chan struct{}
	doneOnce  sync.Once
	doneCh    chan struct{}
}

func (c *siwcUploadProgressContext) Value(key any) any {
	c.valueOnce.Do(func() {
		close(c.valueCh)
	})
	return c.Context.Value(key)
}

func (c *siwcUploadProgressContext) Done() <-chan struct{} {
	c.doneOnce.Do(func() {
		close(c.doneCh)
	})
	return c.Context.Done()
}

func TestWriteAuthFileWaitsForCodexFileLock(t *testing.T) {
	for _, tc := range []struct {
		name         string
		useSymlink   bool
		uploadedBody map[string]any
	}{
		{
			name:       "codex_upload",
			useSymlink: false,
			uploadedBody: map[string]any{
				"type":          "codex",
				"access_token":  "uploaded-outer",
				"refresh_token": "uploaded-refresh",
				"account_id":    "account",
				"note":          "after-lock",
			},
		},
		{
			name:       "non_codex_replacing_existing_codex",
			useSymlink: false,
			uploadedBody: map[string]any{
				"type":          "claude",
				"access_token":  "claude-access",
				"refresh_token": "claude-refresh",
				"note":          "replaced-provider",
			},
		},
		{
			name:       "symlinked_auth_dir_alias",
			useSymlink: true,
			uploadedBody: map[string]any{
				"type":          "codex",
				"access_token":  "uploaded-outer",
				"refresh_token": "uploaded-refresh",
				"account_id":    "account",
				"note":          "via-symlink-dir",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "codex.json")
			if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"original","note":"untouched"}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AuthDir: dir}
			h := &Handler{cfg: cfg, authManager: coreauth.NewManager(nil, nil, nil)}
			router := gin.New()
			router.POST("/legacy", h.UploadAuthFile)
			f := struct {
				path   string
				h      *Handler
				router *gin.Engine
			}{path, h, router}

			lockPath := f.path
			if tc.useSymlink {
				aliasDir := filepath.Join(t.TempDir(), "auths-link")
				if errSymlink := os.Symlink(filepath.Dir(f.path), aliasDir); errSymlink != nil {
					t.Skipf("symlinks unavailable: %v", errSymlink)
				}
				f.h.cfg.AuthDir = aliasDir
				if realPath, errEval := filepath.EvalSymlinks(f.path); errEval == nil {
					lockPath = realPath
				}
			}

			rawBody, errMarshal := json.Marshal(tc.uploadedBody)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}

			locked := make(chan struct{})
			releaseLock := make(chan struct{})
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					close(releaseLock)
				})
			}
			t.Cleanup(release)
			lockDone := make(chan error, 1)
			go func() {
				lockDone <- codex.WithFileLock(lockPath, func() error {
					close(locked)
					<-releaseLock
					return nil
				})
			}()
			<-locked

			reachedMutation := make(chan struct{})
			reachedUpsert := make(chan struct{})
			uploadDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/legacy?name=codex.json", strings.NewReader(string(rawBody)))
				req.Header.Set("Content-Type", "application/json")
				req = req.WithContext(&siwcUploadProgressContext{
					Context: req.Context(),
					valueCh: reachedMutation,
					doneCh:  reachedUpsert,
				})
				w := httptest.NewRecorder()
				f.router.ServeHTTP(w, req)
				uploadDone <- w
			}()
			<-reachedMutation
			for range 200 {
				runtime.Gosched()
			}
			select {
			case <-reachedUpsert:
				t.Fatal("writeAuthFile bypassed codex.WithFileLock and reached upsertAuthRecord while lock was held")
			default:
			}
			diskWhileLocked, errRead := os.ReadFile(f.path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if strings.Contains(string(diskWhileLocked), tc.uploadedBody["note"].(string)) {
				t.Fatalf("writeAuthFile modified disk while codex.WithFileLock was held: %s", string(diskWhileLocked))
			}

			release()
			select {
			case errLock := <-lockDone:
				if errLock != nil {
					t.Fatal(errLock)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("file lock holder did not finish")
			}
			var wUpload *httptest.ResponseRecorder
			select {
			case wUpload = <-uploadDone:
			case <-time.After(5 * time.Second):
				t.Fatal("legacy upload did not finish")
			}
			if wUpload.Code != http.StatusOK {
				t.Fatalf("legacy upload status = %d, body = %s", wUpload.Code, wUpload.Body.String())
			}
			diskAfter, errReadAfter := os.ReadFile(f.path)
			if errReadAfter != nil {
				t.Fatal(errReadAfter)
			}
			if !strings.Contains(string(diskAfter), tc.uploadedBody["note"].(string)) {
				t.Fatalf("expected uploaded content after lock release, got: %s", string(diskAfter))
			}
		})
	}
}

func TestOrdinaryOAuthCallbackLeavesLegacyCredentialUntouched(t *testing.T) {
	dir := t.TempDir()
	current := []byte(`{"type":"codex","access_token":"official-fixture","legacy":{"marker":"keep"}}`)
	path := filepath.Join(dir, "codex.json")
	if err := os.WriteFile(path, current, 0600); err != nil {
		t.Fatal(err)
	}

	replaceOAuthSessionStoreForTest(t, newOAuthSessionStore(time.Minute))
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	if err := h.RegisterFileOAuthSession("ordinary-callback-isolation", "codex", dir); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/callback", h.PostOAuthCallbackV8)
	w := siwcRequest(t, router, http.MethodPost, "/callback", map[string]any{"state": "ordinary-callback-isolation", "code": "ordinary-code"})
	if w.Code != http.StatusOK {
		t.Fatalf("ordinary callback status=%d: %s", w.Code, w.Body.String())
	}
	payload, err := os.ReadFile(filepath.Join(dir, ".oauth-codex-ordinary-callback-isolation.oauth"))
	if err != nil || !strings.Contains(string(payload), "ordinary-code") {
		t.Fatalf("ordinary callback file missing: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(current) {
		t.Fatalf("callback mutated the existing credential: %v", err)
	}
}
