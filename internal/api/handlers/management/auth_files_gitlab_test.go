package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPostOAuthCallback_GitLabWritesPendingCallbackFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	state := "gitlab-state-123"
	t.Cleanup(func() { CompleteOAuthSession(state) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	if err := h.RegisterFileOAuthSession(state, "gitlab", authDir); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(`{"provider":"gitlab","redirect_url":"http://localhost:17171/auth/callback?code=test-code&state=`+state+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PostOAuthCallback(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	filePath := filepath.Join(authDir, ".oauth-gitlab-"+state+".oauth")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read callback file: %v", err)
	}

	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode callback payload: %v", err)
	}
	if got := payload["code"]; got != "test-code" {
		t.Fatalf("callback code = %q, want test-code", got)
	}
	if got := payload["state"]; got != state {
		t.Fatalf("callback state = %q, want %q", got, state)
	}
}
