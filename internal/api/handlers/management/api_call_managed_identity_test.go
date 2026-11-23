package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Fork (fix-oauth-egress-anticorr, vuln 4): handler-level guards for the
// management api-call path that cpamp uses for quota lookups. A claude / codex
// api-call to the provider's protected upstream must present the account's
// serving-managed identity headers (not the Go default UA, not a UA hardcoded
// by the dashboard), and must refuse to dial at all when the account has no
// usable proxy_url.
//
// The capture proxy is a plain HTTP forward proxy: the api-call targets an
// http:// URL on the protected hostname, so the per-account transport forwards
// the absolute-form request to the proxy and the outbound headers can be
// asserted without a TLS MITM.

const apiCallHardcodedCodexUA = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

type apiCallCaptureServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	headers []http.Header
	urls    []string
}

func newAPICallCaptureServer(t *testing.T) *apiCallCaptureServer {
	t.Helper()
	s := &apiCallCaptureServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.urls = append(s.urls, r.URL.String())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *apiCallCaptureServer) URL() string { return s.srv.URL }

func (s *apiCallCaptureServer) requests() ([]http.Header, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...), append([]string(nil), s.urls...)
}

func newAPICallIdentityHandler(t *testing.T, cfg *config.Config) (*Handler, *coreauth.Manager) {
	t.Helper()
	if cfg.AuthDir == "" {
		cfg.AuthDir = filepath.Join(t.TempDir(), "auths")
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewClaudeExecutor(cfg))
	manager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.tokenStore = &memoryAuthStore{}
	return h, manager
}

func registerAPICallIdentityAuth(t *testing.T, manager *coreauth.Manager, provider, id, proxyURL string) (*coreauth.Auth, string) {
	t.Helper()
	metadata := map[string]any{
		"type":         provider,
		"email":        "apicall-" + provider + "@example.test",
		"access_token": "tok-" + provider,
	}
	if provider == "codex" {
		metadata["account_id"] = "acct-apicall"
	}
	if proxyURL != "" {
		metadata["proxy_url"] = proxyURL
	}
	auth := &coreauth.Auth{
		ID:       id,
		FileName: id,
		Provider: provider,
		ProxyURL: proxyURL,
		Status:   coreauth.StatusActive,
		Metadata: metadata,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register %s auth %s: %v", provider, id, err)
	}
	registered, ok := manager.GetByID(id)
	if !ok || registered == nil {
		t.Fatalf("registered auth %s not found", id)
	}
	return registered, registered.EnsureIndex()
}

func doAPICallRequest(t *testing.T, h *Handler, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.POST("/v0/management/api-call", h.APICall)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// expectedClaudeManagedUserAgent derives the account's serving-managed UA from
// the claude executor's own background-request header path (the one quota
// polling uses via exec.HttpRequest), applied to a fresh request.
func expectedClaudeManagedUserAgent(t *testing.T, cfg *config.Config, auth *coreauth.Auth, target string) string {
	t.Helper()
	probe, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build probe: %v", err)
	}
	if err := executor.NewClaudeExecutor(cfg).PrepareRequest(probe, auth); err != nil {
		t.Fatalf("prepare probe: %v", err)
	}
	return probe.Header.Get("User-Agent")
}

