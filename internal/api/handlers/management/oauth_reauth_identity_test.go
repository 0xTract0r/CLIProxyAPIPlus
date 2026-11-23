package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Handler-level tests for what a card re-auth persists after the token
// exchange (fix-oauth-egress-anticorr review follow-up). The account-scoped
// exchange transport is replaced through oauthExchangeTransportForTest with a
// fake token endpoint, so the real RequestAnthropicToken / RequestCodexToken
// goroutines run end to end and write through a real file token store.

const (
	identityTargetDeviceID = "device-target-0123456789abcdef"
	identityTargetSeed     = "seed-target-0123456789abcdef"
	identityTargetCreated  = "2026-01-02T03:04:05Z"
)

type fakeTokenEndpoint struct {
	calls atomic.Int32
	body  func(req *http.Request) []byte
}

func (f *fakeTokenEndpoint) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(f.body(req))),
		Request:    req,
	}, nil
}

func useFakeTokenEndpoint(t *testing.T, body func(req *http.Request) []byte) *fakeTokenEndpoint {
	t.Helper()
	endpoint := &fakeTokenEndpoint{body: body}
	orig := oauthExchangeTransportForTest
	oauthExchangeTransportForTest = func(*coreauth.Auth) http.RoundTripper { return endpoint }
	t.Cleanup(func() { oauthExchangeTransportForTest = orig })
	return endpoint
}

func claudeTokenResponse(email string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"access_token":  "new-access-" + email,
		"refresh_token": "new-refresh-" + email,
		"token_type":    "bearer",
		"expires_in":    3600,
		"account":       map[string]any{"uuid": "uuid-" + email, "email_address": email},
	})
	return raw
}

func fakeCodexIDToken(email, accountID, plan string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	})
	return header + "." + enc.EncodeToString(claims) + ".sig"
}

func codexTokenResponse(email, accountID, plan string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"access_token":  "new-access-" + accountID,
		"refresh_token": "new-refresh-" + accountID,
		"id_token":      fakeCodexIDToken(email, accountID, plan),
		"token_type":    "bearer",
		"expires_in":    3600,
	})
	return raw
}

type identityFixture struct {
	h        *Handler
	manager  *coreauth.Manager
	authDir  string
	target   string
	other    string
	files    map[string][]byte
	inMemory map[string]*coreauth.Auth
}

