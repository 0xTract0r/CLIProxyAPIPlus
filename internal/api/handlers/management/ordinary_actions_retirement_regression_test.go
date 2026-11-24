package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryManualRefreshHistoricalStateAssertions(t *testing.T) {
	for _, scenario := range []string{"clear-stale-warning", "keep-usable-grant", "keep-valid-access-on-rejected-refresh-grant", "persist-terminal-failure", "late-failure-after-grant-replacement", "keep-quarantine"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			const name = "ordinary-retirement-file.json"
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			expiry := time.Now().Add(time.Hour)
			if scenario == "persist-terminal-failure" {
				expiry = time.Now().Add(-time.Hour)
			}
			records := map[string]any{
				"type": "codex", "refresh_token": "synthetic-grant-A", "access_token": "synthetic-access-A",
				"expired": expiry.UTC().Format(time.RFC3339), "note": "keep existing note",
			}
			if scenario == "keep-quarantine" {
				records["auto_quarantined"] = true
				records["quarantine_reason"] = "terminal_auth_failure"
			}
			raw, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			if err = manager.Load(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario == "clear-stale-warning" {
				current, _ := manager.GetByID(name)
				current.Status, current.StatusMessage = coreauth.StatusError, "synthetic previous transient warning"
				if _, err = manager.Update(coreauth.WithSkipPersist(ctx), current); err != nil {
					t.Fatal(err)
				}
			}
			refreshCalls := 0
			manager.RegisterExecutor(&manualStatusTestExecutor{refresh: func(previous *coreauth.Auth) (*coreauth.Auth, error) {
				refreshCalls++
				switch scenario {
				case "keep-usable-grant":
					return nil, manualStatusWaitError{}
				case "keep-valid-access-on-rejected-refresh-grant":
					return nil, &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "refresh_token_reused", Message: "refresh_token_reused"}
				case "persist-terminal-failure":
					return nil, &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "invalid_grant", Message: "invalid_grant"}
				case "late-failure-after-grant-replacement":
					records["refresh_token"] = "synthetic-grant-replacement"
					raw, err := json.Marshal(records)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err = manager.Load(ctx); err != nil {
						t.Fatal(err)
					}
					return nil, &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "invalid_grant", Message: "invalid_grant"}
				}
				baseline := previous.Clone().Metadata
				previous.Metadata["access_token"] = "synthetic-rotated-access"
				previous.Metadata["refresh_token"] = "synthetic-rotated-grant"
				previous.CodexRefreshPatch = codex.NewCredentialRefreshPatchWithDiskBaseline(baseline, baseline, previous.Metadata, true, false)
				previous.CodexRefreshPatch.SetUpstreamRefreshed(true)
				return previous, nil
			}})
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
			h.tokenStore = store
			router := gin.New()
			router.POST("/auth-files/refresh-status", h.RefreshOrdinaryAuthFileStatus)
			response := callManualAction(router, "/auth-files/refresh-status", `{"name":"ordinary-retirement-file.json","trigger":"manual"}`)

			cold := coreauth.NewManager(store, nil, nil)
			if err = cold.Load(ctx); err != nil {
				t.Fatal(err)
			}
			current, _ := manager.GetByID(name)
			persisted, _ := cold.GetByID(name)
			if current == nil || persisted == nil || persisted.Metadata["note"] != "keep existing note" {
				t.Fatal("ordinary file fields lost during refresh")
			}
			body := response.Body.String()
			if strings.Contains(body, "synthetic-grant") || strings.Contains(body, "synthetic-access") {
				t.Fatal("refresh response exposed a credential")
			}
			switch scenario {
			case "clear-stale-warning":
				if response.Code != 200 || refreshCalls != 1 || current.Status != coreauth.StatusActive ||
					current.StatusMessage != "" || persisted.Metadata["refresh_token"] != "synthetic-rotated-grant" {
					t.Fatalf("stale warning or grant not refreshed: code=%d calls=%d status=%s", response.Code, refreshCalls, current.Status)
				}
			case "keep-usable-grant":
				if response.Code != 429 || refreshCalls != 1 || current.Unavailable || current.Status == coreauth.StatusDisabled ||
					persisted.Metadata["access_token"] != "synthetic-access-A" || response.Header().Get("Retry-After") != "3600" {
					t.Fatalf("transient refresh failure damaged usable grant: code=%d calls=%d status=%s", response.Code, refreshCalls, current.Status)
				}
			case "keep-valid-access-on-rejected-refresh-grant":
				if response.Code != 401 || refreshCalls != 1 || current.Unavailable || !current.HasValidAccessToken(time.Now()) ||
					persisted.Metadata["access_token"] != "synthetic-access-A" || coreauth.IsReauthRequiredMetadata(persisted.Metadata) {
					t.Fatalf("rejected refresh grant damaged a still-valid access token: code=%d calls=%d", response.Code, refreshCalls)
				}
			case "persist-terminal-failure":
				if response.Code != 401 || refreshCalls != 1 || !coreauth.IsReauthRequiredMetadata(persisted.Metadata) ||
					!strings.Contains(body, `"credential_error":true`) {
					t.Fatalf("terminal refusal did not retain cold reauth: code=%d calls=%d", response.Code, refreshCalls)
				}
			case "late-failure-after-grant-replacement":
				if response.Code != 409 || refreshCalls != 1 || coreauth.IsReauthRequiredMetadata(persisted.Metadata) ||
					persisted.Metadata["refresh_token"] != "synthetic-grant-replacement" {
					t.Fatalf("late failure changed the replacement: code=%d calls=%d", response.Code, refreshCalls)
				}
			case "keep-quarantine":
				if response.Code != 200 || refreshCalls != 0 || !current.AutoQuarantined || !persisted.AutoQuarantined ||
					!strings.Contains(body, `"refreshed":false`) {
					t.Fatalf("quarantine was not read-only: code=%d calls=%d", response.Code, refreshCalls)
				}
			}
		})
	}
}

