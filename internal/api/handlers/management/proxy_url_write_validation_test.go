package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPatchAuthFileFields_RejectsInvalidProxyURL verifies a PATCH that sets an
// invalid proxy_url is rejected with 400 and the previously stored value is left
// untouched (fail-closed: an invalid account proxy must never be persisted).
func TestPatchAuthFileFields_RejectsInvalidProxyURL(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "codex.json",
		FileName:   "codex.json",
		Provider:   "codex",
		ProxyURL:   "socks5://good.proxy:1080",
		Attributes: map[string]string{"path": "/tmp/codex.json"},
		Metadata: map[string]any{
			"type":      "codex",
			"proxy_url": "socks5://good.proxy:1080",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"codex.json","proxy_url":"ftp://bad-scheme:1"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid proxy_url, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to still exist")
	}
	if got, _ := updated.Metadata["proxy_url"].(string); got != "socks5://good.proxy:1080" {
		t.Fatalf("metadata.proxy_url = %q, want unchanged %q", got, "socks5://good.proxy:1080")
	}
	if updated.ProxyURL != "socks5://good.proxy:1080" {
		t.Fatalf("ProxyURL = %q, want unchanged %q", updated.ProxyURL, "socks5://good.proxy:1080")
	}
}

// TestPatchAuthFileFields_AllowsValidProxyURL is the negative control: a valid
// proxy_url PATCH still succeeds and persists.
func TestPatchAuthFileFields_AllowsValidProxyURL(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "codex.json",
		FileName:   "codex.json",
		Provider:   "codex",
		Attributes: map[string]string{"path": "/tmp/codex.json"},
		Metadata:   map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"codex.json","proxy_url":"socks5://proxy.remote:1080"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d for valid proxy_url, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID("codex.json")
	if updated.ProxyURL != "socks5://proxy.remote:1080" {
		t.Fatalf("ProxyURL = %q, want %q", updated.ProxyURL, "socks5://proxy.remote:1080")
	}
}

// TestUploadAuthFile_RejectsInvalidProxyURL verifies a raw-body upload whose JSON
// carries an invalid proxy_url is rejected with 400 and the file is never written
// to disk (validated before os.WriteFile).
func TestUploadAuthFile_RejectsInvalidProxyURL(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	content := `{"type":"codex","email":"x@example.com","proxy_url":"ftp://bad-scheme:1"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=codex-bad.json", strings.NewReader(content))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid proxy_url upload, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(authDir, "codex-bad.json")); !os.IsNotExist(err) {
		t.Fatalf("auth file must not be written on invalid proxy_url; stat err = %v", err)
	}
}

// TestPutProxyURL_RejectsInvalidValue verifies the global proxy-url endpoint rejects
// an invalid value with 400 and leaves the existing cfg.ProxyURL untouched.
func TestPutProxyURL_RejectsInvalidValue(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.ProxyURL = "http://good.proxy:8080"
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	body := `{"value":"ftp://bad-scheme:1"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/proxy-url", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PutProxyURL(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid global proxy_url, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if cfg.ProxyURL != "http://good.proxy:8080" {
		t.Fatalf("cfg.ProxyURL = %q, want unchanged %q", cfg.ProxyURL, "http://good.proxy:8080")
	}
}
