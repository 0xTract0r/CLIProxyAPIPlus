package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAuthFilesWarmupPacingReadOnlyProjection(t *testing.T) {
	now := time.Now()
	cfg := &config.Config{AuthDir: t.TempDir(), AccountScheduling: config.DefaultAccountSchedulingConfig()}
	cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
	s := coreauth.NewAdaptiveSelector(coreauth.AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling}, coreauth.WithAdaptiveClock(func() time.Time { return now }))
	t.Cleanup(s.Stop)
	m := coreauth.NewManager(nil, s, nil)
	m.SetConfig(cfg)
	t.Cleanup(func() { coreauth.RegisterAccountPacingUsageSink(nil) })
	a := &coreauth.Auth{
		ID: "private-account-id", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"quota_snapshot": map[string]any{"profile": map[string]any{"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"}}}},
	}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, authManager: m, managedHeaderScheduler: newManagedHeaderSyncScheduler()}
	// Isolate this projection from the existing, unrelated background header sync.
	h.managedHeaderScheduler.recordSuccess(a.ID, time.Now().Add(time.Hour))
	router := gin.New()
	router.GET("/v0/management/auth-files", h.ListAuthFiles)
	paths, err := filepath.Glob(filepath.Join(cfg.AuthDir, "*.pacing"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("ledger fixture missing: %v %v", paths, err)
	}
	before, _ := os.ReadFile(paths[0])
	info, _ := os.Stat(paths[0])
	now = now.Add(432 * time.Second)
	for range 3 {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		var result struct {
			Files []struct {
				Scheduling map[string]json.RawMessage `json:"account_scheduling"`
			} `json:"files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Files) != 1 {
			t.Fatal("invalid list", err, w.Body.String())
		}
		encoded := result.Files[0].Scheduling["warmup_traffic_pacing"]
		var got coreauth.WarmupTrafficPacingSnapshot
		if err := json.Unmarshal(encoded, &got); err != nil || got.Status != "active" || *got.RequestBalance != 1 || got.ObservedAt != now.UTC().Format(time.RFC3339) {
			t.Fatalf("invalid pacing projection: %s (%v)", encoded, err)
		}
		for _, sensitive := range []string{a.ID, cfg.AuthDir, "proxy_url", "group_id", "token"} {
			if strings.Contains(string(encoded), sensitive) {
				t.Fatalf("pacing projection leaked %q", sensitive)
			}
		}
	}
	after, _ := os.ReadFile(paths[0])
	afterInfo, _ := os.Stat(paths[0])
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("list projection rewrote ledger")
	}
}

func TestAuthFilesWarmupPacingNullableStates(t *testing.T) {
	a := &coreauth.Auth{ID: "account", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"runtime_only": "true"}}
	for _, enabled := range []bool{false, true} {
		cfg := &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}
		cfg.AccountScheduling.WarmupTrafficPacing.Enabled = enabled
		h := &Handler{cfg: cfg}
		view := h.buildAuthFileEntry(a.Clone())["account_scheduling"].(gin.H)
		encoded, err := json.Marshal(view["warmup_traffic_pacing"])
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		want := "disabled"
		if enabled {
			want = "uninitialized"
		}
		if got["status"] != want || got["observed_at"] == "" {
			t.Fatal("incorrect status", string(encoded))
		}
		for _, field := range []string{"request_balance", "request_capacity", "min_admission_requests", "refill_per_hour", "admission_balance_eta_seconds", "rolling_24h_requests", "daily_request_budget", "rolling_60s_requests", "rpm_limit", "active_binding_groups", "max_active_binding_groups", "active_binding_idle_seconds", "inflight", "concurrency_limit", "pending_requests"} {
			if value, ok := got[field]; !ok || value != nil {
				t.Fatalf("unknown %s must be explicit null: %s", field, encoded)
			}
		}
		if blocks, ok := got["blocking_reasons"].([]any); !ok || len(blocks) != 0 {
			t.Fatal("inactive blockers must be an empty array", string(encoded))
		}
	}
	a.Provider = "codex"
	if _, ok := (&Handler{}).buildAuthFileEntry(a)["account_scheduling"]; ok {
		t.Fatal("pacing added a Claude scheduling view to another provider")
	}
}