func TestOrdinaryManualMessageRequiresExplicitModelAndKeepsHTTPDate(t *testing.T) {
	dir := t.TempDir()
	const name = "anthropic-fixture.json"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"type":"claude","api_key":"synthetic-claude-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.tokenStore = store
	models := registry.GetGlobalRegistry()
	const model = "synthetic-registered-model"
	models.RegisterClient(name, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { models.UnregisterClient(name) })
	calls := 0
	retry := time.Now().Add(4 * time.Minute).UTC().Format(http.TimeFormat)
	transport := manualActionRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://api.anthropic.com/v1/messages" || req.Header.Get("X-Api-Key") != "synthetic-claude-key" ||
			req.Header.Get("Authorization") != "" || req.Header.Get("Anthropic-Version") == "" {
			t.Fatal("credential was not bound to the public Anthropic Messages protocol")
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {retry}},
			Body: io.NopCloser(strings.NewReader("private response body should never be copied")),
		}, nil
	})
	router := gin.New()
	const endpoint = "/auth-files/test-message"
	router.POST(endpoint, func(c *gin.Context) { h.testOrdinaryAuthFileMessage(c, transport) })
	if response := callManualAction(router, endpoint, `{"name":"anthropic-fixture.json"}`); response.Code != 400 || calls != 0 {
		t.Fatal("missing model triggered an implicit probe")
	}
	response := callManualAction(router, endpoint, `{"name":"anthropic-fixture.json","model":"synthetic-registered-model"}`)
	if response.Code != 429 || calls != 1 || response.Header().Get("Retry-After") != retry || strings.Contains(response.Body.String(), "private response body") {
		t.Fatal("public provider's HTTP date or private error isolation was lost")
	}
}

func TestOrdinaryQuotaReadbackKeepsKnownPlanAndSanitizedFailure(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "quota-synthetic", Provider: "codex", FileName: "quota-synthetic.json",
		Metadata: map[string]any{
			"plan_type": "PLUS", "quota_refresh_status": "reauth_required",
			"quota_refresh_error":     "synthetic raw text with a private fixture value",
			"quota_last_refreshed_at": "2026-11-12T03:12:00Z",
			"quota_snapshot": map[string]any{"usage": map[string]any{"plan_type": "PLUS",
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 31.0}}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/quota/snapshots", nil)
	h.GetStoredQuotaSnapshots(c)
	var payload struct{ Entries []storedQuotaEntry }
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || rec.Code != http.StatusOK || len(payload.Entries) != 1 {
		t.Fatal("saved snapshot unreadable")
	}
	entry := payload.Entries[0]
	if entry.PlanType != "plus" || entry.Status != "reauth_required" || entry.Error != "Credential requires reauthentication" ||
		entry.LastRefreshedAt == nil || entry.LastRefreshedAt.Format(time.RFC3339) != "2026-11-12T03:12:00Z" ||
		!strings.Contains(rec.Body.String(), `"used_percent":31`) || strings.Contains(rec.Body.String(), "private fixture") {
		t.Fatal("cold display lost the last-known plan/time/window or exposed the raw provider error")
	}
}