// newIdentityFixture writes a re-auth target file and an unrelated account
// file into a real auth dir, registers both with the manager, and snapshots
// their bytes and in-memory state.
func newIdentityFixture(t *testing.T, provider string, targetMeta, otherMeta map[string]any, targetName, otherName string) *identityFixture {
	t.Helper()
	h, manager, authDir := newEgressTestHandler(t, "")
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	h.tokenStore = store
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	fx := &identityFixture{h: h, manager: manager, authDir: authDir, target: targetName, other: otherName,
		files: map[string][]byte{}, inMemory: map[string]*coreauth.Auth{}}
	for name, meta := range map[string]map[string]any{targetName: targetMeta, otherName: otherMeta} {
		raw, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		path := filepath.Join(authDir, name)
		if err = os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		auth, err := h.buildAuthFromFileData(path, raw)
		if err != nil {
			t.Fatalf("build auth %s: %v", name, err)
		}
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return fx
}

// snapshot records file bytes and manager state AFTER the auth URL request,
// so only what the token-exchange goroutine does is compared.
func (fx *identityFixture) snapshot(t *testing.T) {
	t.Helper()
	for _, name := range []string{fx.target, fx.other} {
		raw, err := os.ReadFile(filepath.Join(fx.authDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fx.files[name] = raw
		auth, ok := fx.manager.GetByID(name)
		if !ok {
			t.Fatalf("auth %s not registered", name)
		}
		fx.inMemory[name] = auth
	}
}

func (fx *identityFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(fx.authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || entry.IsDir() {
			continue
		}
		if _, known := fx.files[name]; !known {
			t.Errorf("rejected re-auth created a new credential file %s", name)
		}
	}
	for name, want := range fx.files {
		got, errRead := os.ReadFile(filepath.Join(fx.authDir, name))
		if errRead != nil {
			t.Errorf("credential file %s disappeared: %v", name, errRead)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("rejected re-auth rewrote %s:\n got: %s\nwant: %s", name, got, want)
		}
		auth, ok := fx.manager.GetByID(name)
		if !ok {
			t.Errorf("in-memory auth %s disappeared", name)
			continue
		}
		if !reflect.DeepEqual(auth.Metadata, fx.inMemory[name].Metadata) || auth.Disabled != fx.inMemory[name].Disabled {
			t.Errorf("rejected re-auth changed in-memory auth %s", name)
		}
	}
}

func waitOAuthSessionSettled(t *testing.T, state string) (status string, done bool) {
	t.Helper()
	deadline := time.Now().Add(egressWaitTimeout)
	for time.Now().Before(deadline) {
		_, status, _, _, completed, ok := GetOAuthSessionDetails(state)
		if !ok {
			return "", false
		}
		if completed || isOAuthSessionCompleteStatus(status) {
			return status, true
		}
		if strings.TrimSpace(status) != "" {
			return status, false
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("OAuth session %s did not settle within %s", state, egressWaitTimeout)
	return "", false
}

func startCardReauth(t *testing.T, fx *identityFixture, path, provider string) string {
	t.Helper()
	code, state, body := egressCallAuthURL(t, fx.h, path, url.Values{"auth_name": {fx.target}})
	if code != http.StatusOK || state == "" {
		t.Fatalf("%s?auth_name=%s: status=%d body=%s, want 200", path, fx.target, code, body)
	}
	fx.snapshot(t)
	if _, err := WriteOAuthCallbackFileForPendingSession(fx.authDir, provider, state, "test-code", ""); err != nil {
		t.Fatalf("write callback file: %v", err)
	}
	return state
}

func claudeTargetMeta(email string) map[string]any {
	return map[string]any{
		"type":                             "claude",
		"email":                            email,
		"access_token":                     "old-access",
		"refresh_token":                    "old-refresh-target",
		"proxy_url":                        "http://127.0.0.1:9",
		"note":                             "target note",
		"managed_header_seed":              identityTargetSeed,
		coreauth.ClaudeDeviceIDMetadataKey: identityTargetDeviceID,
	}
}

// TestRequestAnthropicTokenReauthRejectsDifferentAccount: a card re-auth of
// target T completed while the browser is logged in as O must be rejected
// without writing any file or changing any in-memory auth.
func TestRequestAnthropicTokenReauthRejectsDifferentAccount(t *testing.T) {
	cases := []struct {
		name       string
		targetMeta map[string]any
		targetFile string
	}{
		{name: "email mismatch", targetMeta: claudeTargetMeta("target@example.test"), targetFile: "claude-target@example.test.json"},
		{name: "case-insensitive file-name fallback mismatch", targetMeta: func() map[string]any {
			m := claudeTargetMeta("")
			delete(m, "email")
			return m
		}(), targetFile: "claude-Target@Example.test.json"},
		{name: "target email unknown fails closed", targetMeta: func() map[string]any {
			m := claudeTargetMeta("")
			delete(m, "email")
			return m
		}(), targetFile: "claude-target-no-email.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useFakeTokenEndpoint(t, func(*http.Request) []byte { return claudeTokenResponse("other@example.test") })
			fx := newIdentityFixture(t, "claude", tc.targetMeta, claudeTargetMeta("other@example.test"),
				tc.targetFile, "claude-other@example.test.json")
			state := startCardReauth(t, fx, "/anthropic-auth-url", "anthropic")
			status, completed := waitOAuthSessionSettled(t, state)
			if completed || !strings.Contains(strings.ToLower(status), "does not match") && !strings.Contains(strings.ToLower(status), "cannot be verified") {
				t.Fatalf("session completed=%v status=%q, want an identity-mismatch error", completed, status)
			}
			if strings.Contains(status, "other@example.test") {
				t.Fatalf("session error leaks the full email: %q", status)
			}
			fx.assertUnchanged(t)
		})
	}
}

// TestRequestCodexTokenReauthRejectsDifferentAccount: a codex card re-auth
// must reject a login whose chatgpt_account_id or email differs from the
// target (and fail closed when the target has no account_id).
func TestRequestCodexTokenReauthRejectsDifferentAccount(t *testing.T) {
	targetMeta := func(accountID string) map[string]any {
		m := map[string]any{
			"type":                "codex",
			"email":               "target@example.test",
			"access_token":        "old-access",
			"refresh_token":       "old-refresh-target",
			"id_token":            fakeCodexIDToken("target@example.test", accountID, "plus"),
			"proxy_url":           "http://127.0.0.1:9",
			"note":                "target note",
			"managed_header_seed": identityTargetSeed,
		}
		if accountID != "" {
			m["account_id"] = accountID
		}
		return m
	}
	cases := []struct {
		name          string
		targetAccount string
		loginEmail    string
		loginAccount  string
	}{
		{name: "different account_id", targetAccount: "acct-target", loginEmail: "other@example.test", loginAccount: "acct-other"},
		{name: "same account_id different email", targetAccount: "acct-target", loginEmail: "other@example.test", loginAccount: "acct-target"},
		{name: "target account_id unknown fails closed", targetAccount: "", loginEmail: "target@example.test", loginAccount: "acct-target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useEgressRecordingCodexService(t)
			useFakeTokenEndpoint(t, func(*http.Request) []byte {
				return codexTokenResponse(tc.loginEmail, tc.loginAccount, "pro")
			})
			otherMeta := targetMeta("acct-other")
			otherMeta["email"] = "other@example.test"
			fx := newIdentityFixture(t, "codex", targetMeta(tc.targetAccount), otherMeta,
				"codex-target@example.test-plus.json", "codex-other@example.test-pro.json")
			state := startCardReauth(t, fx, "/codex-auth-url", "codex")
			status, completed := waitOAuthSessionSettled(t, state)
			if completed || !strings.Contains(strings.ToLower(status), "does not match") && !strings.Contains(strings.ToLower(status), "cannot be verified") {
				t.Fatalf("session completed=%v status=%q, want an identity-mismatch error", completed, status)
			}
			if strings.Contains(status, "other@example.test") {
				t.Fatalf("session error leaks the full email: %q", status)
			}
			fx.assertUnchanged(t)
		})
	}
}

func savedCredential(t *testing.T, authDir, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(authDir, name))
	if err != nil {
		t.Fatalf("read saved %s: %v", name, err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode saved %s: %v", name, err)
	}
	return out
}

// TestRequestAnthropicTokenReauthSameAccountPreservesIdentity: a matching card
// re-auth rewrites the target's file in place with the new tokens while keeping
// its proxy_url, note, managed_header_seed and device id.
func TestRequestAnthropicTokenReauthSameAccountPreservesIdentity(t *testing.T) {
	useFakeTokenEndpoint(t, func(*http.Request) []byte { return claudeTokenResponse("Target@Example.test") })
	fx := newIdentityFixture(t, "claude", claudeTargetMeta("target@example.test"), claudeTargetMeta("other@example.test"),
		"claude-target@example.test.json", "claude-other@example.test.json")
	state := startCardReauth(t, fx, "/anthropic-auth-url", "anthropic")
	if status, completed := waitOAuthSessionSettled(t, state); !completed {
		t.Fatalf("same-account re-auth did not complete: status=%q", status)
	}
	saved := savedCredential(t, fx.authDir, fx.target)
	if saved["refresh_token"] != "new-refresh-Target@Example.test" {
		t.Fatalf("saved refresh_token = %v, want the new token", saved["refresh_token"])
	}
	for key, want := range map[string]any{
		"proxy_url":                        "http://127.0.0.1:9",
		"note":                             "target note",
		"managed_header_seed":              identityTargetSeed,
		coreauth.ClaudeDeviceIDMetadataKey: identityTargetDeviceID,
	} {
		if saved[key] != want {
			t.Fatalf("saved %s = %v, want %v", key, saved[key], want)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(fx.authDir, fx.other)); !bytes.Equal(raw, fx.files[fx.other]) {
		t.Fatalf("same-account re-auth touched the unrelated credential %s", fx.other)
	}
}

// TestRequestCodexTokenReauthSameAccountPreservesSettings: a codex card re-auth
// of the same account_id (plan change allowed) keeps the target's proxy_url,
// note, seed and its existing account_settings (extra_headers,
// refresh_enabled, tls_profile, runtime identity created_at).
func TestRequestCodexTokenReauthSameAccountPreservesSettings(t *testing.T) {
	useEgressRecordingCodexService(t)
	useFakeTokenEndpoint(t, func(*http.Request) []byte {
		return codexTokenResponse("target@example.test", "acct-target", "plus")
	})
	refreshEnabled := false
	targetMeta := map[string]any{
		"type":                "codex",
		"email":               "target@example.test",
		"account_id":          "acct-target",
		"access_token":        "old-access",
		"refresh_token":       "old-refresh-target",
		"proxy_url":           "http://127.0.0.1:9",
		"note":                "target note",
		"managed_header_seed": identityTargetSeed,
		"account_settings": map[string]any{
			"schema_version":  1,
			"extra_headers":   map[string]any{"X-Operator-Extra": "keep-me"},
			"refresh_enabled": refreshEnabled,
			"tls_profile":     "codex_rustls_native_v1",
			"runtime_identity_state": map[string]any{
				"current": map[string]any{"identity_id": "ari_target", "created_at": identityTargetCreated, "revision": 3},
			},
		},
	}
	otherMeta := map[string]any{"type": "codex", "email": "other@example.test", "account_id": "acct-other",
		"refresh_token": "old-refresh-other", "proxy_url": "http://127.0.0.1:9"}
	fx := newIdentityFixture(t, "codex", targetMeta, otherMeta,
		"codex-target@example.test-plus.json", "codex-other@example.test-pro.json")
	state := startCardReauth(t, fx, "/codex-auth-url", "codex")
	status, completed := waitOAuthSessionSettled(t, state)
	if !completed {
		t.Fatalf("same-account codex re-auth did not complete: status=%q", status)
	}
	var saved map[string]any
	entries, _ := os.ReadDir(fx.authDir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "target@example.test") {
			saved = savedCredential(t, fx.authDir, entry.Name())
		}
	}
	if saved == nil {
		t.Fatal("no saved credential for the target account")
	}
	if saved["refresh_token"] != "new-refresh-acct-target" {
		t.Fatalf("saved refresh_token = %v, want the new token", saved["refresh_token"])
	}
	for key, want := range map[string]any{"proxy_url": "http://127.0.0.1:9", "note": "target note", "managed_header_seed": identityTargetSeed} {
		if saved[key] != want {
			t.Fatalf("saved %s = %v, want %v", key, saved[key], want)
		}
	}
	settings, _ := saved["account_settings"].(map[string]any)
	if settings == nil {
		t.Fatal("saved account_settings missing")
	}
	if headers, _ := settings["extra_headers"].(map[string]any); headers["X-Operator-Extra"] != "keep-me" {
		t.Fatalf("account_settings.extra_headers = %v, want the target's operator header kept", settings["extra_headers"])
	}
	if settings["refresh_enabled"] != false {
		t.Fatalf("account_settings.refresh_enabled = %v, want false kept", settings["refresh_enabled"])
	}
	if settings["tls_profile"] != "codex_rustls_native_v1" {
		t.Fatalf("account_settings.tls_profile = %v, want codex_rustls_native_v1 kept", settings["tls_profile"])
	}
	rid, _ := settings["runtime_identity_state"].(map[string]any)
	current, _ := rid["current"].(map[string]any)
	if current["created_at"] != identityTargetCreated {
		t.Fatalf("runtime_identity_state.current.created_at = %v, want %s kept", current["created_at"], identityTargetCreated)
	}
}

// TestRequestAnthropicTokenReauthFallbackStaysOnTargetProxy: when the primary
// account-scoped exchange fails with a retriable network error, the
// same-account fallback must still dial the TARGET proxy and never the global
// proxy (a sentinel here).
func TestRequestAnthropicTokenReauthFallbackStaysOnTargetProxy(t *testing.T) {
	var primaryCalls atomic.Int32
	orig := oauthExchangeTransportForTest
	oauthExchangeTransportForTest = func(*coreauth.Auth) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			primaryCalls.Add(1)
			return nil, io.ErrUnexpectedEOF
		})
	}
	t.Cleanup(func() { oauthExchangeTransportForTest = orig })

	targetProxy := newEgressConnectProxy(t, http.StatusOK)
	globalSentinel := newEgressConnectProxy(t, http.StatusForbidden)
	h, manager, authDir := newEgressTestHandler(t, globalSentinel.URL())
	const targetName = "claude-target@example.test.json"
	registerEgressTarget(t, manager, "claude", targetName, targetProxy.URL())

	code, state, body := egressCallAuthURL(t, h, "/anthropic-auth-url", url.Values{"auth_name": {targetName}})
	if code != http.StatusOK || state == "" {
		t.Fatalf("anthropic-auth-url: status=%d body=%s", code, body)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(authDir, "anthropic", state, "test-code", ""); err != nil {
		t.Fatalf("write callback file: %v", err)
	}
	capture, ok := targetProxy.waitHello(egressWaitTimeout)
	if !ok {
		t.Fatalf("fallback exchange never reached the target proxy (primary attempts=%d, global CONNECTs=%v)", primaryCalls.Load(), globalSentinel.connects())
	}
	if primaryCalls.Load() == 0 {
		t.Fatal("primary exchange was never attempted; fallback path not exercised")
	}
	if capture.target != "api.anthropic.com:443" {
		t.Fatalf("fallback CONNECT = %q, want api.anthropic.com:443", capture.target)
	}
	if got := globalSentinel.connects(); len(got) != 0 {
		t.Fatalf("fallback dialed the global proxy: %v", got)
	}
}
