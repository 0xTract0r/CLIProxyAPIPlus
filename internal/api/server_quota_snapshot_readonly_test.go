package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestStoredQuotaSnapshotsRegisteredReadOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	const private = "synthetic-private-sentinel"
	const managementKey = "synthetic-quota-management-key"
	fixtures := map[string]string{
		"codex.json": `{
			"type":"codex", "access_token":"` + private + `", "email":"` + private + `@example.invalid",
			"plan_type":"plus", "quota_refresh_status":"ok", "quota_last_refreshed_at":"2026-10-04T12:00:00Z",
			"quota_next_refresh_after":"2026-10-05T12:00:00Z",
			"quota_snapshot":{"usage":{
				"plan_type":"plus", "user_id":"` + private + `", "account_id":"` + private + `",
				"rate_limit":{"allowed":true,"primary_window":{"used_percent":42,"reset_at":1800000000,"token":"` + private + `"}},
				"additionalRateLimits":[{"limitName":"Code Review","rateLimit":{"secondaryWindow":{"usedPercent":"11","resetAfterSeconds":"60"}},"private_url":"https://` + private + `.example.invalid"},
					{"meteredFeature":"` + private + `","rateLimit":{"primaryWindow":{"usedPercent":7}}}],
				"credits":{"has_credits":true,"unlimited":false,"balance":"3.5","access_token":"` + private + `"},
				"rate_limit_reset_credits":{"available_count":0,"credit_ids":["` + private + `"]},
				"subscription_active_until":"2026-11-01T00:00:00Z",
				"promo":{"token":"` + private + `"}
			}}
		}`,
		"claude.json": `{
			"type":"claude", "label":"` + private + `", "disabled":true, "quota_refresh_status":"error",
			"quota_refresh_error":"https://example.invalid/?token=` + private + `",
			"quota_last_refreshed_at":"0001-01-01T00:00:00Z",
			"quota_snapshot":{
				"profile":{
					"account":{"has_claude_max":false,"has_claude_pro":false,"uuid":"` + private + `","email":"` + private + `"},
					"organization":{"organization_type":"claude_team","subscription_status":"active","uuid":"` + private + `","name":"` + private + `"}
				},
				"usage":{
					"five_hour":{"utilization":55,"resets_at":"2026-10-06T00:00:00Z","private_url":"https://` + private + `.example.invalid"},
					"seven_day":{"utilization":"` + private + `"},
					"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1200,"utilization":24}
				}
			}
		}`,
		"other.json": `{"type":"openai","quota_refresh_status":"` + private + `"}`,
	}
	for file, body := range fixtures {
		if err := os.WriteFile(filepath.Join(authDir, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != len(fixtures) {
		t.Fatalf("cold-load entries=%d", len(manager.List()))
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("remote-management: {secret-key: "+managementKey+"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = authDir
	cfg.Plugins.Dir = filepath.Join(dir, "plugins")
	server := NewServer(cfg, manager, nil, path, WithRequestLoggerFactory(func(*config.Config, string) logging.RequestLogger { return nil }))
	do := func(method, path, body string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:35009"
		req.Header.Set("Content-Type", "application/json")
		if authorized {
			req.Header.Set("Authorization", "Bearer "+managementKey)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	const endpoint = "/v0/management/quota/snapshots"
	if rec := do(http.MethodGet, endpoint, "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", rec.Code)
	}
	if rec := do(http.MethodPost, "/v0/management/quota/refresh", "{}", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unsupported refresh route status=%d", rec.Code)
	}
	rec := do(http.MethodGet, endpoint, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), private) {
		t.Fatal("private value was included in the quota response")
	}
	var response struct {
		GeneratedAt string           `json:"generated_at"`
		Policy      map[string]any   `json:"policy"`
		Entries     []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.GeneratedAt == "" || !reflect.DeepEqual(response.Policy, map[string]any{"enabled": false}) || len(response.Entries) != 2 {
		t.Fatalf("incompatible response shape: %+v", response)
	}

	claude, codex := response.Entries[0], response.Entries[1]
	if claude["provider"] != "claude" || claude["name"] != "claude.json" || claude["auth_id"] != "claude.json" || claude["auth_index"] == nil ||
		claude["disabled"] != true || claude["status"] != "error" || claude["error"] != "Quota snapshot refresh failed" || claude["last_refreshed_at"] != nil {
		t.Fatalf("wrong Claude row: %v", claude)
	}
	if codex["provider"] != "codex" || codex["name"] != "codex.json" || codex["auth_id"] != "codex.json" || codex["auth_index"] == nil ||
		codex["plan_type"] != "plus" || codex["status"] != "ok" ||
		codex["last_refreshed_at"] != "2026-10-04T12:00:00Z" || codex["next_refresh_at"] != "2026-10-05T12:00:00Z" {
		t.Fatalf("wrong Codex row: %v", codex)
	}
	const claudeDisplay = `{
		"profile":{"account":{"has_claude_max":false,"has_claude_pro":false},"organization":{"organization_type":"claude_team","subscription_status":"active"}},
		"usage":{"five_hour":{"utilization":55,"resets_at":"2026-10-06T00:00:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1200,"utilization":24}}
	}`
	const codexDisplay = `{
		"usage":{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":42,"reset_at":1800000000}},
			"additionalRateLimits":[{"limitName":"Code Review","rateLimit":{"secondaryWindow":{"usedPercent":"11","resetAfterSeconds":"60"}}},{"rateLimit":{"primaryWindow":{"usedPercent":7}}}],
			"credits":{"has_credits":true,"unlimited":false,"balance":"3.5"},"rate_limit_reset_credits":{"available_count":0},
			"subscription_active_until":"2026-11-01T00:00:00Z"}
	}`
	for _, pair := range []struct {
		actual any
		expect string
	}{{claude["snapshot"], claudeDisplay}, {codex["snapshot"], codexDisplay}} {
		var expected any
		if err := json.Unmarshal([]byte(pair.expect), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(pair.actual, expected) {
			t.Fatalf("wrong display projection actual=%v expected=%v", pair.actual, expected)
		}
	}
	for file, original := range fixtures {
		stored, err := os.ReadFile(filepath.Join(authDir, file))
		if err != nil || string(stored) != original {
			t.Fatalf("%s: read-only endpoint modified local data: %v", file, err)
		}
	}
}
