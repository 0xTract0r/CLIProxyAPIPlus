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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestUsageInstanceColdStart(t *testing.T) {
	const admin = "synthetic-usage-admin"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	const base = "/v0/management/usage"
	const pricing = base + "/pricing"
	const key = pricing + "/overrides/fixture-model"
	const importBody = `{"version":2,"usage":{"apis":{"fixture":{"models":{"fixture-model":{"details":[{"timestamp":"2026-11-20T12:00:00Z","tokens":{"input_tokens":10}}]}}}}}}`
	start := func(dir string, opts ...ServerOption) *Server {
		t.Helper()
		cfg := &config.Config{AuthDir: dir}
		cfg.RemoteManagement.DisableControlPanel = true
		path := ""
		if dir != "" {
			path = filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return NewServer(cfg, nil, nil, path, opts...)
	}
	call := func(s *Server, method, url, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", admin)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != want {
			t.Errorf("%s %s=%d want %d: %s", method, url, res.Code, want, res.Body)
		}
		return res.Body.Bytes()
	}
	check := func(s *Server, requests int64, price float64) {
		t.Helper()
		var u struct{ Usage usage.StatisticsSnapshot }
		if err := json.Unmarshal(call(s, "GET", base+"?include_details=false", "", 200), &u); err != nil || u.Usage.TotalRequests != requests {
			t.Fatalf("requests=%d want %d err=%v", u.Usage.TotalRequests, requests, err)
		}
		var p struct{ Pricing usage.PricingSnapshot }
		if err := json.Unmarshal(call(s, "GET", pricing, "", 200), &p); err != nil || p.Pricing.Overrides[usage.NormalizeCanonicalModelID("fixture-model")].InputUSDPerMTok != price {
			t.Fatalf("unexpected pricing err=%v", err)
		}
	}
	stop := func(s *Server) {
		t.Helper()
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	aDir, bDir := t.TempDir(), t.TempDir()
	a, b := start(aDir), start(bDir)
	if a.UsageStatistics() == b.UsageStatistics() {
		t.Fatal("instances share a usage store")
	}
	call(a, "POST", base+"/import", importBody, 200)
	call(a, "PUT", key, `{"input_usd_per_mtok":2,"output_usd_per_mtok":3}`, 200)
	call(b, "PUT", key, `{"input_usd_per_mtok":7,"output_usd_per_mtok":8}`, 200)
	check(a, 1, 2)
	check(b, 0, 7)
	call(a, "GET", base+"/export", "", 200)
	call(a, "GET", base+"-queue?count=bad", "", 400)
	stop(a)
	stop(b)
	a, b = start(aDir), start(bDir)
	check(a, 1, 2)
	check(b, 0, 7)
	stop(a)
	stop(b)

	readonly := []byte("{invalid")
	for _, tc := range []struct {
		dir, name string
		history   int64
		price     float64
	}{
		{aDir, ".usage-pricing-catalog.json", 1, 0},
		{bDir, ".usage-statistics.json", 0, 7},
	} {
		path := filepath.Join(tc.dir, tc.name)
		if err := os.WriteFile(path, readonly, 0600); err != nil {
			t.Fatal(err)
		}
		s := start(tc.dir)
		check(s, tc.history, tc.price)
		wantImport := 200
		if tc.name == ".usage-statistics.json" {
			wantImport = 409
		}
		call(s, "POST", base+"/import", importBody, wantImport)
		call(s, "PUT", key, `{"input_usd_per_mtok":9,"output_usd_per_mtok":10}`, 200)
		stop(s)
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, readonly) {
			t.Fatal("corrupt original was overwritten")
		}
	}

	// Immediate shutdown must leave an in-flight request to complete before the final flush.
	dir := t.TempDir()
	entered, released, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pending := start(dir, WithMiddleware(func(c *gin.Context) {
		if c.Request.URL.Path == base+"/import" {
			close(entered)
			<-released
		}
	}))
	go func() { defer close(done); call(pending, "POST", base+"/import", importBody, 200) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("import did not enter middleware")
	}
	stop(pending)
	if _, err := os.Stat(filepath.Join(dir, ".usage-statistics.json")); !os.IsNotExist(err) {
		t.Fatal("history flushed before the active import")
	}
	close(released)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("import did not complete")
	}
	restarted := start(dir)
	check(restarted, 1, 0)
	stop(restarted)

	memory := start("")
	check(memory, 0, 0)
	stop(memory)
}
