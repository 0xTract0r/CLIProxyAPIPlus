package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetAuthFileAccountSettings_SplitsLegacyHeadersIntoManagedAndExtra(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude.json",
		FileName: "claude.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude.json",
			"note": "legacy note",
		},
		Metadata: map[string]any{
			"type":      "claude",
			"proxy_url": "http://proxy.legacy",
			"headers": map[string]any{
				"User-Agent": "legacy-managed-ua/0.1",
				"X-Team":     "blue",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/3.0.0 (external, cli)",
			PackageVersion: "0.90.0",
			RuntimeVersion: "v30.0.0",
			Timeout:        "601",
		},
	}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccountSettings.ProxyURL != "http://proxy.legacy" {
		t.Fatalf("proxy_url = %q, want %q", resp.AccountSettings.ProxyURL, "http://proxy.legacy")
	}
	if resp.AccountSettings.Note != "legacy note" {
		t.Fatalf("note = %q, want %q", resp.AccountSettings.Note, "legacy note")
	}
	if got := resp.AccountSettings.ManagedHeaders["User-Agent"]; got != "claude-cli/3.0.0 (external, cli)" {
		t.Fatalf("managed User-Agent = %q, want %q", got, "claude-cli/3.0.0 (external, cli)")
	}
	if got := resp.AccountSettings.ExtraHeaders["X-Team"]; got != "blue" {
		t.Fatalf("extra X-Team = %q, want %q", got, "blue")
	}
	if _, ok := resp.AccountSettings.ExtraHeaders["User-Agent"]; ok {
		t.Fatalf("legacy managed User-Agent should not surface as extra_headers")
	}
}

func TestPatchAuthFileAccountSettings_RewritesRuntimeSnapshotAndStoredSchema(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/codex.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "managed-codex-ua/1.2.3",
			BetaFeatures: "feature-a,feature-b",
		},
	}, manager)

	body := `{"name":"codex.json","proxy_url":"http://proxy.remote","note":"remote account","disabled":true,"extra_headers":{"X-Team":"core"},"transport_profile":{"preset":"relay"},"tls_profile":{"preset":"future"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected updated auth record")
	}
	if !updated.Disabled {
		t.Fatalf("expected auth to be disabled")
	}
	if updated.ProxyURL != "http://proxy.remote" {
		t.Fatalf("proxy_url = %q, want %q", updated.ProxyURL, "http://proxy.remote")
	}

	accountSettingsRaw, ok := updated.Metadata["account_settings"]
	if !ok {
		t.Fatalf("expected metadata.account_settings to be present")
	}
	payload, _ := json.Marshal(accountSettingsRaw)
	if !strings.Contains(string(payload), `"schema_version":1`) {
		t.Fatalf("expected schema_version to be persisted, got %s", string(payload))
	}
	if !strings.Contains(string(payload), `"X-Team":"core"`) {
		t.Fatalf("expected extra_headers to be persisted, got %s", string(payload))
	}

	headersMeta, ok := updated.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.headers = %T, want map[string]any", updated.Metadata["headers"])
	}
	if got := headersMeta["User-Agent"]; got != "managed-codex-ua/1.2.3" {
		t.Fatalf("metadata.headers.User-Agent = %#v, want %q", got, "managed-codex-ua/1.2.3")
	}
	if got := headersMeta["Version"]; got != "1.2.3" {
		t.Fatalf("metadata.headers.Version = %#v, want %q", got, "1.2.3")
	}
	if got := headersMeta["Originator"]; got != "Codex Desktop" {
		t.Fatalf("metadata.headers.Originator = %#v, want %q", got, "Codex Desktop")
	}
	if got := headersMeta["X-Codex-Beta-Features"]; got != "feature-a,feature-b" {
		t.Fatalf("metadata.headers.X-Codex-Beta-Features = %#v, want %q", got, "feature-a,feature-b")
	}
	if got := headersMeta["X-Team"]; got != "core" {
		t.Fatalf("metadata.headers.X-Team = %#v, want %q", got, "core")
	}
	if got := updated.Attributes["header:User-Agent"]; got != "managed-codex-ua/1.2.3" {
		t.Fatalf("attrs header:User-Agent = %q, want %q", got, "managed-codex-ua/1.2.3")
	}
	if got := updated.Attributes["header:Version"]; got != "1.2.3" {
		t.Fatalf("attrs header:Version = %q, want %q", got, "1.2.3")
	}
	if got := updated.Attributes["header:X-Team"]; got != "core" {
		t.Fatalf("attrs header:X-Team = %q, want %q", got, "core")
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode patch response: %v", err)
	}
	if len(resp.AccountSettings.Warnings) != 2 {
		t.Fatalf("warnings = %#v, want 2 reservation warnings", resp.AccountSettings.Warnings)
	}
	if resp.AccountSettings.ProxyURL != "http://proxy.remote" {
		t.Fatalf("response proxy_url = %q, want %q", resp.AccountSettings.ProxyURL, "http://proxy.remote")
	}
	if got := resp.AccountSettings.ManagedHeaders["Version"]; got != "1.2.3" {
		t.Fatalf("response managed Version = %q, want %q", got, "1.2.3")
	}
	if got := resp.AccountSettings.ManagedHeaders["Originator"]; got != "Codex Desktop" {
		t.Fatalf("response managed Originator = %q, want %q", got, "Codex Desktop")
	}
	if resp.AccountSettings.ManagedHeaderState == nil || resp.AccountSettings.ManagedHeaderState.Current == nil {
		t.Fatalf("expected managed_header_state.current to be present")
	}
}

func TestPatchAuthFileAccountSettings_RejectsEmptyProxyURLForEnabledAccount(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "codex.json",
		FileName:   "codex.json",
		Provider:   "codex",
		ProxyURL:   "http://proxy.remote:8080",
		Attributes: map[string]string{"path": "/tmp/codex.json"},
		Metadata:   map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	// Enabled account with an empty proxy_url must be rejected to prevent IP exposure.
	body := `{"name":"codex.json","proxy_url":"","disabled":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for empty proxy_url, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	// The original proxy_url must remain untouched after the rejected patch.
	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to still exist")
	}
	if updated.ProxyURL != "http://proxy.remote:8080" {
		t.Fatalf("proxy_url = %q, want unchanged %q", updated.ProxyURL, "http://proxy.remote:8080")
	}
}

