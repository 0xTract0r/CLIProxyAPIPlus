package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestUsageDeferredShutdownCannotOverwriteReplacement(t *testing.T) {
	const admin = "handoff-fixture-admin"
	const route = "/v0/management/usage/import"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	path := filepath.Join(dir, ".usage-statistics.json")
	if err := os.WriteFile(cfgPath, []byte("port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start := func(options ...ServerOption) *Server {
		cfg := &config.Config{AuthDir: dir}
		cfg.RemoteManagement.DisableControlPanel = true
		return NewServer(cfg, nil, nil, cfgPath, options...)
	}
	call := func(s *Server, stamp string) int {
		body := `{"version":2,"usage":{"apis":{"fixture":{"models":{"model":{"details":[{"timestamp":"` + stamp + `","tokens":{"input_tokens":1}}]}}}}}}`
		req := httptest.NewRequest("POST", route, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", admin)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	stop := func(s *Server) {
		t.Helper()
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	seed := start()
	if call(seed, "2026-11-20T12:00:00Z") != 200 {
		t.Fatal("seed import failed")
	}
	stop(seed)
	entered, release, complete := make(chan struct{}), make(chan struct{}), make(chan int, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	old := start(WithMiddleware(func(c *gin.Context) {
		if c.Request.URL.Path == route {
			close(entered)
			<-release
		}
	}))
	go func() { complete <- call(old, "2026-11-20T12:01:00Z") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not block")
	}
	stop(old)
	replacement := start()
	if code := call(replacement, "2026-11-20T12:02:00Z"); code != 200 {
		t.Fatalf("replacement import status %d", code)
	}
	stop(replacement)
	read := func() []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	_ = read()
	close(release)
	select {
	case code := <-complete:
		if code != 200 {
			t.Errorf("old request status %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not finish")
	}
	got := read()
	var restored struct{ Usage usage.StatisticsSnapshot }
	if err := json.Unmarshal(got, &restored); err != nil {
		t.Fatal(err)
	}
	rows := restored.Usage.APIs["fixture"].Models["model"].Details
	if len(rows) != 3 {
		t.Fatalf("expected seed and both imports, got %d", len(rows))
	}
	first, second := start(), start()
	for i, server := range []*Server{first, second, first, second} {
		stamp := fmt.Sprintf("2026-11-20T12:0%d:00Z", 3+i)
		if code := call(server, stamp); code != 200 {
			t.Fatalf("alternating import %d status %d", i, code)
		}
	}
	latest := start()
	rows = latest.UsageStatistics().Snapshot().APIs["fixture"].Models["model"].Details
	stamps := make(map[string]bool)
	for _, row := range rows {
		stamps[row.Timestamp.Format(time.RFC3339)] = true
	}
	if len(rows) != 7 || len(stamps) != 7 {
		t.Fatalf("successful imports surviving restart: %d, unique=%d", len(rows), len(stamps))
	}
	for i := range 7 {
		if !stamps[fmt.Sprintf("2026-11-20T12:0%d:00Z", i)] {
			t.Fatalf("confirmed import %d missing after restart", i)
		}
	}
	external := append(read(), '\n') // distinct but still valid JSON, no in-process commit
	if err := os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if code := call(first, "2026-11-20T12:09:00Z"); code != 409 || !bytes.Equal(read(), external) {
		t.Fatalf("valid external replacement import status %d or overwritten", code)
	}
	if err := os.WriteFile(path, []byte("externally damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := call(first, "2026-11-20T12:10:00Z"); code != 409 {
		t.Fatalf("external replacement import status %d", code)
	}
	if !bytes.Equal(read(), []byte("externally damaged")) {
		t.Fatal("unrecognized disk content was overwritten")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code := call(second, "2026-11-20T12:11:00Z"); code != 409 {
		t.Fatalf("external deletion import status %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("externally deleted snapshot recreated")
	}
}
