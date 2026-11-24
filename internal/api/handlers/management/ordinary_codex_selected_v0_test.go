package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryCodexV0SelectedReauth(t *testing.T) {
	previous := newCodexOAuthService
	starts := 0
	newCodexOAuthService = func(*config.Config) codexOAuthService {
		starts++
		return &fakeCodexOAuthService{}
	}
	t.Cleanup(func() { newCodexOAuthService = previous })
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.tokenStore = store
	const initial = "{\"type\":\"codex\",\"email\":\"codex-selected@example.test\",\"note\":\"keep\"}\n"
	add := func(name, provider string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := manager.Register(context.Background(), &coreauth.Auth{ID: name, FileName: name, Provider: provider,
			Attributes: map[string]string{coreauth.AttributePath: path},
			Metadata:   map[string]any{"type": provider, "email": "codex-selected@example.test", "note": "keep"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("selected.json", "codex")
	add("other.json", "codex")
	add("wrong-provider.json", "claude")
	// Simulate legacy fields that this OAuth flow does not own. The manager has
	// already registered the credential, as it would after a watcher refresh.
	legacy := []byte(`{"type":"codex","email":"codex-selected@example.test","note":"keep","disabled":true,"legacy":9007199254740993,"custom":{"mode":"leave unchanged"}}`)
	if err := os.WriteFile(filepath.Join(dir, "selected.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/codex-auth-url", h.RequestCodexTokenV0)
	router.GET("/get-auth-status", h.GetAuthStatus)
	router.POST("/oauth-callback", h.PostOAuthCallback)
	router.DELETE("/oauth-session", h.CancelAuthSession)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, caseResult := range []struct {
		query, errorText string
		status           int
	}{
		{"auth_name=", "invalid auth_name", 400},
		{"auth_name=selected.json&auth_name=other.json", "invalid auth_name", 400},
		{"auth_name=..%2Fselected.json", "invalid auth_name", 400},
		{"auth_name=renamed-away.json", "auth file not found", 404},
		{"auth_name=wrong-provider.json", "provider does not match", 400},
	} {
		resp := get("/codex-auth-url?" + caseResult.query)
		if resp.Code != caseResult.status || !strings.Contains(resp.Body.String(), caseResult.errorText) || strings.Contains(resp.Body.String(), "\"state\"") {
			t.Fatalf("preflight %s: got %d %s", caseResult.query, resp.Code, resp.Body.String())
		}
	}
	if starts != 0 {
		t.Fatalf("invalid selection initialized %d OAuth services", starts)
	}
	start := func(name string) string {
		t.Helper()
		resp := get("/codex-auth-url?is_webui=true&auth_name=" + url.QueryEscape(name))
		var payload struct {
			State      string
			TargetName string `json:"target_auth_name"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil || resp.Code != 200 || payload.State == "" || payload.TargetName != name {
			t.Fatalf("start status %d, body %s, error %v", resp.Code, resp.Body.String(), err)
		}
		t.Cleanup(func() { CancelOAuthSession(payload.State) })
		if target, ok := GetOAuthSessionTarget(payload.State); !ok || target.FileName != name || target.SelectedFileRevision == "" {
			t.Fatal("missing target on registered session")
		}
		return payload.State
	}
	callback := func(state string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"provider": "codex", "redirect_url": "http://localhost/callback?code=selected&state=" + url.QueryEscape(state)})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/oauth-callback", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("callback status %d: %s", rec.Code, rec.Body.String())
		}
	}
	first := start("selected.json")
	second := start("other.json")
	if first == second {
		t.Fatal("sessions reused state")
	}
	callback(first)
	waitForOAuthSessionDone(t, first)
	var won struct{ Status, AuthName string }
	completed := get("/get-auth-status?state=" + url.QueryEscape(first))
	if err := json.Unmarshal(completed.Body.Bytes(), &won); err != nil || won.Status != "ok" {
		t.Fatalf("selected grant did not complete: %s", completed.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "selected.json"))
	if err != nil || !strings.Contains(string(raw), "access-selected") || !strings.Contains(string(raw), "keep") {
		t.Fatalf("selected file update did not preserve metadata: %v", err)
	}
	var untouched map[string]json.RawMessage
	if err := json.Unmarshal(raw, &untouched); err != nil || string(untouched["legacy"]) != "9007199254740993" || string(untouched["disabled"]) != "true" || string(untouched["custom"]) != `{"mode":"leave unchanged"}` {
		t.Fatal("reauthentication changed fields outside the login grant")
	}
	beforeMove, err := os.ReadFile(filepath.Join(dir, "other.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "other.json"), filepath.Join(dir, "renamed.json")); err != nil {
		t.Fatal(err)
	}
	callback(second)
	waitForOAuthSessionDone(t, second)
	rejected := get("/get-auth-status?state=" + url.QueryEscape(second))
	if !strings.Contains(rejected.Body.String(), "removed, renamed or changed") {
		t.Fatalf("late callback did not reject renamed target: %s", rejected.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "other.json")); !os.IsNotExist(err) {
		t.Fatalf("renamed target was recreated: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "renamed.json")); err != nil || string(raw) != string(beforeMove) {
		t.Fatalf("rename destination changed: %v", err)
	}
	// The post-auth hook runs after session/target validation and before the store commits.
	// Change the file at that exact point, as an overlapping editor or watcher could.
	for _, mode := range []string{"replaced", "removed", "same-bytes-new-file"} {
		name := mode + ".json"
		add(name, "codex")
		path := filepath.Join(dir, name)
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		replacement := []byte(`{"type":"codex","email":"another@example.test","note":"protected"}`)
		if mode == "same-bytes-new-file" {
			replacement = original
		}
		h.SetPostAuthHook(func(_ context.Context, record *coreauth.Auth) error {
			if record.ID != name {
				return nil
			}
			if mode == "removed" {
				return os.Remove(path)
			}
			next := path + ".new"
			if err := os.WriteFile(next, replacement, 0600); err != nil {
				return err
			}
			return os.Rename(next, path)
		})
		state := start(name)
		callback(state)
		waitForOAuthSessionDone(t, state)
		var outcome struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(get("/get-auth-status?state="+url.QueryEscape(state)).Body.Bytes(), &outcome); err != nil || outcome.Status != "error" {
			t.Fatalf("%s reauthentication was not rejected", mode)
		}
		bytes, err := os.ReadFile(path)
		if mode == "removed" {
			if !os.IsNotExist(err) {
				t.Fatalf("removed credential recreated, err=%v", err)
			}
		} else if err != nil || string(bytes) != string(replacement) {
			t.Fatalf("%s credential overwritten", mode)
		}
	}
	h.SetPostAuthHook(nil)
	if starts != 5 {
		t.Fatalf("unexpected OAuth starts: %d", starts)
	}
}
