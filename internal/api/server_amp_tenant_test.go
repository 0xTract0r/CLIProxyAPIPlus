package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

type ampTenantTransport func(*http.Request) (*http.Response, error)

func (f ampTenantTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type ampFabricatedPrincipal struct{ key string }

func (p ampFabricatedPrincipal) Identifier() string { return sdkaccess.DefaultAccessProviderName }

func (p ampFabricatedPrincipal) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: p.key}, nil
}

func TestOrdinaryAmpFallbackUsesOnlyCallerBinding(t *testing.T) {
	const (
		a         = "synthetic-tenant-a-inbound"
		b         = "synthetic-tenant-b-inbound"
		unbound   = "synthetic-unbound-inbound"
		upA       = "synthetic-tenant-a-secret"
		upB       = "synthetic-tenant-b-secret"
		newA      = "synthetic-tenant-new-a-secret"
		shared    = "synthetic-legacy-shared-key"
		target    = "https://amp-tenant.example.invalid/fixed"
		postPath  = "/api/provider/openai/v1/chat/completions"
		modelList = "/api/provider/openai/v1/models"
		control   = "/api/user/profile"
	)
	keys := []string{a, b, unbound, upA, upB, newA, shared}
	bind := func(caller, key string) config.AmpUpstreamAPIKeyEntry {
		return config.AmpUpstreamAPIKeyEntry{APIKeys: []string{caller}, UpstreamAPIKey: key}
	}
	noKey := func(value string) {
		t.Helper()
		for _, secret := range keys {
			if strings.Contains(value, secret) {
				t.Fatal("credential leaked through an ordinary response or request log")
			}
		}
	}
	logs := t.TempDir()
	cfg := &config.Config{AmpCode: config.AmpCode{UpstreamURL: target, UpstreamAPIKey: shared,
		UpstreamAPIKeys: []config.AmpUpstreamAPIKeyEntry{bind(a, upA), bind(b, upB)}}}
	cfg.APIKeys = []string{a, b, unbound}
	cfg.RequestLog = true
	s := newTestServerWithConfig(t, cfg, WithIsolatedUsage(), WithRequestLoggerFactory(func(*config.Config, string) logging.RequestLogger {
		return logging.NewFileRequestLogger(true, logs, "", 20)
	}))
	wantKey := ""
	expectedPath := ""
	calls := 0
	transport := ampTenantTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		outbound := req.Header.Get("X-Api-Key")
		if wantKey == "" || outbound != wantKey || req.Header.Get("Authorization") != "Bearer "+wantKey ||
			req.Header.Get("X-Goog-Api-Key") != "" || req.Header.Get("Cookie") != "" ||
			req.URL.Host != "amp-tenant.example.invalid" || req.URL.Path != "/fixed"+expectedPath ||
			req.URL.RawQuery != "keep=1" {
			t.Fatal("unexpected key, route, or caller metadata in actual proxy transport")
		}
		status := http.StatusOK
		h := http.Header{"X-Amp-Reflected": []string{outbound}, "Set-Cookie": []string{outbound},
			"X-Api-Key": []string{outbound}, "Content-Type": []string{"application/json"}}
		if outbound == upB {
			status = http.StatusTooManyRequests
			h.Set("Retry-After", "17")
		}
		mid := len(outbound) / 2
		echo := io.MultiReader(strings.NewReader(`{"answer":"ordinary","echo":"`+outbound[:mid]), strings.NewReader(outbound[mid:]+`"}`))
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(echo), ContentLength: -1}, nil
	})
	attach := func() { s.ampForward.Load().proxy.Transport = transport }
	attach()

	call := func(method, path, client, expectedUpstream string, status int) *httptest.ResponseRecorder {
		t.Helper()
		before := calls
		wantKey, expectedPath = expectedUpstream, path
		req := ampFixtureRequest(method, path+"?keep=1&key="+client, `{"model":"synthetic-missing-model","messages":[]}`, "127.0.0.1:5300", client)
		req.AddCookie(&http.Cookie{Name: "browser-fixture", Value: "ignored"})
		w := httptest.NewRecorder()
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		s.engine.ServeHTTP(w, req.WithContext(ctx))
		if w.Code != status {
			t.Fatalf("unexpected status for %s: got HTTP %d, want %d", path, w.Code, status)
		}
		if delta := calls - before; (expectedUpstream == "" && delta != 0) || (expectedUpstream != "" && delta != 1) {
			t.Fatalf("unexpected dispatch count: %d", delta)
		}
		noKey(w.Body.String())
		for name, values := range w.Result().Header {
			noKey(name + strings.Join(values, " "))
		}
		return w
	}

	first := call(http.MethodPost, postPath, a, upA, http.StatusOK)
	if !strings.Contains(first.Body.String(), `"answer":"ordinary"`) || first.Header().Get("X-Api-Key") != "" {
		t.Fatal("authorized normal data or header protection is missing")
	}
	refused := call(http.MethodPost, postPath, b, upB, http.StatusTooManyRequests)
	if refused.Header().Get("Retry-After") != "17" {
		t.Fatal("upstream wait condition was lost")
	}
	call(http.MethodPost, postPath, unbound, "", http.StatusForbidden)
	call(http.MethodPost, postPath, "", "", http.StatusUnauthorized)
	call(http.MethodGet, control, a, upA, http.StatusOK)
	call(http.MethodGet, control, unbound, "", http.StatusForbidden)
	call(http.MethodGet, modelList, a, "", http.StatusOK)
	call(http.MethodGet, modelList, b, "", http.StatusOK)

	reload := func(url string, entries []config.AmpUpstreamAPIKeyEntry, callers ...string) {
		t.Helper()
		next := s.getConfig().CloneForRuntime()
		next.AmpCode.UpstreamURL = url
		next.AmpCode.UpstreamAPIKeys = entries
		next.APIKeys = callers
		if !s.UpdateClientsContext(context.Background(), next) {
			t.Fatal("test configuration reload failed")
		}
		attach()
	}
	reload(target, []config.AmpUpstreamAPIKeyEntry{bind(a, newA), bind(b, upB)}, a, b, unbound)
	call(http.MethodPost, postPath, a, newA, http.StatusOK)

	providers := s.accessManager.Providers()
	s.accessManager.SetProviders([]sdkaccess.Provider{ampFabricatedPrincipal{key: a}})
	call(http.MethodPost, postPath, unbound, "", http.StatusForbidden)
	s.accessManager.SetProviders(providers)

	// An invalid new destination keeps the old destination. It grants no new
	// key at that destination and takes the removed caller out immediately.
	reload("ftp://amp-tenant.example.invalid", []config.AmpUpstreamAPIKeyEntry{bind(a, upA), bind(unbound, shared)}, a, b, unbound)
	call(http.MethodPost, postPath, a, "", http.StatusForbidden)
	call(http.MethodPost, postPath, b, "", http.StatusForbidden)
	call(http.MethodPost, postPath, unbound, "", http.StatusForbidden)

	reload(target, []config.AmpUpstreamAPIKeyEntry{bind(a, newA), bind(a, upA), bind(b, upB)}, a, b, unbound)
	call(http.MethodPost, postPath, a, "", http.StatusForbidden)
	call(http.MethodPost, postPath, b, upB, http.StatusTooManyRequests)

	// Removing the inbound credential takes effect even with a stale mapping.
	reload(target, []config.AmpUpstreamAPIKeyEntry{bind(b, upB)}, a, unbound)
	call(http.MethodPost, postPath, b, "", http.StatusUnauthorized)
	reload(target, nil, a, unbound)
	call(http.MethodPost, postPath, a, "", http.StatusForbidden)
	call(http.MethodGet, control, a, "", http.StatusForbidden)

	evidence := 0
	if err := filepath.WalkDir(logs, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
			r, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return err
			}
			body, err = io.ReadAll(r)
			_ = r.Close()
			if err != nil {
				return err
			}
		}
		noKey(string(body))
		if strings.Contains(string(body), `"answer":"ordinary"`) {
			evidence++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if evidence == 0 {
		t.Fatal("test did not observe a persisted proxy response log")
	}
}

