package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestUsageKnownKeyConfigurationCommit(t *testing.T) {
	const admin = "synthetic-key-reload-admin"
	const key = "synthetic-inbound-reload-key"
	const base = "/v0/management"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: dir}
	cfg.RemoteManagement.DisableControlPanel = true
	s := NewServer(cfg, nil, nil, path, WithIsolatedUsage())
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	call := func(method, route, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, base+route, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("X-Management-Key", admin)
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, req)
		if r.Code != want {
			t.Fatalf("%s %s=%d want %d", method, route, r.Code, want)
		}
		return r.Body.Bytes()
	}
	body := `{"version":2,"usage":{"apis":{"` + key + `":{"models":{"fixture":{"details":[{"timestamp":"2026-11-21T05:00:00Z","request_id":"fixture-a","tokens":{"input_tokens":2}}]}}}}}}`
	call("POST", "/usage/import", body, 200)
	call("PUT", "/api-keys", `["`+key+`"]`, 200)
	stable := func(requests int64) {
		t.Helper()
		data := call("GET", "/usage", "", 200)
		if bytes.Contains(data, []byte(key)) {
			t.Error("known old key exposed")
		}
		var payload struct{ Usage usage.StatisticsSnapshot }
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		bucket := fmt.Sprintf("h:%x", sha256.Sum256([]byte(key)))
		if payload.Usage.TotalRequests != requests || len(payload.Usage.APIs) != 1 || len(payload.Usage.APIs[bucket].Models["fixture"].Details) != int(requests) {
			t.Errorf("canonical requests=%d want %d (buckets=%d)", payload.Usage.TotalRequests, requests, len(payload.Usage.APIs))
		}
		if bytes.Contains(call("GET", "/usage/export", "", 200), []byte(key)) {
			t.Error("known old key exposed in export")
		}
	}
	stable(1)
	call("PUT", "/api-keys", `[]`, 200)
	call("POST", "/usage/import", body, 200)
	stable(1)
	call("POST", "/usage/import", strings.ReplaceAll(body, "fixture-a", "fixture-b"), 200)
	stable(2)
}