func TestPatchAuthFileAccountSettings_RejectsInvalidProxyURLForEnabledAccount(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "codex.json",
		FileName:   "codex.json",
		Provider:   "codex",
		ProxyURL:   "http://proxy.remote:8080",
		Attributes: map[string]string{"path": "/tmp/codex.json"},
		Metadata:   map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	// Malformed proxy_url (unsupported scheme) must be rejected.
	body := `{"name":"codex.json","proxy_url":"ftp://bad-scheme:1","disabled":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid proxy_url, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestPatchAuthFileAccountSettings_AllowsValidProxyURLForEnabledAccount(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "codex.json",
		FileName:   "codex.json",
		Provider:   "codex",
		Attributes: map[string]string{"path": "/tmp/codex.json"},
		Metadata:   map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	// Negative control: a valid proxy_url on an enabled account is accepted.
	body := `{"name":"codex.json","proxy_url":"socks5://proxy.remote:1080","disabled":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d for valid proxy_url, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected updated auth record")
	}
	if updated.ProxyURL != "socks5://proxy.remote:1080" {
		t.Fatalf("proxy_url = %q, want %q", updated.ProxyURL, "socks5://proxy.remote:1080")
	}
}

func TestPatchAuthFileAccountSettings_DisablesRefreshForAccessTokenOnlyRecords(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:               "codex-access-token-only.json",
		FileName:         "codex-access-token-only.json",
		Provider:         "codex",
		NextRefreshAfter: time.Now().Add(5 * time.Second),
		Attributes: map[string]string{
			"path": "/tmp/codex-access-token-only.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "access-token",
			"refresh_token": "must-not-be-used",
			"email":         "codex@example.test",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"codex-access-token-only.json","proxy_url":"http://test-proxy:8080","disabled":false,"refresh_enabled":false,"extra_headers":{}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-access-token-only.json")
	if !ok || updated == nil {
		t.Fatalf("expected updated auth record")
	}
	if !updated.RefreshDisabled() {
		t.Fatalf("expected refresh to be disabled")
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero", updated.NextRefreshAfter)
	}
	if got := updated.Metadata["refresh_token"]; got != "must-not-be-used" {
		t.Fatalf("refresh token should not be mutated by settings toggle, got %#v", got)
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode patch response: %v", err)
	}
	if resp.AccountSettings.RefreshEnabled {
		t.Fatalf("response refresh_enabled = true, want false")
	}
	if resp.AccountSettings.Activation.State != "refresh-disabled" {
		t.Fatalf("activation state = %q, want refresh-disabled", resp.AccountSettings.Activation.State)
	}

	refreshed, errRefresh := h.refreshAuthStatus(context.Background(), updated)
	if !errors.Is(errRefresh, errAuthRefreshDisabled) {
		t.Fatalf("refreshAuthStatus error = %v, want errAuthRefreshDisabled", errRefresh)
	}
	if refreshed == nil || !refreshed.RefreshDisabled() {
		t.Fatalf("refresh-disabled auth should remain disabled after manual refresh attempt")
	}
}

func TestGetAuthFileAccountSettings_PersistsCodexManagedHeaderHistoryAcrossVersionUpgrades(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	authName := "codex-history-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "-") + ".json"
	record := &coreauth.Auth{
		ID:       authName,
		FileName: authName,
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/" + authName,
		},
		Metadata: map[string]any{
			"type": "codex",
			"headers": map[string]any{
				"User-Agent":            "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)",
				"Version":               "0.118.0",
				"Originator":            "codex-tui",
				"X-Codex-Beta-Features": "feature-a",
			},
			"account_settings": map[string]any{
				"schema_version": 1,
				"managed_header_state": map[string]any{
					"policy_version": "codex-managed/v2",
					"current": map[string]any{
						"generated_at": "2026-04-24T10:00:00Z",
						"summary_headers": map[string]any{
							"User-Agent":            "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)",
							"Version":               "0.118.0",
							"Originator":            "codex-tui",
							"X-Codex-Beta-Features": "feature-a",
						},
						"versioned_capabilities": map[string]any{
							"User-Agent":            "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)",
							"Version":               "0.118.0",
							"X-Codex-Beta-Features": "feature-a",
						},
						"stable_identity": map[string]any{
							"Originator": "codex-tui",
						},
						"runtime_fingerprint": map[string]any{
							"platform": "Mac OS 26.3.1; arm64",
							"terminal": "iTerm.app/3.6.9 (codex-tui; 0.118.0)",
						},
					},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    runtimehelps.DefaultCodexManagedUserAgent(),
			BetaFeatures: "feature-a",
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	projectCodexVersion := func(version string, terminal string) {
		t.Helper()
		_ = runtimehelps.ResolveCodexClientProfile(record, http.Header{
			"User-Agent": []string{"codex-tui/" + version + " (Mac OS 27.0.0; arm64) " + terminal},
			"Version":    []string{version},
			"Originator": []string{"codex-tui"},
		}, cfg)
	}
	getAccountSettings := func() authFileAccountSettingsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name="+authName, nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
		}

		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		return resp
	}
	readStoredState := func() *authFileManagedHeaderState {
		t.Helper()
		updated, ok := manager.GetByID(authName)
		if !ok || updated == nil {
			t.Fatalf("expected updated auth record")
		}
		stored := readAccountSettingsMetadata(updated, cfg)
		if stored.ManagedHeaderState == nil {
			t.Fatalf("expected stored managed_header_state to be present")
		}
		return stored.ManagedHeaderState
	}

	projectCodexVersion("0.124.0", "Ghostty/1.0.0")
	firstResp := getAccountSettings()
	if got := firstResp.AccountSettings.ManagedHeaders["Version"]; got != "0.124.0" {
		t.Fatalf("managed Version = %q, want %q", got, "0.124.0")
	}
	if got := firstResp.AccountSettings.ManagedHeaders["User-Agent"]; !strings.Contains(got, "codex-tui/0.124.0") {
		t.Fatalf("managed User-Agent did not bump version marker: %q", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaders["User-Agent"]; strings.Contains(got, "Ghostty/1.0.0") {
		t.Fatalf("managed User-Agent unexpectedly changed stable terminal fingerprint: %q", got)
	}
	if firstResp.AccountSettings.ManagedHeaderState == nil {
		t.Fatalf("expected managed_header_state to be present")
	}
	if len(firstResp.AccountSettings.ManagedHeaderState.History) != 1 {
		t.Fatalf("history length = %d, want 1: %#v", len(firstResp.AccountSettings.ManagedHeaderState.History), firstResp.AccountSettings.ManagedHeaderState.History)
	}
	firstHistory := firstResp.AccountSettings.ManagedHeaderState.History[0]
	if got := firstHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "Version", "terminal"}) {
		t.Fatalf("first changed_fields = %#v, want only version markers", got)
	}
	if got := firstHistory.PreviousSummaryHeaders["User-Agent"]; got != "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)" {
		t.Fatalf("previous summary User-Agent = %q", got)
	}
	if got := firstHistory.NextSummaryHeaders["User-Agent"]; got != firstResp.AccountSettings.ManagedHeaders["User-Agent"] {
		t.Fatalf("next summary User-Agent = %q, want current managed User-Agent", got)
	}
	if got := firstHistory.PreviousVersionedCapabilities["Version"]; got != "0.118.0" {
		t.Fatalf("previous Version = %q, want %q", got, "0.118.0")
	}
	if got := firstHistory.NextVersionedCapabilities["Version"]; got != "0.124.0" {
		t.Fatalf("next Version = %q, want %q", got, "0.124.0")
	}
	if got := firstHistory.PreviousStableIdentity["Originator"]; got != "codex-tui" {
		t.Fatalf("previous stable Originator = %q, want codex-tui", got)
	}
	if got := firstHistory.NextStableIdentity["Originator"]; got != "codex-tui" {
		t.Fatalf("next stable Originator = %q, want codex-tui", got)
	}
	if got := firstHistory.PreviousRuntimeFingerprint["terminal"]; got != "iTerm.app/3.6.9 (codex-tui; 0.118.0)" {
		t.Fatalf("previous runtime terminal = %q", got)
	}
	if got := firstHistory.NextRuntimeFingerprint["terminal"]; got != "iTerm.app/3.6.9 (codex-tui; 0.124.0)" {
		t.Fatalf("next runtime terminal = %q", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["Originator"]; got != "codex-tui" {
		t.Fatalf("stable identity Originator = %q, want %q", got, "codex-tui")
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["platform"]; got != "Mac OS 26.3.1; arm64" {
		t.Fatalf("runtime fingerprint platform = %q, want pinned baseline", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; got != "iTerm.app/3.6.9 (codex-tui; 0.124.0)" {
		t.Fatalf("runtime fingerprint terminal = %q, want preserved terminal identity with bumped version", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; strings.Contains(got, "Ghostty/1.0.0") {
		t.Fatalf("runtime fingerprint terminal unexpectedly drifted to candidate terminal: %q", got)
	}
	firstStoredState := readStoredState()
	if len(firstStoredState.History) != 1 {
		t.Fatalf("stored history length = %d, want 1", len(firstStoredState.History))
	}

	projectCodexVersion("0.125.0", "Warp/2.0.0")
	secondResp := getAccountSettings()
	if secondResp.AccountSettings.ManagedHeaderState == nil {
		t.Fatalf("expected second managed_header_state to be present")
	}
	if len(secondResp.AccountSettings.ManagedHeaderState.History) != 2 {
		t.Fatalf("second history length = %d, want 2", len(secondResp.AccountSettings.ManagedHeaderState.History))
	}
	if !reflect.DeepEqual(secondResp.AccountSettings.ManagedHeaderState.History[0], firstHistory) {
		t.Fatalf("history should append only; first entry changed: %#v", secondResp.AccountSettings.ManagedHeaderState.History)
	}
	secondHistory := secondResp.AccountSettings.ManagedHeaderState.History[1]
	if got := secondHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "Version", "terminal"}) {
		t.Fatalf("second changed_fields = %#v, want only version markers", got)
	}
	if got := secondHistory.PreviousVersionedCapabilities["Version"]; got != "0.124.0" {
		t.Fatalf("second previous Version = %q, want %q", got, "0.124.0")
	}
	if got := secondHistory.NextVersionedCapabilities["Version"]; got != "0.125.0" {
		t.Fatalf("second next Version = %q, want %q", got, "0.125.0")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.VersionedCapabilities["Version"]; got != "0.125.0" {
		t.Fatalf("current Version = %q, want %q", got, "0.125.0")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["Originator"]; got != "codex-tui" {
		t.Fatalf("second stable identity Originator = %q, want %q", got, "codex-tui")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["platform"]; got != "Mac OS 26.3.1; arm64" {
		t.Fatalf("second runtime fingerprint platform = %q, want pinned baseline", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; !strings.Contains(got, "iTerm.app/3.6.9") {
		t.Fatalf("second runtime fingerprint terminal = %q, want preserved iTerm identity", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; strings.Contains(got, "Warp/2.0.0") {
		t.Fatalf("second runtime fingerprint terminal unexpectedly drifted to candidate terminal: %q", got)
	}
	secondStoredState := readStoredState()
	if len(secondStoredState.History) != 2 {
		t.Fatalf("stored second history length = %d, want 2", len(secondStoredState.History))
	}
	if !reflect.DeepEqual(secondStoredState.History, secondResp.AccountSettings.ManagedHeaderState.History) {
		t.Fatalf("stored history = %#v, want %#v", secondStoredState.History, secondResp.AccountSettings.ManagedHeaderState.History)
	}
}

func TestGetAuthFileAccountSettings_MigratesLegacyCodexHeadersToManagedState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	online := true
	oldOverride := runtimehelps.ManagedHeaderOnlineFetchOverride
	runtimehelps.ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (runtimehelps.ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return runtimehelps.ManagedHeaderOnlineVersion{}, false
		}
		return runtimehelps.ManagedHeaderOnlineVersion{
			Version: "0.130.0",
			ManagedHeaderProfileSource: runtimehelps.ManagedHeaderProfileSource{
				Source:    "online:npm",
				SourceURL: "https://registry.npmjs.org/@openai%2fcodex/latest",
				CheckedAt: "2026-04-29T12:00:00Z",
			},
		}, true
	}
	t.Cleanup(func() {
		runtimehelps.ManagedHeaderOnlineFetchOverride = oldOverride
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-legacy-managed-state.json",
		FileName: "codex-legacy-managed-state.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type": "codex",
			"headers": map[string]any{
				"User-Agent": "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
				"Version":    "0.124.0",
				"Originator": "codex-tui",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-legacy-managed-state.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	state := resp.AccountSettings.ManagedHeaderState
	if state == nil || state.Current == nil {
		t.Fatalf("expected migrated managed_header_state.current")
	}
	if got := state.Current.Source; got != "community:codex-proxy" {
		t.Fatalf("current source = %q, want community:codex-proxy", got)
	}
	if got := resp.AccountSettings.ManagedHeaders["Version"]; got != "26.318.11754" {
		t.Fatalf("managed Version = %q, want Codex Desktop app version", got)
	}
	updated, ok := manager.GetByID("codex-legacy-managed-state.json")
	if !ok || updated == nil {
		t.Fatalf("expected updated auth record")
	}
	stored := readAccountSettingsMetadata(updated, cfg)
	if stored.ManagedHeaderState == nil || stored.ManagedHeaderState.Current == nil {
		t.Fatalf("expected managed state persisted into account_settings")
	}
}

func TestGetAuthFileAccountSettings_UsesOnlineManagedHeaderSourceAndRecordsHistory(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	onlineVersion := "0.130.0"
	checkedAt := "2026-04-29T12:00:00Z"
	online := true
	oldOverride := runtimehelps.ManagedHeaderOnlineFetchOverride
	runtimehelps.ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (runtimehelps.ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return runtimehelps.ManagedHeaderOnlineVersion{}, false
		}
		return runtimehelps.ManagedHeaderOnlineVersion{
			Version: onlineVersion,
			ManagedHeaderProfileSource: runtimehelps.ManagedHeaderProfileSource{
				Source:    "online:npm",
				SourceURL: "https://registry.npmjs.org/@openai%2fcodex/latest",
				CheckedAt: checkedAt,
			},
		}, true
	}
	t.Cleanup(func() {
		runtimehelps.ManagedHeaderOnlineFetchOverride = oldOverride
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-online-history.json",
		FileName: "codex-online-history.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/codex-online-history.json",
		},
		Metadata: map[string]any{
			"type": "codex",
			"headers": map[string]any{
				"User-Agent": "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
				"Version":    "0.124.0",
				"Originator": "codex-tui",
			},
			"account_settings": map[string]any{
				"schema_version": 1,
				"managed_header_state": map[string]any{
					"policy_version": "codex-managed/v2",
					"current": map[string]any{
						"generated_at": "2026-04-24T10:00:00Z",
						"source":       "default",
						"summary_headers": map[string]any{
							"User-Agent": "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
							"Version":    "0.124.0",
							"Originator": "codex-tui",
						},
						"versioned_capabilities": map[string]any{
							"User-Agent": "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
							"Version":    "0.124.0",
						},
						"stable_identity": map[string]any{
							"Originator": "codex-tui",
						},
						"runtime_fingerprint": map[string]any{
							"platform": "Mac OS 26.3.1; arm64",
							"terminal": "iTerm.app/3.6.9 (codex-tui; 0.124.0)",
						},
					},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	getAccountSettings := func() authFileAccountSettingsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-online-history.json", nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
		}

		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		return resp
	}

	firstResp := getAccountSettings()
	if got := firstResp.AccountSettings.ManagedHeaders["Version"]; got != "26.318.11754" {
		t.Fatalf("managed Version = %q, want Codex Desktop app version", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.Source; got != "community:codex-proxy" {
		t.Fatalf("current source = %q, want community:codex-proxy", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.SourceURL; got != "https://github.com/icebear0828/codex-proxy" {
		t.Fatalf("current source_url = %q", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.CheckedAt; got != "" {
		t.Fatalf("current checked_at = %q, want empty for static community profile", got)
	}
	if len(firstResp.AccountSettings.ManagedHeaderState.History) != 1 {
		t.Fatalf("history length = %d, want 1: %#v", len(firstResp.AccountSettings.ManagedHeaderState.History), firstResp.AccountSettings.ManagedHeaderState.History)
	}
	firstHistory := firstResp.AccountSettings.ManagedHeaderState.History[0]
	if got := firstHistory.PreviousSource; got != "default" {
		t.Fatalf("previous source = %q, want default", got)
	}
	if got := firstHistory.NextSource; got != "community:codex-proxy" {
		t.Fatalf("next source = %q, want community:codex-proxy", got)
	}
	if got := firstHistory.PreviousVersionedCapabilities["Version"]; got != "0.124.0" {
		t.Fatalf("previous Version = %q, want 0.124.0", got)
	}
	if got := firstHistory.NextVersionedCapabilities["Version"]; got != "26.318.11754" {
		t.Fatalf("next Version = %q, want 26.318.11754", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["Originator"]; got != "Codex Desktop" {
		t.Fatalf("stable identity Originator = %q, want Codex Desktop", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["platform"]; got != "darwin; arm64" {
		t.Fatalf("platform fingerprint = %q, want Codex Desktop platform", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; got != "" {
		t.Fatalf("terminal fingerprint = %q, want empty Codex Desktop terminal", got)
	}

	onlineVersion = "0.131.0"
	checkedAt = "2026-04-29T13:00:00Z"
	secondResp := getAccountSettings()
	if got := secondResp.AccountSettings.ManagedHeaders["Version"]; got != "26.318.11754" {
		t.Fatalf("second managed Version = %q, want Codex Desktop app version", got)
	}
	if len(secondResp.AccountSettings.ManagedHeaderState.History) != 1 {
		t.Fatalf("second history length = %d, want 1", len(secondResp.AccountSettings.ManagedHeaderState.History))
	}
	if !reflect.DeepEqual(secondResp.AccountSettings.ManagedHeaderState.History[0], firstHistory) {
		t.Fatalf("history should append only; first entry changed: %#v", secondResp.AccountSettings.ManagedHeaderState.History)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["Originator"]; got != "Codex Desktop" {
		t.Fatalf("stable identity Originator = %q, want Codex Desktop", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["platform"]; got != "darwin; arm64" {
		t.Fatalf("platform fingerprint = %q, want Codex Desktop platform", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["terminal"]; got != "" {
		t.Fatalf("second terminal fingerprint = %q, want empty Codex Desktop terminal", got)
	}
}

func TestGetAuthFileAccountSettings_UsesOnlineCodexProxyBundleAndRecordsHistory(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	online := true
	oldOverride := runtimehelps.ManagedHeaderOnlineFetchOverride
	runtimehelps.ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (runtimehelps.ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return runtimehelps.ManagedHeaderOnlineVersion{}, false
		}
		return runtimehelps.ManagedHeaderOnlineVersion{
			Version: "26.400.1",
			ManagedHeaderProfileSource: runtimehelps.ManagedHeaderProfileSource{
				Source:       "community:codex-proxy",
				SourceURL:    "https://raw.githubusercontent.com/icebear0828/codex-proxy/master/config/default.yaml https://raw.githubusercontent.com/icebear0828/codex-proxy/master/config/fingerprint.yaml",
				CheckedAt:    "2026-05-09T02:00:00Z",
				Completeness: "online-coherent-bundle",
			},
			CodexProxyBundle: &runtimehelps.CodexProxyManagedHeaderBundle{
				Originator:      "Codex Desktop",
				AppVersion:      "26.400.1",
				Platform:        "darwin",
				Arch:            "arm64",
				ChromiumVersion: "145",
				DefaultHeaders: map[string]string{
					"sec-ch-ua":          `"Chromium";v="145", "Not A(Brand";v="24"`,
					"sec-ch-ua-mobile":   "?0",
					"sec-ch-ua-platform": `"macOS"`,
					"Accept-Encoding":    "gzip, deflate, br, zstd",
					"Accept-Language":    "en-US,en;q=0.9",
					"sec-fetch-site":     "same-origin",
					"sec-fetch-mode":     "cors",
					"sec-fetch-dest":     "empty",
				},
			},
		}, true
	}
	t.Cleanup(func() {
		runtimehelps.ManagedHeaderOnlineFetchOverride = oldOverride
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-proxy-online-history.json",
		FileName: "codex-proxy-online-history.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type": "codex",
			"account_settings": map[string]any{
				"schema_version": 1,
				"managed_header_state": map[string]any{
					"policy_version": "codex-managed/v2",
					"current": map[string]any{
						"generated_at": "2026-05-08T10:00:00Z",
						"source":       "community:codex-proxy",
						"source_url":   "https://github.com/icebear0828/codex-proxy",
						"completeness": "static-coherent-bundle",
						"summary_headers": map[string]any{
							"User-Agent": "Codex Desktop/26.318.11754 (darwin; arm64)",
							"Version":    "26.318.11754",
							"Originator": "Codex Desktop",
						},
						"versioned_capabilities": map[string]any{
							"User-Agent": "Codex Desktop/26.318.11754 (darwin; arm64)",
							"Version":    "26.318.11754",
						},
						"stable_identity": map[string]any{
							"Originator":         "Codex Desktop",
							"sec-ch-ua":          `"Chromium";v="144"`,
							"sec-ch-ua-mobile":   "?0",
							"sec-ch-ua-platform": `"macOS"`,
						},
						"runtime_fingerprint": map[string]any{
							"platform": "darwin; arm64",
							"terminal": "",
						},
					},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-proxy-online-history.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	state := resp.AccountSettings.ManagedHeaderState
	if state == nil || state.Current == nil {
		t.Fatalf("expected managed header state")
	}
	if got := resp.AccountSettings.ManagedHeaders["Version"]; got != "26.400.1" {
		t.Fatalf("managed Version = %q, want online codex-proxy version", got)
	}
	if got := state.Current.CheckedAt; got != "2026-05-09T02:00:00Z" {
		t.Fatalf("checked_at = %q", got)
	}
	if got := state.Current.Completeness; got != "online-coherent-bundle" {
		t.Fatalf("completeness = %q", got)
	}
	if got := state.Current.StableIdentity["sec-ch-ua"]; !strings.Contains(got, `"Chromium";v="145"`) {
		t.Fatalf("sec-ch-ua = %q, want Chromium 145", got)
	}
	if len(state.History) != 1 {
		t.Fatalf("history length = %d, want 1: %#v", len(state.History), state.History)
	}
	history := state.History[0]
	if got := history.PreviousVersionedCapabilities["Version"]; got != "26.318.11754" {
		t.Fatalf("previous version = %q", got)
	}
	if got := history.NextVersionedCapabilities["Version"]; got != "26.400.1" {
		t.Fatalf("next version = %q", got)
	}
	if !containsString(history.ChangedFields, "Version") || !containsString(history.ChangedFields, "sec-ch-ua") {
		t.Fatalf("changed fields = %#v, want version and sec-ch-ua changes", history.ChangedFields)
	}
}

func TestGetAuthFileAccountSettings_PersistsClaudeManagedHeaderHistoryAcrossVersionUpgrades(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	runtimehelps.ResetClaudeDeviceProfileCache()

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-history.json",
		FileName: "claude-history.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-history.json",
		},
		Metadata: map[string]any{
			"type": "claude",
			"headers": map[string]any{
				"User-Agent":                  "claude-cli/2.1.63 (external, cli)",
				"X-App":                       "cli",
				"X-Stainless-Package-Version": "0.74.0",
				"X-Stainless-Runtime-Version": "v24.3.0",
				"X-Stainless-Timeout":         "600",
				"X-Stainless-Os":              "MacOS",
				"X-Stainless-Arch":            "arm64",
			},
			"account_settings": map[string]any{
				"schema_version": 1,
				"managed_header_state": map[string]any{
					"policy_version": "claude-managed/v2",
					"current": map[string]any{
						"generated_at": "2026-04-24T10:00:00Z",
						"summary_headers": map[string]any{
							"User-Agent":                  "claude-cli/2.1.63 (external, cli)",
							"X-App":                       "cli",
							"X-Stainless-Package-Version": "0.74.0",
							"X-Stainless-Runtime-Version": "v24.3.0",
							"X-Stainless-Timeout":         "600",
						},
						"versioned_capabilities": map[string]any{
							"User-Agent":                  "claude-cli/2.1.63 (external, cli)",
							"X-Stainless-Package-Version": "0.74.0",
							"X-Stainless-Runtime-Version": "v24.3.0",
							"X-Stainless-Timeout":         "600",
						},
						"stable_identity": map[string]any{
							"X-App": "cli",
						},
						"runtime_fingerprint": map[string]any{
							"X-Stainless-Os":   "MacOS",
							"X-Stainless-Arch": "arm64",
						},
					},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.63 (external, cli)",
			PackageVersion: "0.74.0",
			RuntimeVersion: "v24.3.0",
			Timeout:        "600",
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	projectClaudeVersion := func(version string, packageVersion string, runtimeVersion string) {
		t.Helper()
		_ = runtimehelps.ResolveClaudeDeviceProfile(record, "", http.Header{
			"User-Agent":                  []string{"claude-cli/" + version + " (external, cli)"},
			"X-Stainless-Package-Version": []string{packageVersion},
			"X-Stainless-Runtime-Version": []string{runtimeVersion},
			"X-Stainless-Os":              []string{"Linux"},
			"X-Stainless-Arch":            []string{"x64"},
		}, cfg)
	}
	getAccountSettings := func() authFileAccountSettingsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-history.json", nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
		}

		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		return resp
	}
	readStoredState := func() *authFileManagedHeaderState {
		t.Helper()
		updated, ok := manager.GetByID("claude-history.json")
		if !ok || updated == nil {
			t.Fatalf("expected updated auth record")
		}
		stored := readAccountSettingsMetadata(updated, cfg)
		if stored.ManagedHeaderState == nil {
			t.Fatalf("expected stored managed_header_state to be present")
		}
		return stored.ManagedHeaderState
	}

	projectClaudeVersion("2.2.0", "0.75.0", "v24.4.0")
	firstResp := getAccountSettings()
	if firstResp.AccountSettings.ManagedHeaderState == nil {
		t.Fatalf("expected first managed_header_state to be present")
	}
	if len(firstResp.AccountSettings.ManagedHeaderState.History) != 1 {
		t.Fatalf("first history length = %d, want 1", len(firstResp.AccountSettings.ManagedHeaderState.History))
	}
	firstHistory := firstResp.AccountSettings.ManagedHeaderState.History[0]
	if got := firstHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version"}) {
		t.Fatalf("first changed_fields = %#v, want only claude version fields", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["X-App"]; got != "cli" {
		t.Fatalf("stable identity X-App = %q, want %q", got, "cli")
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["X-Stainless-Os"]; got != "MacOS" {
		t.Fatalf("runtime fingerprint os = %q, want pinned baseline", got)
	}
	if got := firstResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["X-Stainless-Arch"]; got != "arm64" {
		t.Fatalf("runtime fingerprint arch = %q, want pinned baseline", got)
	}
	if len(readStoredState().History) != 1 {
		t.Fatalf("stored first history length mismatch")
	}

	projectClaudeVersion("2.3.0", "0.76.0", "v24.5.0")
	secondResp := getAccountSettings()
	if secondResp.AccountSettings.ManagedHeaderState == nil {
		t.Fatalf("expected second managed_header_state to be present")
	}
	if len(secondResp.AccountSettings.ManagedHeaderState.History) != 2 {
		t.Fatalf("second history length = %d, want 2", len(secondResp.AccountSettings.ManagedHeaderState.History))
	}
	if !reflect.DeepEqual(secondResp.AccountSettings.ManagedHeaderState.History[0], firstHistory) {
		t.Fatalf("history should append only; first claude entry changed: %#v", secondResp.AccountSettings.ManagedHeaderState.History)
	}
	secondHistory := secondResp.AccountSettings.ManagedHeaderState.History[1]
	if got := secondHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version"}) {
		t.Fatalf("second changed_fields = %#v, want only claude version fields", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.VersionedCapabilities["X-Stainless-Package-Version"]; got != "0.76.0" {
		t.Fatalf("current package version = %q, want %q", got, "0.76.0")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.VersionedCapabilities["X-Stainless-Runtime-Version"]; got != "v24.5.0" {
		t.Fatalf("current runtime version = %q, want %q", got, "v24.5.0")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.StableIdentity["X-App"]; got != "cli" {
		t.Fatalf("second stable identity X-App = %q, want %q", got, "cli")
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["X-Stainless-Os"]; got != "MacOS" {
		t.Fatalf("second runtime fingerprint os = %q, want pinned baseline", got)
	}
	if got := secondResp.AccountSettings.ManagedHeaderState.Current.RuntimeFingerprint["X-Stainless-Arch"]; got != "arm64" {
		t.Fatalf("second runtime fingerprint arch = %q, want pinned baseline", got)
	}
	secondStoredState := readStoredState()
	if len(secondStoredState.History) != 2 {
		t.Fatalf("stored second history length = %d, want 2", len(secondStoredState.History))
	}
	if !reflect.DeepEqual(secondStoredState.History, secondResp.AccountSettings.ManagedHeaderState.History) {
		t.Fatalf("stored claude history = %#v, want %#v", secondStoredState.History, secondResp.AccountSettings.ManagedHeaderState.History)
	}
}

func TestGetAuthFileAccountSettings_ReturnsClaudeClientVersionObservations(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	runtimehelps.ResetClaudeDeviceProfileCache()
	t.Cleanup(runtimehelps.ResetClaudeDeviceProfileCache)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-observations.json",
		FileName: "claude-observations.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-observations.json",
		},
		Metadata: map[string]any{
			"type": "claude",
			"account_settings": map[string]any{
				"schema_version": 1,
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	cfg := &config.Config{AuthDir: t.TempDir()}
	_ = runtimehelps.ResolveClaudeDeviceProfile(record, "", http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.140 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.80.0"},
		"X-Stainless-Runtime-Version": []string{"v24.5.0"},
	}, cfg)
	_ = runtimehelps.ResolveClaudeDeviceProfile(record, "", http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
	}, cfg)

	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-observations.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	observations := resp.AccountSettings.ClientObservations
	if len(observations) != 2 {
		t.Fatalf("client observations length = %d, want 2: %#v", len(observations), observations)
	}
	versions := map[string]bool{}
	for _, observation := range observations {
		versions[observation.Version] = true
	}
	if !versions["2.1.140"] || !versions["2.1.142"] {
		t.Fatalf("expected observed versions 2.1.140 and 2.1.142, got %#v", observations)
	}
}

func TestGetAuthFileAccountSettings_PersistsCoreManagedRuntimeIdentity(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	records := []*coreauth.Auth{
		{
			ID:       "claude-runtime.json",
			FileName: "claude-runtime.json",
			Provider: "claude",
			ProxyURL: "http://shared-proxy:8080",
			Metadata: map[string]any{
				"type":  "claude",
				"email": "claude-a@example.test",
			},
		},
		{
			ID:       "codex-runtime-a.json",
			FileName: "codex-runtime-a.json",
			Provider: "codex",
			ProxyURL: "http://shared-proxy:8080",
			Metadata: map[string]any{
				"type":  "codex",
				"email": "codex-a@example.test",
			},
		},
		{
			ID:       "codex-runtime-b.json",
			FileName: "codex-runtime-b.json",
			Provider: "codex",
			ProxyURL: "http://shared-proxy:8080",
			Metadata: map[string]any{
				"type":  "codex",
				"email": "codex-b@example.test",
			},
		},
		{
			ID:       "gemini-runtime.json",
			FileName: "gemini-runtime.json",
			Provider: "gemini-cli",
			Metadata: map[string]any{
				"type": "gemini-cli",
			},
		},
	}
	for _, record := range records {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	getAccountSettings := func(name string) authFileAccountSettingsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name="+name, nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d for %s, got %d with body %s", http.StatusOK, name, rec.Code, rec.Body.String())
		}
		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response for %s: %v", name, err)
		}
		return resp
	}

	claude := getAccountSettings("claude-runtime.json")
	if claude.AccountSettings.RuntimeIdentity == nil || claude.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected runtime identity for claude account without explicit profile")
	}
	// claude default (no explicit tls_profile) now surfaces the replicated
	// claude-cli ClientHello identity, which is the core-managed claude->anthropic
	// default outbound profile.
	if got := claude.AccountSettings.RuntimeIdentity.Current.ProfileID; got != "claude_cli_clienthello_v1" {
		t.Fatalf("claude profile_id = %q, want claude_cli_clienthello_v1", got)
	}
	if got := claude.AccountSettings.RuntimeIdentity.Current.TLSProfileID; got != "claude_cli_clienthello_v1" {
		t.Fatalf("claude tls_profile_id = %q, want claude_cli_clienthello_v1", got)
	}
	if !claude.AccountSettings.RuntimeIdentity.Current.CoreManaged || !claude.AccountSettings.RuntimeIdentity.Current.RuntimeEnforced {
		t.Fatalf("claude identity core/runtime flags = core:%v enforced:%v", claude.AccountSettings.RuntimeIdentity.Current.CoreManaged, claude.AccountSettings.RuntimeIdentity.Current.RuntimeEnforced)
	}

	first := getAccountSettings("codex-runtime-a.json")
	if first.AccountSettings.RuntimeIdentity == nil || first.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected runtime_identity.current for codex account")
	}
	firstIdentity := first.AccountSettings.RuntimeIdentity.Current
	if firstIdentity.IdentityID == "" {
		t.Fatalf("identity_id should be generated")
	}
	if firstIdentity.ProfileID != "codex_proxy_compatible_v1" || firstIdentity.TLSProfileID != "codex_proxy_compatible_v1" {
		t.Fatalf("profile IDs = (%q, %q), want codex_proxy_compatible_v1", firstIdentity.ProfileID, firstIdentity.TLSProfileID)
	}
	if !firstIdentity.CoreManaged || !firstIdentity.RuntimeEnforced {
		t.Fatalf("identity core/runtime flags = core:%v enforced:%v", firstIdentity.CoreManaged, firstIdentity.RuntimeEnforced)
	}
	if firstIdentity.Revision != 1 {
		t.Fatalf("revision = %d, want 1", firstIdentity.Revision)
	}
	if firstIdentity.AuthIDHash == "" || firstIdentity.AccountHash == "" || firstIdentity.ProxyHash == "" || firstIdentity.SeedHash == "" {
		t.Fatalf("expected hashed identity fields: %#v", firstIdentity)
	}
	if strings.Contains(firstIdentity.AccountHash, "codex-a@example.test") || strings.Contains(firstIdentity.ProxyHash, "shared-proxy") {
		t.Fatalf("identity should not expose raw account/proxy values: %#v", firstIdentity)
	}

	repeated := getAccountSettings("codex-runtime-a.json")
	if repeated.AccountSettings.RuntimeIdentity == nil || repeated.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected repeated runtime identity")
	}
	if repeated.AccountSettings.RuntimeIdentity.Current.IdentityID != firstIdentity.IdentityID {
		t.Fatalf("identity_id changed across reads: %q -> %q", firstIdentity.IdentityID, repeated.AccountSettings.RuntimeIdentity.Current.IdentityID)
	}
	if repeated.AccountSettings.RuntimeIdentity.Current.Revision != 1 {
		t.Fatalf("revision after repeated read = %d, want 1", repeated.AccountSettings.RuntimeIdentity.Current.Revision)
	}
	if len(repeated.AccountSettings.RuntimeIdentity.History) != 0 {
		t.Fatalf("history after repeated read = %#v, want empty", repeated.AccountSettings.RuntimeIdentity.History)
	}

	second := getAccountSettings("codex-runtime-b.json")
	if second.AccountSettings.RuntimeIdentity == nil || second.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected runtime identity for second codex account")
	}
	if second.AccountSettings.RuntimeIdentity.Current.IdentityID == firstIdentity.IdentityID {
		t.Fatalf("different account should get different identity_id %q", firstIdentity.IdentityID)
	}

	gemini := getAccountSettings("gemini-runtime.json")
	if gemini.AccountSettings.RuntimeIdentity == nil || gemini.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected runtime identity for gemini-cli account without managed headers")
	}
	if got := gemini.AccountSettings.RuntimeIdentity.Current.ProfileID; got != "gemini_cli_native_v1" {
		t.Fatalf("gemini profile_id = %q, want gemini_cli_native_v1", got)
	}

	updated, ok := manager.GetByID("gemini-runtime.json")
	if !ok || updated == nil {
		t.Fatalf("expected stored gemini auth")
	}
	stored := readAccountSettingsMetadata(updated, &config.Config{AuthDir: t.TempDir()})
	if stored.RuntimeIdentityState == nil || stored.RuntimeIdentityState.Current == nil {
		t.Fatalf("expected runtime_identity_state persisted for provider without managed headers")
	}
}

func TestPatchAuthFileAccountSettings_AppendsRuntimeIdentityHistoryOnProfileChange(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-runtime-history.json",
		FileName: "codex-runtime-history.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "codex-history@example.test",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	getAccountSettings := func() authFileAccountSettingsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-runtime-history.json", nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected GET status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
		}
		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode get response: %v", err)
		}
		return resp
	}

	initial := getAccountSettings()
	if initial.AccountSettings.RuntimeIdentity == nil || initial.AccountSettings.RuntimeIdentity.Current == nil {
		t.Fatalf("expected initial runtime identity")
	}
	initialIdentity := initial.AccountSettings.RuntimeIdentity.Current

	body := `{"name":"codex-runtime-history.json","proxy_url":"http://test-proxy:8080","disabled":false,"extra_headers":{},"transport_profile":{"preset":"provider-default","alpn":["h2"]}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected PATCH status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var patched authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("failed to decode patch response: %v", err)
	}
	state := patched.AccountSettings.RuntimeIdentity
	if state == nil || state.Current == nil {
		t.Fatalf("expected patched runtime identity")
	}
	if state.Current.IdentityID == initialIdentity.IdentityID {
		t.Fatalf("identity_id should change when profile seed changes")
	}
	if state.Current.ProfileID != "provider-default" {
		t.Fatalf("current profile_id = %q, want provider-default", state.Current.ProfileID)
	}
	if state.Current.CoreManaged {
		t.Fatalf("explicit provider-default profile should not be marked core-managed")
	}
	if len(state.History) != 1 {
		t.Fatalf("history length = %d, want 1: %#v", len(state.History), state.History)
	}
	history := state.History[0]
	if history.Previous == nil || history.Next == nil {
		t.Fatalf("history should store previous and next snapshots: %#v", history)
	}
	if history.Previous.IdentityID != initialIdentity.IdentityID {
		t.Fatalf("history previous identity_id = %q, want %q", history.Previous.IdentityID, initialIdentity.IdentityID)
	}
	if !containsString(history.ChangedFields, "profile_id") || !containsString(history.ChangedFields, "source") {
		t.Fatalf("changed fields = %#v, want profile/source changes", history.ChangedFields)
	}
}

func TestGetAuthFileAccountSettings_CodexSupportedTransportProfileWarnsAboutScope(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-transport.json",
		FileName: "codex-transport.json",
		Provider: "codex",
		ProxyURL: "http://proxy.remote",
		Attributes: map[string]string{
			"path": "/tmp/codex-transport.json",
		},
		Metadata: map[string]any{
			"type": "codex",
			"account_settings": map[string]any{
				"schema_version": 1,
				"transport_profile": map[string]any{
					"preset": "provider-default",
					"alpn":   []string{"h2"},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "managed-codex-ua/1.2.3",
			BetaFeatures: "feature-a",
		},
	}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-transport.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.AccountSettings.Warnings) != 1 {
		t.Fatalf("warnings = %#v, want exactly 1 codex scope warning", resp.AccountSettings.Warnings)
	}
	if !strings.Contains(resp.AccountSettings.Warnings[0], "account-scoped transport isolation only") {
		t.Fatalf("unexpected codex warning: %#v", resp.AccountSettings.Warnings)
	}
}

func TestPatchAuthFileAccountSettings_RejectsManagedHeaderConflicts(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-conflict.json",
		FileName: "claude-conflict.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-conflict.json",
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"claude-conflict.json","proxy_url":"http://test-proxy:8080","note":null,"disabled":false,"extra_headers":{"User-Agent":"manual-override"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestGetAuthFileAccountSettings_ClaudeTransportProfileShowsRuntimeActive(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-runtime.json",
		FileName: "claude-runtime.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-runtime.json",
		},
		Metadata: map[string]any{
			"type": "claude",
			"account_settings": map[string]any{
				"schema_version": 1,
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v2",
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-runtime.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccountSettings.Activation.State != "transport-profile-active" {
		t.Fatalf("activation.state = %q, want %q", resp.AccountSettings.Activation.State, "transport-profile-active")
	}
	if resp.AccountSettings.Activation.Summary != "transport profile active" {
		t.Fatalf("activation.summary = %q, want %q", resp.AccountSettings.Activation.Summary, "transport profile active")
	}
	if len(resp.AccountSettings.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want 0 for supported claude runtime transport_profile", resp.AccountSettings.Warnings)
	}
}

func TestGetAuthFileAccountSettings_ClaudeTLSProfileShowsRuntimeActive(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-tls-runtime.json",
		FileName: "claude-tls-runtime.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-tls-runtime.json",
		},
		Metadata: map[string]any{
			"type": "claude",
			"account_settings": map[string]any{
				"schema_version": 1,
				"tls_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v3",
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-tls-runtime.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccountSettings.Activation.State != "tls-profile-active" {
		t.Fatalf("activation.state = %q, want %q", resp.AccountSettings.Activation.State, "tls-profile-active")
	}
	if resp.AccountSettings.Activation.Summary != "TLS profile active" {
		t.Fatalf("activation.summary = %q, want %q", resp.AccountSettings.Activation.Summary, "TLS profile active")
	}
	if len(resp.AccountSettings.Warnings) != 1 || !strings.Contains(resp.AccountSettings.Warnings[0], "uTLS ClientHello") {
		t.Fatalf("warnings = %#v, want claude uTLS runtime warning", resp.AccountSettings.Warnings)
	}
}

func TestGetAuthFileAccountSettings_CodexTLSProfileWarnsAboutScope(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-tls-runtime.json",
		FileName: "codex-tls-runtime.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/codex-tls-runtime.json",
		},
		Metadata: map[string]any{
			"type": "codex",
			"account_settings": map[string]any{
				"schema_version": 1,
				"tls_profile": map[string]any{
					"preset":       "codex_go_http11_v1",
					"force_http11": true,
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-tls-runtime.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccountSettings.Activation.State != "tls-profile-active" {
		t.Fatalf("activation.state = %q, want %q", resp.AccountSettings.Activation.State, "tls-profile-active")
	}
	if len(resp.AccountSettings.Warnings) != 1 || !strings.Contains(resp.AccountSettings.Warnings[0], "not the Codex Desktop rustls native transport yet") {
		t.Fatalf("warnings = %#v, want codex scoped tls warning", resp.AccountSettings.Warnings)
	}
}

func TestGetAuthFileAccountSettings_FallsBackToDisplayNameWhenFileNameMissing(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-runtime.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/runtime/codex-runtime.json",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-runtime.json", nil)
	ctx.Request = req
	h.GetAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got := resp.Name; got != "codex-runtime.json" {
		t.Fatalf("response name = %q, want %q", got, "codex-runtime.json")
	}
}

// TestGetAuthFileAccountSettings_SyntheticDeviceIDMasked verifies that GET
// account-settings returns a masked synthetic_device_id (first 16 hex chars +
// ellipsis), that it is stable across repeated calls for the same account, and
// that it is absent (omitempty) when the auth record is missing.
func TestGetAuthFileAccountSettings_SyntheticDeviceIDMasked(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-synthetic.json",
		FileName: "claude-synthetic.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-synthetic.json",
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/3.0.0 (external, cli)",
			PackageVersion: "0.90.0",
			RuntimeVersion: "v30.0.0",
		},
	}, manager)

	callGet := func() authFileAccountSettingsView {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-synthetic.json", nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
		}
		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		return resp.AccountSettings
	}

	first := callGet()

	// Must be present and non-empty.
	if first.SyntheticDeviceID == "" {
		t.Fatal("synthetic_device_id must be non-empty for a valid account")
	}

	// Must be masked: ends with ellipsis, prefix is exactly 16 hex chars.
	if !strings.HasSuffix(first.SyntheticDeviceID, "…") {
		t.Fatalf("synthetic_device_id = %q: must end with ellipsis", first.SyntheticDeviceID)
	}
	// The ellipsis character "…" is 3 UTF-8 bytes; the mask prefix is everything before it.
	prefix := strings.TrimSuffix(first.SyntheticDeviceID, "…")
	if len(prefix) != 16 {
		t.Fatalf("synthetic_device_id prefix length = %d, want 16; got %q", len(prefix), first.SyntheticDeviceID)
	}
	for _, r := range prefix {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("synthetic_device_id prefix %q contains non-hex character %q", prefix, string(r))
		}
	}

	// Must not contain the full 64-hex raw device id: the prefix has only 16 chars.
	if len(prefix) >= 64 {
		t.Fatalf("synthetic_device_id must not expose the full 64-hex value; got prefix len %d", len(prefix))
	}

	// Must be stable across repeated GET calls for the same account.
	second := callGet()
	if second.SyntheticDeviceID != first.SyntheticDeviceID {
		t.Fatalf("synthetic_device_id changed between calls: %q != %q", first.SyntheticDeviceID, second.SyntheticDeviceID)
	}
}

// TestPatchAuthFileAccountSettings_SyntheticDeviceIDIsReadOnly verifies that
// sending synthetic_device_id in a PATCH body does not overwrite any persisted
// state and that the response still returns the server-derived masked value.
func TestPatchAuthFileAccountSettings_SyntheticDeviceIDIsReadOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude-patch-readonly.json",
		FileName: "claude-patch-readonly.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/claude-patch-readonly.json",
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: t.TempDir(),
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/3.0.0 (external, cli)",
			PackageVersion: "0.90.0",
			RuntimeVersion: "v30.0.0",
		},
	}, manager)

	// First GET to capture the server-derived masked value.
	getSettings := func() string {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=claude-patch-readonly.json", nil)
		ctx.Request = req
		h.GetAuthFileAccountSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
		}
		var resp authFileAccountSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("GET decode: %v", err)
		}
		return resp.AccountSettings.SyntheticDeviceID
	}

	serverDerived := getSettings()
	if serverDerived == "" {
		t.Fatal("server-derived synthetic_device_id must be non-empty")
	}

	// PATCH that attempts to overwrite synthetic_device_id.
	body := `{"name":"claude-patch-readonly.json","proxy_url":"http://test-proxy:8080","disabled":false,"synthetic_device_id":"aaaa000000000000…"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountSettings(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var patchResp authFileAccountSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &patchResp); err != nil {
		t.Fatalf("PATCH decode: %v", err)
	}

	// Response must return the server-derived value, not the client-supplied one.
	if patchResp.AccountSettings.SyntheticDeviceID != serverDerived {
		t.Fatalf("PATCH response synthetic_device_id = %q, want server-derived %q; field appears writable",
			patchResp.AccountSettings.SyntheticDeviceID, serverDerived)
	}

	// Confirm the stored metadata does not contain synthetic_device_id.
	updated, ok := manager.GetByID("claude-patch-readonly.json")
	if !ok || updated == nil {
		t.Fatal("expected updated auth record to be present")
	}
	raw, _ := json.Marshal(updated.Metadata["account_settings"])
	if strings.Contains(string(raw), "synthetic_device_id") {
		t.Fatalf("account_settings metadata must not persist synthetic_device_id; got: %s", string(raw))
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