func TestOrdinaryAmpInformationalHeadersProtectBinding(t *testing.T) {
	const caller = "synthetic-interim-client"
	const key = "synthetic-interim-upstream"
	req := httptest.NewRequest(http.MethodPost, "https://amp-tenant.example.invalid", nil)
	proxyObserved := false
	proxyTrace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, headers textproto.MIMEHeader) error {
			proxyObserved = true
			if code != http.StatusEarlyHints || headers.Get("Authorization") != "" ||
				strings.Contains(headers.Get("X-Reflected"), key) || strings.Contains(headers.Get("X-Reflected"), caller) ||
				headers.Get("Link") != "</asset>; rel=preload" {
				t.Error("interim header filtering did not preserve safe data and remove secrets before forwarding")
			}
			return nil
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), proxyTrace))
	transport := ampProtectedTransport{
		caller: caller, upstreamKey: key,
		RoundTripper: ampTenantTransport(func(req *http.Request) (*http.Response, error) {
			err := httptrace.ContextClientTrace(req.Context()).Got1xxResponse(http.StatusEarlyHints, textproto.MIMEHeader{
				"X-Reflected": []string{caller + " " + key}, "Authorization": []string{"Bearer " + key},
				"Link": []string{"</asset>; rel=preload"},
			})
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, err
		}),
	}
	if _, err := transport.RoundTrip(req); err != nil || !proxyObserved {
		t.Fatal("the fake upstream's informational response did not reach the proxy callback")
	}
}

