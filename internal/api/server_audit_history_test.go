package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrdinaryAuditHistoryRegisteredAndScoped(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "fixture-management")
	s := newTestServer(t)
	scoped := func(server *Server, dir string) {
		cfg := server.cfg.CloneForRuntime()
		cfg.SourceHash = ""
		cfg.AuthDir = dir
		server.mgmt.SetConfig(cfg)
	}
	call := func(server *Server, route string, authorized bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management"+route, nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer fixture-management")
		}
		server.engine.ServeHTTP(w, req)
		return w
	}
	a, b := t.TempDir(), t.TempDir()
	other := newTestServer(t)
	scoped(s, a)
	scoped(other, b)
	for _, tc := range []struct{ route, file, fixture string }{
		{"/auth-status-history", ".auth-status-history/status.jsonl", `{"event_type":"fixture","auth_name":"item.json","status":"disabled","error":"access_token=secret-fixture"}`},
		{"/oauth-reauth-history", ".oauth-history/reauth.jsonl", `{"event_type":"fixture","target_auth_file":"item.json","before":{"plan":"test-plan"},"error":"Bearer secret-fixture"}`},
	} {
		t.Run(tc.route, func(t *testing.T) {
			path := filepath.Join(a, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.fixture+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if got := call(s, tc.route, false); got.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated GET %d", got.Code)
			}
			if got := call(s, tc.route+"?auth_name=item.json&limit=1", true); got.Code != http.StatusOK ||
				!strings.Contains(got.Body.String(), "item.json") || !strings.Contains(got.Body.String(), "[redacted]") ||
				strings.Contains(got.Body.String(), "secret-fixture") {
				t.Fatalf("GET did not preserve safe audit fields: %d %s", got.Code, got.Body)
			}
			if got := call(other, tc.route, true); got.Code != http.StatusOK || strings.Contains(got.Body.String(), "item.json") {
				t.Fatalf("other directory exposed history: %d %s", got.Code, got.Body)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(b, "outside.jsonl"), path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b, "outside.jsonl"), []byte(tc.fixture), 0600); err != nil {
				t.Fatal(err)
			}
			if got := call(s, tc.route, true); got.Code != http.StatusInternalServerError || strings.Contains(got.Body.String(), b) {
				t.Fatalf("linked source returned unsafe response: %d %s", got.Code, got.Body)
			}
		})
	}
}
