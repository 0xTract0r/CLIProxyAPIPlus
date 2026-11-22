package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageImportWriteFailureLeavesCommittedHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root filesystem permissions")
	}
	t.Setenv("MANAGEMENT_PASSWORD", "fixture-import-tx")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: dir, UsageStatisticsEnabled: true}
	cfg.RemoteManagement.DisableControlPanel = true
	s := NewServer(cfg, nil, nil, cfgPath)
	call := func(s *Server, method, path, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, "/v0/management/usage"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", "fixture-import-tx")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s got %d want %d", method, path, rec.Code, want)
		}
		return rec.Body.Bytes()
	}
	body := func(model string) string {
		return `{"version":2,"usage":{"apis":{"fixture":{"models":{"` + model + `":{"details":[{"timestamp":"2026-11-20T12:00:00Z","tokens":{"input_tokens":1}}]}}}}}}`
	}
	call(s, "POST", "/import", body("seed"), 200)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.UsageStatistics().Record(context.Background(), sdkusage.Record{Provider: "fixture", Model: "live", RequestedAt: time.Now(), Detail: sdkusage.Detail{InputTokens: 2, TotalTokens: 2}})
	}()
	call(s, "POST", "/import", body("must-not-persist"), 500)
	<-done
	inspect := func(s *Server, want int64) {
		t.Helper()
		b := call(s, "GET", "", "", 200)
		var payload struct{ Usage usage.StatisticsSnapshot }
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Usage.TotalRequests != want || bytes.Contains(b, []byte("must-not-persist")) {
			t.Errorf("unexpected published history count=%d want %d", payload.Usage.TotalRequests, want)
		}
	}
	inspect(s, 2)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := NewServer(cfg, nil, nil, cfgPath)
	inspect(reopened, 2)
	if err := reopened.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
