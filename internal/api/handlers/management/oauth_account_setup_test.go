package management

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildAuthFromFileDataRestoresRuntimeProxyURL(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex-proxy.json")
	data := []byte(`{"type":"codex","email":"proxy@example.test","proxy_url":"socks5://proxy.example:1080","headers":{"Accept-Language":"en-US"}}`)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		t.Fatalf("build auth: %v", err)
	}
	if got := auth.ProxyURL; got != "socks5://proxy.example:1080" {
		t.Fatalf("ProxyURL = %q, want %q", got, "socks5://proxy.example:1080")
	}
	if got := auth.Attributes["header:Accept-Language"]; got != "en-US" {
		t.Fatalf("header:Accept-Language = %q, want ordinary custom header", got)
	}
}

func TestGetAuthStatusReturnsCompletedOAuthSavedInfo(t *testing.T) {
	authDir := t.TempDir()
	state := "ordinary-completed-oauth"
	owner := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	if err := owner.RegisterFileOAuthSession(state, "codex", authDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CompleteOAuthSession(state) })
	CompleteOAuthSessionWithOutcome(state, "ordinary.json", "", true, false, false, 0, 0, 0, 0, "", "")
	got := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(got)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/get-auth-status?state="+state, nil)
	owner.GetAuthStatus(ctx)
	body := got.Body.String()
	for _, required := range []string{`"status":"ok"`, `"auth_id":"ordinary.json"`, `"saved":true`, `"adopted":false`} {
		if got.Code != 200 || !strings.Contains(body, required) {
			t.Fatalf("bad OAuth status: %d %s", got.Code, body)
		}
	}
	for _, private := range []string{authDir, "saved_path", "proxy_url"} {
		if strings.Contains(body, private) {
			t.Fatalf("OAuth completion exposes internal information")
		}
	}
	foreign := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	out := httptest.NewRecorder()
	other, _ := gin.CreateTestContext(out)
	other.Request = httptest.NewRequest(http.MethodGet, "/v0/management/get-auth-status?state="+state, nil)
	foreign.GetAuthStatus(other)
	if strings.Contains(out.Body.String(), "ordinary.json") || !strings.Contains(out.Body.String(), "unknown or expired state") {
		t.Fatal("other auth directory read completed OAuth data")
	}
}
