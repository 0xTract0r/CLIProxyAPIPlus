package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestRequestLogByIDReaderAnchorsToConfigDir reproduces the reader/writer log
// directory mismatch behind the always-404 bug on GET /request-log-by-id/{id}.
//
// The writer (defaultRequestLoggerFactory -> NewFileRequestLogger) resolves a
// relative logs directory against the config file's directory, while the reader
// (SetLogDirectory) previously resolved it against the process working
// directory. When cwd != configDir the writer wrote <configDir>/logs but the
// reader looked in <cwd>/logs, so every by-id lookup returned 404.
//
// This test forces ResolveLogDirectory to return the relative "logs" value
// (WRITABLE_PATH unset, a writable "logs" dir under cwd) and places the config
// file in a separate directory. It then drops a request-log file where the
// writer would put it (<configDir>/logs) and asserts the real reader route
// finds it. Before the fix this returns 404; after the fix it returns 200.
func TestRequestLogByIDReaderAnchorsToConfigDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Neutralize WRITABLE_PATH so ResolveLogDirectory can fall through to the
	// relative "logs" branch instead of an absolute WRITABLE_PATH/logs.
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	// cwd: a temp dir with a writable "logs" subdir so ResolveLogDirectory
	// returns the relative "logs". This is intentionally NOT the config dir.
	cwdDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwdDir, "logs"), 0o755); err != nil {
		t.Fatalf("failed to create cwd logs dir: %v", err)
	}
	t.Chdir(cwdDir)

	// configDir: a separate temp dir holding the config file. This is where the
	// writer anchors the relative "logs" directory.
	configDir := t.TempDir()
	if configDir == cwdDir {
		t.Fatalf("configDir and cwdDir unexpectedly equal: %s", configDir)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	// Sanity check: ResolveLogDirectory must yield the relative "logs" here,
	// otherwise the test is not exercising the mismatch scenario.
	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                filepath.Join(configDir, "auth"),
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	if got := internallogging.ResolveLogDirectory(cfg); got != "logs" {
		t.Fatalf("ResolveLogDirectory = %q, want relative %q (test precondition)", got, "logs")
	}

	// The writer target directory for a relative "logs" resolved against the
	// config file directory.
	writerLogDir := filepath.Join(configDir, "logs")
	if err := os.MkdirAll(writerLogDir, 0o755); err != nil {
		t.Fatalf("failed to create writer log dir: %v", err)
	}
	const requestID = "abc123"
	logFileName := "2026-07-16_120000-" + requestID + ".log"
	logContent := []byte("request log body for " + requestID)
	if err := os.WriteFile(filepath.Join(writerLogDir, logFileName), logContent, 0o644); err != nil {
		t.Fatalf("failed to write request log file: %v", err)
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	server := NewServer(cfg, authManager, accessManager, configPath)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/request-log-by-id/"+requestID, nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("request-log-by-id status = %d, want %d (reader must anchor to configDir like the writer) body=%s",
			rr.Code, http.StatusOK, rr.Body.String())
	}
	if body := rr.Body.Bytes(); string(body) != string(logContent) {
		t.Fatalf("request-log-by-id body = %q, want %q", string(body), string(logContent))
	}
}