func TestOrdinaryAmpBindingRevocationSurvivesManagementSaveAndRestart(t *testing.T) {
	const (
		client       = "synthetic-cold-inbound"
		upstream     = "synthetic-cold-bound"
		legacy       = "synthetic-cold-shared"
		management   = "synthetic-cold-management"
		controlPath  = "/api/user/profile"
		fallbackPath = "/api/provider/openai/v1/chat/completions"
		deletePath   = "/v0/management/ampcode/upstream-api-keys"
	)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	hash, err := bcrypt.GenerateFromPassword([]byte(management), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`auth-dir: %q
api-keys:
  - %q
remote-management:
  secret-key: %q
ampcode:
  upstream-url: https://amp-cold-fixture.example.invalid
  upstream-api-key: %q
  upstream-api-keys:
    - upstream-api-key: %q
      api-keys:
        - %q
`, filepath.Join(dir, "auth"), client, string(hash), legacy, upstream, client)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	call := func(s *Server, method, path, key, body string, status int) {
		t.Helper()
		req := ampFixtureRequest(method, path, body, "127.0.0.1:5300", key)
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req.WithContext(ctx))
		if w.Code != status {
			t.Fatalf("unexpected status for %s %s: got %d, want %d", method, path, w.Code, status)
		}
	}
	liveConfig := load()
	if liveConfig.AmpCode.RequireUpstreamAPIKeyBinding || len(liveConfig.AmpCode.UpstreamAPIKeys) != 1 {
		t.Fatal("test requires a legacy file with one mapping and no persisted mode")
	}
	reloaded := make(chan bool, 1)
	var s *Server
	s = NewServer(liveConfig, coreauth.NewManager(nil, nil, nil), sdkaccess.NewManager(), configPath,
		WithIsolatedUsage(), WithConfigReloadHook(func(ctx context.Context, next *config.Config) {
			reloaded <- s.UpdateClientsContext(ctx, next)
		}))
	dispatched := 0
	s.ampForward.Load().proxy.Transport = ampTenantTransport(func(req *http.Request) (*http.Response, error) {
		dispatched++
		if req.Header.Get("X-Api-Key") != upstream || req.Header.Get("Authorization") != "Bearer "+upstream {
			t.Error("the existing tenant selected the wrong upstream credential")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})
	call(s, http.MethodGet, controlPath, client, "", http.StatusNoContent)
	call(s, http.MethodDelete, deletePath, management, `{"value":["synthetic-cold-bound"]}`, http.StatusOK)
	select {
	case ok := <-reloaded:
		if !ok {
			t.Fatal("management snapshot reload failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management save did not publish a runtime reload")
	}
	s.ampForward.Load().proxy.Transport = ampTenantTransport(func(*http.Request) (*http.Response, error) {
		t.Error("a revoked caller was forwarded before restart")
		return &http.Response{StatusCode: http.StatusTeapot, Header: make(http.Header), Body: http.NoBody}, nil
	})
	call(s, http.MethodGet, controlPath, client, "", http.StatusForbidden)
	if dispatched != 1 {
		t.Fatalf("unexpected original upstream dispatch count: %d", dispatched)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	coldConfig := load()
	cold := NewServer(coldConfig, coreauth.NewManager(nil, nil, nil), sdkaccess.NewManager(), configPath, WithIsolatedUsage())
	t.Cleanup(func() {
		if err := cold.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	cold.ampForward.Load().proxy.Transport = ampTenantTransport(func(*http.Request) (*http.Response, error) {
		t.Error("a revoked caller was forwarded after restart")
		return &http.Response{StatusCode: http.StatusTeapot, Header: make(http.Header), Body: http.NoBody}, nil
	})
	call(cold, http.MethodGet, controlPath, client, "", http.StatusForbidden)
	call(cold, http.MethodPost, fallbackPath, client, `{"model":"synthetic-unresolved"}`, http.StatusForbidden)
	if !coldConfig.AmpCode.RequireUpstreamAPIKeyBinding || len(coldConfig.AmpCode.UpstreamAPIKeys) != 0 || coldConfig.AmpCode.UpstreamAPIKey != legacy {
		t.Fatal("saved configuration did not retain tenant mode and the legacy setting while removing the binding")
	}
}
