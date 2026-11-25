package api

import (
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	usage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageRecorderConfigurationAndShutdown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "review-fixture")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("usage-statistics-enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: dir, UsageStatisticsEnabled: true}
	cfg.RemoteManagement.DisableControlPanel = true
	s := NewServer(cfg, nil, nil, path, WithIsolatedUsage())
	ctx := usage.WithRequestObserver(context.Background(), s.UsageStatistics())
	emit := func() {
		usage.PublishRecord(ctx, usage.Record{Provider: "fixture", Model: "ordinary", APIKey: "synthetic-key", RequestedAt: time.Now(), Detail: usage.Detail{InputTokens: 1, TotalTokens: 1}})
	}
	call := func(method string) (int, bool) {
		r := httptest.NewRequest(method, "/v0/management/usage-statistics-enabled", strings.NewReader(`{"value":false}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Management-Key", "review-fixture")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var v map[string]bool
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v["usage-statistics-enabled"]
	}
	emit()
	if got := s.UsageStatistics().Snapshot().TotalRequests; got != 1 {
		t.Fatalf("fixture expected captured event; got %d", got)
	}
	t.Run("PUT-failed", func(t *testing.T) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		status, _ := call("PUT")
		_, visible := call("GET")
		if status != 500 || !visible {
			t.Errorf("failed PUT status=%d GET-enabled=%t", status, visible)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("usage-statistics-enabled: true\n"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("PUT-success", func(t *testing.T) {
		status, _ := call("PUT")
		_, visible := call("GET")
		n := s.UsageStatistics().Snapshot().TotalRequests
		emit()
		got := s.UsageStatistics().Snapshot().TotalRequests
		if status != 200 || visible || got != n {
			t.Errorf("success status=%d GET-enabled=%t recorder-increment=%d", status, visible, got-n)
		}
	})
	t.Run("late-after-stop", func(t *testing.T) {
		// Reset recording to the expected steady state before testing shutdown.
		s.UsageStatistics().SetRecordingEnabled(true)
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		before := s.UsageStatistics().Snapshot().TotalRequests
		emit()
		for until := time.Now().Add(time.Second); time.Now().Before(until); {
			data, _ := os.ReadFile(filepath.Join(dir, ".usage-statistics.json"))
			var f struct {
				Usage struct {
					Total int64 `json:"total_requests"`
				}
			}
			if json.Unmarshal(data, &f) == nil && f.Usage.Total > before {
				t.Errorf("post-stop event overwrote disk: before=%d after=%d", before, f.Usage.Total)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
