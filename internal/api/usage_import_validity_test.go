package api

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageImportValidityRegisteredRoutes(t *testing.T) {
	const password = "fixture-import-admin"
	const route = "/v0/management/usage/import"
	t.Setenv("MANAGEMENT_PASSWORD", password)
	const model = "usage-validity-fixture"
	cfg := &config.Config{UsageStatisticsEnabled: true}
	cfg.RemoteManagement.DisableControlPanel = true
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	server := NewServer(cfg, nil, nil, configPath)
	stats := bindUsageValidityFixture(t, server, configPath)
	stamp := time.Unix(1700000000, 0).UTC()
	post := func(payload any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", password)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		return res
	}
	importBody := func(version int, rows ...usage.RequestDetail) map[string]any {
		snapshot := usage.StatisticsSnapshot{APIs: map[string]usage.APISnapshot{
			"fixture": {Models: map[string]usage.ModelSnapshot{model: {Details: rows}}},
		}}
		return map[string]any{"version": version, "usage": snapshot}
	}
	detail := func(second int, tokens usage.TokenStats) usage.RequestDetail {
		return usage.RequestDetail{Timestamp: stamp.Add(time.Duration(second) * time.Second), Tokens: tokens}
	}
	stats.Record(t.Context(), coreusage.Record{APIKey: "fixture", Model: model, RequestedAt: stamp, Detail: coreusage.Detail{TotalTokens: 4}})
	for _, bad := range []usage.TokenStats{
		{TotalTokens: -1},
		{TotalTokens: 2, CachedTokens: -1},
		{InputTokens: math.MaxInt64, OutputTokens: 1},
		{TotalTokens: math.MaxInt64, CacheWriteTokens: 1},
		{TotalTokens: math.MaxInt64},
		{TotalTokens: 2, BillableTokens: math.MaxInt64},
		{TotalTokens: 2, BillableTokens: -1},
	} {
		res := post(importBody(2, detail(1, usage.TokenStats{InputTokens: 3}), detail(2, bad)))
		if res.Code != http.StatusBadRequest {
			t.Errorf("mixed batch returned %d, want 400", res.Code)
		}
		if snap := stats.Snapshot(); snap.TotalRequests != 1 || snap.TotalTokens != 4 {
			t.Errorf("rejected batch changed history: requests=%d tokens=%d", snap.TotalRequests, snap.TotalTokens)
		}
	}
	for version := range 3 {
		payload := importBody(version, detail(10+version, usage.TokenStats{InputTokens: 2}))
		if version == 2 {
			donor := usage.NewRequestStatistics()
			for i := range 2 {
				donor.Record(t.Context(), coreusage.Record{APIKey: "fixture", Model: model,
					RequestedAt: stamp.Add(time.Duration(12+i) * time.Second), Detail: coreusage.Detail{TotalTokens: 2}})
			}
			page := donor.SnapshotPageWithOptions(usage.SnapshotOptions{DetailLimit: 1})
			if !page.HasMore || page.Snapshot.TotalRequests != 2 {
				t.Fatal("expected real usage pagination fixture")
			}
			payload = map[string]any{"version": 2, "usage": page.Snapshot,
				"has_more": page.HasMore, "next_since": page.NextSince.Format(time.RFC3339Nano)}
		}
		if res := post(payload); res.Code != http.StatusOK {
			t.Fatalf("valid v%d import returned %d: %s", version, res.Code, res.Body.String())
		}
	}
	if res := post(map[string]any{"version": 2, "usage": stats.SnapshotWithOptions(usage.SnapshotOptions{ExcludeDetails: true})}); res.Code != http.StatusOK {
		t.Fatalf("summary-only HTTP import returned %d", res.Code)
	}
	stopUsageValidityFixture(t, server, stats)
	restarted := NewServer(cfg, nil, nil, configPath)
	history := bindUsageValidityFixture(t, restarted, configPath)
	if got := history.Snapshot(); got.TotalRequests != 4 || got.TotalTokens != 10 {
		t.Fatalf("restart did not recover recorded+imported history: %d requests, %d tokens", got.TotalRequests, got.TotalTokens)
	}
	stopUsageValidityFixture(t, restarted, history)
}

func bindUsageValidityFixture(_ *testing.T, server *Server, _ string) *usage.RequestStatistics {
	return server.UsageStatistics()
}

func stopUsageValidityFixture(t *testing.T, server *Server, _ *usage.RequestStatistics) {
	t.Helper()
	if err := server.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}
