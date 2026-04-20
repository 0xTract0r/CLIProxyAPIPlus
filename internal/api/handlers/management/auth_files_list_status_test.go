package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestListAuthFilesIncludesLiveStatusFields(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:            "claude-status.json",
		FileName:      "claude-status.json",
		Provider:      "claude",
		Label:         "claude@example.com",
		Status:        coreauth.StatusError,
		StatusMessage: "token refresh failed with status 401: invalid_grant",
		Unavailable:   true,
		Attributes: map[string]string{
			"path": "/tmp/claude-status.json",
		},
		Metadata: map[string]any{
			"type":  "claude",
			"email": "claude@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Files []struct {
			Name          string          `json:"name"`
			Status        coreauth.Status `json:"status"`
			StatusMessage string          `json:"status_message"`
			Unavailable   bool            `json:"unavailable"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	file := payload.Files[0]
	if file.Name != "claude-status.json" {
		t.Fatalf("name = %q, want %q", file.Name, "claude-status.json")
	}
	if file.Status != coreauth.StatusError {
		t.Fatalf("status = %q, want %q", file.Status, coreauth.StatusError)
	}
	if file.StatusMessage != "token refresh failed with status 401: invalid_grant" {
		t.Fatalf("status_message = %q, want exact live status message", file.StatusMessage)
	}
	if !file.Unavailable {
		t.Fatal("expected unavailable to be true in list response")
	}
}
