package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCancelOAuthSessionMarksStatusCancelled(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	state := "cancel-test-state"
	t.Cleanup(func() { CompleteOAuthSession(state) })

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	if err := h.RegisterFileOAuthSession(state, "codex", authDir); err != nil {
		t.Fatal(err)
	}

	cancelRec := httptest.NewRecorder()
	cancelCtx, _ := gin.CreateTestContext(cancelRec)
	cancelCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/oauth-session?state="+state, nil)
	h.CancelAuthSession(cancelCtx)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelPayload map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelPayload); err != nil {
		t.Fatalf("decode cancel payload: %v", err)
	}
	if cancelPayload["status"] != "ok" || cancelPayload["cancelled"] != true {
		t.Fatalf("unexpected cancel payload: %#v", cancelPayload)
	}

	statusRec := httptest.NewRecorder()
	statusCtx, _ := gin.CreateTestContext(statusRec)
	statusCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/get-auth-status?state="+state, nil)
	h.GetAuthStatus(statusCtx)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", statusRec.Code, statusRec.Body.String())
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if statusPayload["status"] != oauthCancelledStatus {
		t.Fatalf("status payload = %#v, want cancelled", statusPayload)
	}
}
