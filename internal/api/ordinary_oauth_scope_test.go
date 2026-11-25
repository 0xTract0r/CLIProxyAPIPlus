package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	management "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

func TestOrdinaryOAuthGenerationAndCancellation(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "fixture-key")
	s := newTestServer(t)
	a := t.TempDir()
	request := func(server *Server, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer fixture-key")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		return rec
	}
	start := func(state string) {
		t.Helper()
		if err := s.mgmt.RegisterFileOAuthSession(state, "devin", a); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { management.CompleteOAuthSession(state) })
	}
	callback := func(server *Server, state string) *httptest.ResponseRecorder {
		return request(server, http.MethodGet, "/devin/callback?state="+state+"&code=fixture-code")
	}
	setDirectory := func(dir string) {
		next := s.cfg.CloneForRuntime()
		next.SourceHash = ""
		next.AuthDir = dir
		s.mgmt.SetConfig(next)
	}
	setDirectory(a)
	const old = "generation-fixture"
	start(old)
	other := newTestServer(t)
	otherCfg := other.cfg.CloneForRuntime()
	otherCfg.SourceHash = ""
	otherCfg.AuthDir = a
	other.mgmt.SetConfig(otherCfg)
	if got := request(other, http.MethodDelete, "/v8/management/oauth/session?state="+old); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"cancelled":false`) {
		t.Fatalf("other instance cancel HTTP %d", got.Code)
	}
	if got := callback(other, old); got.Code != http.StatusBadRequest {
		t.Fatalf("other instance: HTTP %d", got.Code)
	}
	b := t.TempDir()
	setDirectory(b)
	if got := callback(s, old); got.Code != http.StatusBadRequest {
		t.Fatalf("after switch: HTTP %d", got.Code)
	}
	setDirectory(a)
	if got := callback(s, old); got.Code != http.StatusBadRequest {
		t.Fatalf("after switch back: HTTP %d", got.Code)
	}
	for _, dir := range []string{a, b, other.cfg.AuthDir} {
		if _, err := os.Stat(filepath.Join(dir, ".oauth-devin-"+old+".oauth")); !os.IsNotExist(err) {
			t.Fatalf("stale file: %v", err)
		}
	}
	start("fresh-fixture")
	if got := callback(s, "fresh-fixture"); got.Code != http.StatusOK {
		t.Fatalf("fresh state: HTTP %d", got.Code)
	}
	start("cancel-fixture")
	if got := request(s, http.MethodDelete, "/v8/management/oauth/session?state=cancel-fixture"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel response: HTTP %d %s", got.Code, got.Body.String())
	}
	if got := request(s, http.MethodGet, "/v8/management/oauth/status?state=cancel-fixture"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("poll response: HTTP %d %s", got.Code, got.Body.String())
	}
	if got := callback(s, "cancel-fixture"); got.Code != http.StatusBadRequest {
		t.Fatalf("late cancel: HTTP %d", got.Code)
	}
	management.RegisterOAuthSession("unbound-fixture", "devin")
	t.Cleanup(func() { management.CompleteOAuthSession("unbound-fixture") })
	if got := callback(s, "unbound-fixture"); got.Code != http.StatusBadRequest {
		t.Fatalf("unbound: HTTP %d", got.Code)
	}
}
