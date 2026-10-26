package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type contractCountingAccess859 struct{ calls atomic.Int32 }

func (p *contractCountingAccess859) Identifier() string { return "fake-provider-859" }
func (p *contractCountingAccess859) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	p.calls.Add(1)
	if r.Header.Get("Authorization") == "Bearer fake-key-859" || r.Header.Get("X-Api-Key") == "fake-key-859" {
		return &sdkaccess.Result{Principal: "fake-issuer-859", Provider: p.Identifier()}, nil
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

type contractHTTP859 struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

func newContractHTTP859(t *testing.T, s *Server) *contractHTTP859 {
	t.Helper()
	hs := httptest.NewServer(s.engine)
	t.Cleanup(hs.Close)
	t.Cleanup(s.codexLiveHandler.Close)
	return &contractHTTP859{t, hs, &http.Client{Timeout: 5 * time.Second}}
}
func (h *contractHTTP859) request(method, path, body, bearer string, extra map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	req, e := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if e != nil {
		h.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, e := h.client.Do(req)
	if e != nil {
		h.t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		h.t.Fatal(e)
	}
	return resp.StatusCode, raw, resp.Header
}
func contractErrorCode859(t *testing.T, body []byte, want string) {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if e := json.Unmarshal(body, &payload); e != nil || payload.Error.Code != want {
		t.Fatalf("error code=%q want=%q body=%s parse=%v", payload.Error.Code, want, body, e)
	}
}

// These tests use production route registration. Authentication provider is test-only;
// no upstream Codex auth or realtime credentials exist in the manager.
func TestRealtimeRegisteredRouteAuthContract859(t *testing.T) {
	s := newTestServerWithConfig(t, &config.Config{})
	fake := &contractCountingAccess859{}
	s.accessManager.SetProviders([]sdkaccess.Provider{fake})
	h := newContractHTTP859(t, s)
	var secret string
	t.Run("standard_key_mints_local_secret_with_issuer", func(t *testing.T) {
		before := fake.calls.Load()
		status, body, headers := h.request("POST", "/v1/realtime/client_secrets", `{"session":{"type":"realtime","model":"gpt-realtime"}}`, "fake-key-859", nil)
		if status != 200 || fake.calls.Load()-before != 1 {
			t.Fatalf("status=%d standard calls=%d", status, fake.calls.Load()-before)
		}
		var result struct {
			Value     string `json:"value"`
			ExpiresAt int64  `json:"expires_at"`
		}
		if e := json.Unmarshal(body, &result); e != nil {
			t.Fatal(e)
		}
		secret = result.Value
		if !strings.HasPrefix(secret, "ek_") || result.ExpiresAt <= time.Now().Unix() || headers.Get("Cache-Control") != "no-store" {
			t.Fatal("local client secret response missing token/lifetime/no-store")
		}
		req := httptest.NewRequest("GET", "/v1/realtime", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		a, matched, e := s.codexLiveHandler.AuthenticateClientSecret(req)
		if e != nil || !matched || a.IssuerPrincipal != "fake-issuer-859" || a.IssuerProvider != fake.Identifier() || a.Principal == "" {
			t.Fatalf("issued authorization mismatch matched=%v err=%v issuer=%q/%q", matched, e, a.IssuerPrincipal, a.IssuerProvider)
		}
		t.Log("production client_secrets route minted a local secret; issuer preserved; standard provider called once")
	})
	if secret == "" {
		t.Fatal("mint precondition failed")
	}
	cases := []struct {
		name, method, path, body, key, code string
		extra                               map[string]string
		want                                int
		calls                               int32
	}{
		{name: "missing_key_standard_endpoint", method: "POST", path: "/v1/realtime/client_secrets", body: `{}`, code: "invalid_api_key", want: 401, calls: 1},
		{name: "invalid_secret_never_falls_back_even_with_good_alternate_key", method: "POST", path: "/v1/realtime/calls", body: `{}`, key: "ek_fake_invalid_859", code: "invalid_realtime_client_secret", extra: map[string]string{"X-Api-Key": "fake-key-859"}, want: 401, calls: 0},
		{name: "ordinary_invalid_key_uses_standard_fallback", method: "POST", path: "/v1/realtime/calls", body: `{}`, key: "fake-wrong-key-859", code: "invalid_api_key", want: 401, calls: 1},
		{name: "ordinary_key_reaches_real_call_body_validation", method: "POST", path: "/v1/realtime/calls", body: `not-json-or-sdp`, key: "fake-key-859", want: 400, calls: 1},
		{name: "valid_secret_reaches_real_call_body_validation", method: "POST", path: "/v1/realtime/calls", body: `not-json-or-sdp`, key: secret, want: 400, calls: 0},
		{name: "valid_secret_cannot_mint_another_secret", method: "POST", path: "/v1/realtime/client_secrets", body: `{}`, key: secret, code: "invalid_api_key", want: 401, calls: 1},
		{name: "invalid_secret_standard_only_endpoint_does_standard_auth", method: "POST", path: "/v1/realtime/client_secrets", body: `{}`, key: "ek_fake_invalid_859", code: "invalid_api_key", want: 401, calls: 1},
		{name: "new_sideband_valid_secret_reaches_upgrade_validation", method: "GET", path: "/v1/realtime/calls/fakecall859", key: secret, want: 426, calls: 0},
		{name: "old_sideband_secret_uses_standard_auth_and_is_rejected", method: "GET", path: "/v1/live/fakecall859", key: secret, want: 401, calls: 1},
		{name: "old_sideband_standard_key_reaches_upgrade_validation", method: "GET", path: "/v1/live/fakecall859", key: "fake-key-859", want: 426, calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fake.calls.Load()
			status, body, _ := h.request(tc.method, tc.path, tc.body, tc.key, tc.extra)
			delta := fake.calls.Load() - before
			if status != tc.want || delta != tc.calls {
				t.Fatalf("status=%d want=%d standard calls=%d want=%d body=%s", status, tc.want, delta, tc.calls, body)
			}
			if tc.code != "" {
				contractErrorCode859(t, body, tc.code)
			}
			t.Logf("production route status=%d standard-provider-calls=%d", status, delta)
		})
	}
}

func TestOAuthRegisteredCallbackStateContract859(t *testing.T) {
	hash, e := bcrypt.GenerateFromPassword([]byte("fake-management-859"), bcrypt.MinCost)
	if e != nil {
		t.Fatal(e)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	s := newTestServerWithConfig(t, cfg)
	s.mgmt.SetLocalPassword("fake-management-859")
	h := newContractHTTP859(t, s)
	for _, version := range []string{"v0", "v8"} {
		t.Run(version+"_ordinary_state_error_callback_uses_generic_sink", func(t *testing.T) {
			state := "fake-normal-859-" + version
			management.RegisterOAuthSession(state, "codex")
			t.Cleanup(func() { management.CancelOAuthSession(state) })
			path := "/v0/management/oauth-callback"
			if version == "v8" {
				path = "/v8/management/oauth/callback"
			}
			query := url.Values{"state": {state}, "error": {"fake_denied_859"}}
			status, body, _ := h.request("GET", path+"?"+query.Encode(), "", "", nil)
			if status != 200 {
				t.Fatalf("status=%d body=%s", status, body)
			}
			raw, e := os.ReadFile(filepath.Join(s.cfg.AuthDir, ".oauth-codex-"+state+".oauth"))
			if e != nil {
				t.Fatal(e)
			}
			var sink map[string]string
			if e = json.Unmarshal(raw, &sink); e != nil || sink["state"] != state || sink["error"] != "fake_denied_859" || sink["code"] != "" {
				t.Fatalf("callback sink mismatch: %s err=%v", raw, e)
			}
			t.Log("ordinary pending state accepted without management bearer; generic callback sink contains exact fake state/error")
		})
	}
	raw := []byte(`{"type":"codex","account_id":"11111111-1111-4111-8111-111111111111","email":"fake859@example.invalid","access_token":"fake-outer-access-859","refresh_token":"fake-outer-refresh-859"}`)
	authPath := filepath.Join(s.cfg.AuthDir, "fake-codex859.json")
	if e = os.WriteFile(authPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(s.cfg.AuthDir)
	manager := coreauth.NewManager(store, nil, nil)
	manager.SetConfig(s.cfg)
	if e = manager.Load(context.Background()); e != nil {
		t.Fatal(e)
	}
	s.mgmt.SetAuthManager(manager)
	status, body, _ := h.request("POST", "/v8/management/oauth/siwc/bind", `{"name":"fake-codex859.json"}`, "fake-management-859", nil)
	if status != 200 {
		t.Fatalf("bind status=%d body=%s", status, body)
	}
	var start struct {
		State string `json:"state"`
		Flow  string `json:"flow"`
	}
	if e = json.Unmarshal(body, &start); e != nil || start.State == "" || start.Flow != "siwc_bind" {
		t.Fatal("invalid bind state")
	}
	t.Cleanup(func() { management.CancelOAuthSession(start.State) })
	old, _ := manager.GetByID("fake-codex859.json")
	assertSourceUnchanged := func() {
		t.Helper()
		disk, e := os.ReadFile(authPath)
		if e != nil || !bytes.Equal(disk, raw) {
			t.Fatal("callback changed outer credential file")
		}
		current, ok := manager.GetByID(old.ID)
		if !ok || current.RegistrationEpoch != old.RegistrationEpoch || current.CredentialVersion != old.CredentialVersion || current.Metadata["access_token"] != old.Metadata["access_token"] || current.Metadata["refresh_token"] != old.Metadata["refresh_token"] {
			t.Fatal("callback changed tested outer credential fields")
		}
		matches, e := filepath.Glob(filepath.Join(s.cfg.AuthDir, ".oauth-*"+start.State+"*"))
		if e != nil || len(matches) != 0 {
			t.Fatal("SIWC state leaked to generic callback sink")
		}
	}
	badCases := []struct {
		name, path, body string
		want             int
	}{
		{"missing_client_id", "/v8/management/oauth/callback", fmt.Sprintf(`{"state":%q,"code":"fake-code-859"}`, start.State), 400},
		{"wrong_provider", "/v8/management/oauth/callback", fmt.Sprintf(`{"state":%q,"provider":"claude","error":"fake_denied_859"}`, start.State), 400},
		{"unexpected_redirect_host", "/v8/management/oauth/callback", fmt.Sprintf(`{"state":%q,"redirect_url":"http://example.invalid/callback?error=fake_denied_859"}`, start.State), 400},
		{"v0_sink_rejects_siwc", "/v0/management/oauth-callback", fmt.Sprintf(`{"state":%q,"error":"fake_denied_859"}`, start.State), 409},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := h.request("POST", tc.path, tc.body, "", nil)
			if status != tc.want {
				t.Fatalf("status=%d want=%d body=%s", status, tc.want, body)
			}
			if !management.IsOAuthSessionPending(start.State, "codex") {
				t.Fatal("rejected callback consumed pending state")
			}
			assertSourceUnchanged()
			t.Logf("status=%d, pending retained, no generic SIWC sink or outer credential mutation", status)
		})
	}
	t.Run("duplicate_query_rejected", func(t *testing.T) {
		path := "/v8/management/oauth/callback?state=" + url.QueryEscape(start.State) + "&state=" + url.QueryEscape(start.State) + "&error=fake_denied_859"
		status, body, _ := h.request("GET", path, "", "", nil)
		if status != 400 {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if !management.IsOAuthSessionPending(start.State, "codex") {
			t.Fatal("duplicate query consumed state")
		}
		assertSourceUnchanged()
	})
	t.Run("siwc_error_terminates_without_exchange_and_replay_conflicts", func(t *testing.T) {
		payload := fmt.Sprintf(`{"state":%q,"error":"fake_private_denial_859"}`, start.State)
		status, body, _ := h.request("POST", "/v8/management/oauth/callback", payload, "", nil)
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, body)
		}
		_, sessionStatus, ok := management.GetOAuthSession(start.State)
		if !ok || sessionStatus != "SIWC authorization was denied or failed" || management.IsOAuthSessionPending(start.State, "codex") {
			t.Fatalf("bad terminal state ok=%v status=%q", ok, sessionStatus)
		}
		assertSourceUnchanged()
		status, body, _ = h.request("POST", "/v8/management/oauth/callback", payload, "", nil)
		if status != 409 {
			t.Fatalf("replay status=%d body=%s", status, body)
		}
		if strings.Contains(string(body), "fake_private_denial_859") {
			t.Fatal("callback response leaked submitted denial detail")
		}
		t.Log("v8 SIWC denial=200 with sanitized terminal state, replay=409, no outer mutation or generic sink; no exchange path entered")
	})
}
