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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func TestCredentialProxyValidationBeforeWriteRegisterAndHook(t *testing.T) {
	dir := t.TempDir()
	name := "ordinary-credential.json"
	path := filepath.Join(dir, name)
	const initial = `{"type":"codex","proxy_url":"http://existing.example.invalid:8080","legacy":9007199254740993}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: name, FileName: name, Provider: "codex", ProxyURL: "http://existing.example.invalid:8080",
		Attributes: map[string]string{"path": path},
		Metadata:   map[string]any{"type": "codex", "proxy_url": "http://existing.example.invalid:8080"},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	handler.tokenStore = store
	hookCalls := 0
	handler.postAuthPersistHook = func(context.Context, *coreauth.Auth) error {
		hookCalls++
		return nil
	}
	router := gin.New()
	router.PATCH("/fields", handler.PatchAuthFileFields)
	router.POST("/v0/auth-files", handler.UploadAuthFile)
	router.POST("/v8/credentials", handler.UploadAuthFileV8)
	send := func(method, endpoint, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, endpoint, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	for _, part := range []string{
		`"proxy_url":"http://bad.example.invalid:90000"`,
		`"proxy-url":false`,
		`"proxy_url.host":"bad.example.invalid"`,
	} {
		resp := send(http.MethodPatch, "/fields", `{"name":"`+name+`","note":"must not publish",`+part+"}")
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("PATCH rejected invalid proxy with %d: %s", resp.Code, resp.Body.String())
		}
	}
	for _, route := range []string{"/v0/auth-files", "/v8/credentials"} {
		for _, fixture := range []string{name, "must-not-register.json"} {
			resp := send(http.MethodPost, route+"?name="+fixture, `{"type":"codex","proxy-url":"ftp://invalid.example.invalid"}`)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("%s invalid upload got %d: %s", route, resp.Code, resp.Body.String())
			}
		}
	}
	if hookCalls != 0 {
		t.Fatalf("rejected request triggered %d persist hooks", hookCalls)
	}
	if _, ok := manager.GetByID("must-not-register.json"); ok {
		t.Fatal("invalid upload registered a credential")
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-register.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid upload produced a path: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != initial {
		t.Fatalf("invalid request altered existing credential bytes: %v", err)
	}
	before, ok := manager.GetByID(name)
	if !ok || before.ProxyURL != "http://existing.example.invalid:8080" || before.Metadata["note"] != nil {
		t.Fatal("invalid request altered live credentials")
	}

	for _, value := range []string{`"direct"`, `""`, "null"} {
		resp := send(http.MethodPatch, "/fields", `{"name":"`+name+`","proxy_url":`+value+"}")
		if resp.Code != http.StatusOK {
			t.Fatalf("valid clear/direct %s got %d: %s", value, resp.Code, resp.Body.String())
		}
	}
	after, ok := manager.GetByID(name)
	if !ok || after.ProxyURL != "" {
		t.Fatal("null did not clear live proxy setting")
	}
}
