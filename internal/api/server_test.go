package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath)
}

func withBuildInfo(t *testing.T, version, commit, buildDate string) {
	t.Helper()

	originalVersion := buildinfo.Version
	originalCommit := buildinfo.Commit
	originalBuildDate := buildinfo.BuildDate
	buildinfo.Version = version
	buildinfo.Commit = commit
	buildinfo.BuildDate = buildDate
	t.Cleanup(func() {
		buildinfo.Version = originalVersion
		buildinfo.Commit = originalCommit
		buildinfo.BuildDate = originalBuildDate
	})
}

func TestBuildInfoHeadersAvailableOnProofEndpoints(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "route-key")
	withBuildInfo(t, "test-version-plus", "test-commit", "2026-04-30T00:00:00Z")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	staticDir := filepath.Join(tmpDir, "static")
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		t.Fatalf("failed to create static dir: %v", err)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if err := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatalf("failed to write management asset: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	server := NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))

	testCases := []struct {
		name string
		path string
		key  bool
	}{
		{name: "health", path: "/healthz"},
		{name: "management asset", path: "/management.html"},
		{name: "auth files", path: "/v0/management/auth-files", key: true},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.key {
				req.Header.Set("Authorization", "Bearer route-key")
			}

			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want %d; body=%s", tc.path, rr.Code, http.StatusOK, rr.Body.String())
			}
			if got := rr.Header().Get("X-CPA-VERSION"); got != "test-version-plus" {
				t.Fatalf("%s X-CPA-VERSION = %q, want %q", tc.path, got, "test-version-plus")
			}
			if got := rr.Header().Get("X-CPA-COMMIT"); got != "test-commit" {
				t.Fatalf("%s X-CPA-COMMIT = %q, want %q", tc.path, got, "test-commit")
			}
			if got := rr.Header().Get("X-CPA-BUILD-DATE"); got != "2026-04-30T00:00:00Z" {
				t.Fatalf("%s X-CPA-BUILD-DATE = %q, want %q", tc.path, got, "2026-04-30T00:00:00Z")
			}
		})
	}
}

func TestProviderTLSProbeRouteRequiresManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "route-key")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
		RemoteManagement: proxyconfig.RemoteManagement{
			AllowRemote: true,
		},
	}
	server := NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))

	req := httptest.NewRequest(http.MethodPost, "/v0/management/diagnostics/provider-tls-probe", strings.NewReader(`{"name":"codex.json","provider":"codex"}`))
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing management key") {
		t.Fatalf("body = %q, want missing management key", rr.Body.String())
	}
}

func TestAmpProviderModelRoutes(t *testing.T) {
	testCases := []struct {
		name         string
		path         string
		wantStatus   int
		wantContains string
	}{
		{
			name:         "openai root models",
			path:         "/api/provider/openai/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "groq root models",
			path:         "/api/provider/groq/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "openai models",
			path:         "/api/provider/openai/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "anthropic models",
			path:         "/api/provider/anthropic/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"data"`,
		},
		{
			name:         "google models v1",
			path:         "/api/provider/google/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
		{
			name:         "google models v1beta",
			path:         "/api/provider/google/v1beta/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer test-key")

			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("unexpected status code for %s: got %d want %d; body=%s", tc.path, rr.Code, tc.wantStatus, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantContains) {
				t.Fatalf("response body for %s missing %q: %s", tc.path, tc.wantContains, body)
			}
		})
	}
}

func TestManagementFrontendOAuthRoutesRegistered(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "route-key")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	manager := auth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &auth.Auth{
		ID:       "codex-route.json",
		FileName: "codex-route.json",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{
			"type":  "codex",
			"email": "route@example.com",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	server := NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))

	testCases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "oauth reauth history",
			method:     http.MethodGet,
			path:       "/v0/management/oauth-reauth-history?limit=5",
			wantStatus: http.StatusOK,
		},
		{
			name:       "auth status history",
			method:     http.MethodGet,
			path:       "/v0/management/auth-status-history?limit=5",
			wantStatus: http.StatusOK,
		},
		{
			name:       "refresh auth status",
			method:     http.MethodPost,
			path:       "/v0/management/auth-files/refresh-status",
			body:       `{"name":"codex-route.json"}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "test auth file message",
			method:     http.MethodPost,
			path:       "/v0/management/auth-files/test-message",
			body:       `{"name":""}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "cancel oauth session",
			method:     http.MethodDelete,
			path:       "/v0/management/oauth-session?state=route-test-state",
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("X-Management-Key", "route-key")
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d; body=%s", tc.method, tc.path, rr.Code, tc.wantStatus, rr.Body.String())
			}
			if rr.Code == http.StatusNotFound && strings.TrimSpace(rr.Body.String()) == "" {
				t.Fatalf("%s %s still appears to be an unregistered route", tc.method, tc.path)
			}
		})
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fallback to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}