func TestAPICallClaudeUsesAccountManagedUserAgent(t *testing.T) {
	const target = "http://api.anthropic.com/api/oauth/usage"
	stabilize := true
	cases := []struct {
		name      string
		stabilize *bool
	}{
		{name: "device-profile stabilized", stabilize: &stabilize},
		{name: "legacy device headers", stabilize: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newAPICallCaptureServer(t)
			cfg := &config.Config{}
			cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile = tc.stabilize
			h, manager := newAPICallIdentityHandler(t, cfg)
			auth, index := registerAPICallIdentityAuth(t, manager, "claude", "claude-apicall-ua.json", proxy.URL())

			rec := doAPICallRequest(t, h, map[string]any{
				"auth_index": index,
				"method":     http.MethodGet,
				"url":        target,
				"header": map[string]string{
					"Authorization":  "Bearer $TOKEN$",
					"Content-Type":   "application/json",
					"anthropic-beta": "oauth-2025-04-20",
				},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("api-call status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			headers, urls := proxy.requests()
			if len(headers) != 1 {
				t.Fatalf("account proxy saw %d requests, want 1 (urls=%v)", len(headers), urls)
			}
			got := headers[0]
			ua := got.Get("User-Agent")
			if strings.HasPrefix(ua, "Go-http-client") || ua == "" {
				t.Fatalf("outbound User-Agent = %q: api-call leaked the Go default UA under the claude-cli TLS fingerprint", ua)
			}
			if !strings.HasPrefix(ua, "claude-cli/") {
				t.Fatalf("outbound User-Agent = %q, want the account's managed claude-cli UA", ua)
			}
			if tc.stabilize != nil {
				if want := expectedClaudeManagedUserAgent(t, cfg, auth, target); ua != want {
					t.Fatalf("outbound User-Agent = %q, want serving-managed %q", ua, want)
				}
			}
			if got.Get("X-App") != "cli" {
				t.Fatalf("outbound X-App = %q, want cli", got.Get("X-App"))
			}
			if got.Get("Authorization") != "Bearer tok-claude" {
				t.Fatalf("outbound Authorization = %q, want the dashboard-supplied bearer", got.Get("Authorization"))
			}
			if got.Get("Anthropic-Beta") != "oauth-2025-04-20" {
				t.Fatalf("outbound anthropic-beta = %q, want dashboard value kept", got.Get("Anthropic-Beta"))
			}
			if got.Get("Content-Type") != "application/json" {
				t.Fatalf("outbound Content-Type = %q, want dashboard value kept", got.Get("Content-Type"))
			}
		})
	}
}

func TestAPICallCodexOverridesHardcodedUserAgent(t *testing.T) {
	const target = "http://chatgpt.com/backend-api/wham/usage"
	proxy := newAPICallCaptureServer(t)
	cfg := &config.Config{}
	h, manager := newAPICallIdentityHandler(t, cfg)
	auth, index := registerAPICallIdentityAuth(t, manager, "codex", "codex-apicall-ua.json", proxy.URL())

	rec := doAPICallRequest(t, h, map[string]any{
		"auth_index": index,
		"method":     http.MethodGet,
		"url":        target,
		"header": map[string]string{
			"Authorization":      "Bearer $TOKEN$",
			"Content-Type":       "application/json",
			"User-Agent":         apiCallHardcodedCodexUA,
			"Chatgpt-Account-Id": "acct-apicall",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("api-call status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	headers, urls := proxy.requests()
	if len(headers) != 1 {
		t.Fatalf("account proxy saw %d requests, want 1 (urls=%v)", len(headers), urls)
	}
	got := headers[0]
	profile := helps.ResolveCodexClientProfile(auth, nil, cfg)
	want := strings.TrimSpace(profile.UserAgent)
	if want == "" {
		want = helps.DefaultCodexManagedUserAgent()
	}
	ua := got.Get("User-Agent")
	if ua == apiCallHardcodedCodexUA {
		t.Fatalf("outbound User-Agent = %q: the dashboard-hardcoded codex UA reached the upstream", ua)
	}
	if ua != want {
		t.Fatalf("outbound User-Agent = %q, want the account's managed codex UA %q", ua, want)
	}
	if strings.TrimSpace(got.Get("Originator")) == "" {
		t.Fatalf("outbound Originator missing, want the account's managed codex originator")
	}
	if got.Get("Authorization") != "Bearer tok-codex" {
		t.Fatalf("outbound Authorization = %q, want the dashboard-supplied bearer", got.Get("Authorization"))
	}
	if got.Get("Chatgpt-Account-Id") != "acct-apicall" {
		t.Fatalf("outbound Chatgpt-Account-Id = %q, want dashboard value kept", got.Get("Chatgpt-Account-Id"))
	}
}

func TestAPICallRejectsClaudeCodexAccountWithoutProxy(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, proxyCase := range []struct {
			name  string
			proxy string
		}{
			{name: "empty proxy", proxy: ""},
			{name: "invalid proxy", proxy: "ftp://bad-proxy.example:21"},
		} {
			t.Run(provider+"/"+proxyCase.name, func(t *testing.T) {
				upstream := newAPICallCaptureServer(t)
				globalSentinel := newAPICallCaptureServer(t)
				cfg := &config.Config{}
				cfg.SDKConfig.ProxyURL = globalSentinel.URL()
				h, manager := newAPICallIdentityHandler(t, cfg)
				_, index := registerAPICallIdentityAuth(t, manager, provider, provider+"-apicall-noproxy.json", proxyCase.proxy)

				rec := doAPICallRequest(t, h, map[string]any{
					"auth_index": index,
					"method":     http.MethodGet,
					"url":        upstream.URL() + "/api/oauth/usage",
					"header": map[string]string{
						"Authorization": "Bearer $TOKEN$",
					},
				})
				if rec.Code < 400 || rec.Code >= 500 {
					t.Fatalf("api-call status = %d, want 4xx fail-closed (body=%s)", rec.Code, rec.Body.String())
				}
				if hs, urls := upstream.requests(); len(hs) != 0 {
					t.Fatalf("upstream was dialed directly %d times (urls=%v); want zero egress", len(hs), urls)
				}
				if hs, urls := globalSentinel.requests(); len(hs) != 0 {
					t.Fatalf("global proxy was used %d times (urls=%v); want zero egress", len(hs), urls)
				}
			})
		}
	}
}

// TestAPICallManagedIdentityScopeUnchanged pins the boundaries: a claude
// account calling a non-protected host and a non-claude/codex account keep the
// dashboard-supplied headers and today's egress behaviour.
func TestAPICallManagedIdentityScopeUnchanged(t *testing.T) {
	t.Run("claude non-protected host keeps dashboard UA", func(t *testing.T) {
		proxy := newAPICallCaptureServer(t)
		h, manager := newAPICallIdentityHandler(t, &config.Config{})
		_, index := registerAPICallIdentityAuth(t, manager, "claude", "claude-apicall-other-host.json", proxy.URL())
		rec := doAPICallRequest(t, h, map[string]any{
			"auth_index": index,
			"method":     http.MethodGet,
			"url":        "http://other.example.test/probe",
			"header":     map[string]string{"User-Agent": "dashboard-probe/1"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("api-call status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		headers, _ := proxy.requests()
		if len(headers) != 1 || headers[0].Get("User-Agent") != "dashboard-probe/1" {
			t.Fatalf("non-protected host request headers = %v, want dashboard UA kept", headers)
		}
	})
	t.Run("gemini account without proxy keeps direct egress", func(t *testing.T) {
		upstream := newAPICallCaptureServer(t)
		h, manager := newAPICallIdentityHandler(t, &config.Config{})
		_, index := registerAPICallIdentityAuth(t, manager, "gemini", "gemini-apicall-noproxy.json", "")
		rec := doAPICallRequest(t, h, map[string]any{
			"auth_index": index,
			"method":     http.MethodGet,
			"url":        upstream.URL() + "/probe",
			"header":     map[string]string{"User-Agent": "dashboard-probe/1"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("api-call status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		headers, _ := upstream.requests()
		if len(headers) != 1 || headers[0].Get("User-Agent") != "dashboard-probe/1" {
			t.Fatalf("gemini request headers = %v, want dashboard UA kept", headers)
		}
	})
}
