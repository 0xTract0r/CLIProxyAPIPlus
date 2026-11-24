package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageDefaultAndExplicitIsolatedStores(t *testing.T) {
	const key = "synthetic-usage-admin"
	t.Setenv("MANAGEMENT_PASSWORD", key)
	global := usage.GetRequestStatistics()
	previous := usage.StatisticsEnabled()
	t.Cleanup(func() {
		if err := global.SetPersistencePath(""); err != nil {
			t.Error(err)
		}
		usage.SetStatisticsEnabled(previous)
	})
	path := filepath.Join(t.TempDir(), ".usage-statistics.json")
	if err := usage.ConfigureDefaultPersistence(path); err != nil {
		t.Fatal(err)
	}
	start := func(enabled, isolated bool) *Server {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{AuthDir: dir, UsageStatisticsEnabled: enabled}
		cfg.RemoteManagement.DisableControlPanel = true
		opts := []ServerOption{
			WithRouterConfigurator(func(engine *gin.Engine, base *handlers.BaseAPIHandler, _ *config.Config) {
				engine.POST("/synthetic-usage", func(c *gin.Context) {
					ctx, cancel := base.GetContextWithCancel(nil, c, c.Request.Context())
					defer cancel()
					coreusage.PublishRecord(ctx, coreusage.Record{
						Provider: "fixture", Model: c.Query("model"), APIKey: "synthetic-inbound-key",
						Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
					})
					c.Status(http.StatusNoContent)
				})
			}),
		}
		if isolated {
			opts = append(opts, WithIsolatedUsage())
		}
		return NewServer(cfg, nil, nil, filepath.Join(dir, "config.yaml"), opts...)
	}
	request := func(s *Server, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "127.0.0.1:24182"
		r.Header.Set("X-Management-Key", key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	defaultServer := start(true, false)
	isolated := start(true, true)
	disabled := start(false, true)
	for _, s := range []*Server{defaultServer, isolated, disabled} {
		t.Cleanup(func() { _ = s.Stop(context.Background()) })
	}
	if defaultServer.UsageStatistics() != global || isolated.UsageStatistics() == global ||
		disabled.UsageStatistics() == global || isolated.UsageStatistics() == disabled.UsageStatistics() {
		t.Fatal("unexpected recorder ownership")
	}
	const globalModel = "synthetic-global-only"
	const isolatedModel = "synthetic-instance-only"
	before := global.Snapshot().TotalRequests
	if !usage.StatisticsEnabled() {
		t.Fatal("isolated disabled instance changed the global enable state")
	}
	for _, v := range []struct {
		server *Server
		model  string
	}{{defaultServer, globalModel}, {isolated, isolatedModel}, {disabled, "synthetic-disabled"}} {
		if response := request(v.server, http.MethodPost, "/synthetic-usage?model="+v.model); response.Code != http.StatusNoContent {
			t.Fatalf("usage fixture route status=%d", response.Code)
		}
	}
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until) && global.Snapshot().TotalRequests == before; {
		time.Sleep(5 * time.Millisecond)
	}
	if global.Snapshot().TotalRequests != before+1 ||
		isolated.UsageStatistics().Snapshot().TotalRequests != 1 ||
		disabled.UsageStatistics().Snapshot().TotalRequests != 0 {
		t.Fatal("usage was not delivered to the selected stores")
	}
	var data []byte
	var err error
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		data, err = os.ReadFile(path)
		if err == nil && strings.Contains(string(data), globalModel) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(data), globalModel) || strings.Contains(string(data), isolatedModel) {
		t.Fatalf("default snapshot file contents incorrect: %v", err)
	}
	visible := request(isolated, http.MethodGet, "/v0/management/usage").Body.String()
	if !strings.Contains(visible, isolatedModel) || strings.Contains(visible, globalModel) {
		t.Fatal("management history used the wrong store")
	}
	for _, route := range []string{"/v0/management/usage-queue", "/v8/management/observability/usage/queue"} {
		rec := request(isolated, http.MethodGet, route)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("isolated queue returned shared records: status=%d", rec.Code)
		}
	}
	side, peer := net.Pipe()
	defer peer.Close()
	go isolated.handleRedisConnection(side, nil)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("isolated Redis connection was not closed: %v", err)
	}
	if err := isolated.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if request(isolated, http.MethodPost, "/synthetic-usage?model=too-late").Code != http.StatusServiceUnavailable {
		t.Fatal("stopped server still accepted an isolated event")
	}
	ctx := coreusage.WithRequestObserver(context.Background(), isolated.UsageStatistics())
	coreusage.PublishRecord(ctx, coreusage.Record{Provider: "fixture", Model: "late-callback"})
	if isolated.UsageStatistics().Snapshot().TotalRequests != 1 || global.Snapshot().TotalRequests != before+1 {
		t.Fatal("late scoped callback reached a store")
	}
}
