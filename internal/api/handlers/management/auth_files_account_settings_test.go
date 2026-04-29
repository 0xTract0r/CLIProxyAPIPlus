package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
	if got := headersMeta["Originator"]; got != "codex-tui" {
		t.Fatalf("metadata.headers.Originator = %#v, want %q", got, "codex-tui")
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
	if got := resp.AccountSettings.ManagedHeaders["Originator"]; got != "codex-tui" {
		t.Fatalf("response managed Originator = %q, want %q", got, "codex-tui")
	}
	if resp.AccountSettings.ManagedHeaderState == nil || resp.AccountSettings.ManagedHeaderState.Current == nil {
		t.Fatalf("expected managed_header_state.current to be present")
	}
}

func TestGetAuthFileAccountSettings_PersistsCodexManagedHeaderHistoryAcrossVersionUpgrades(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex-history.json",
		FileName: "codex-history.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/codex-history.json",
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
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/account-settings?name=codex-history.json", nil)
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
		updated, ok := manager.GetByID("codex-history.json")
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
		t.Fatalf("history length = %d, want 1", len(firstResp.AccountSettings.ManagedHeaderState.History))
	}
	firstHistory := firstResp.AccountSettings.ManagedHeaderState.History[0]
	if got := firstHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "Version"}) {
		t.Fatalf("first changed_fields = %#v, want only version markers", got)
	}
	if got := firstHistory.PreviousVersionedCapabilities["Version"]; got != "0.118.0" {
		t.Fatalf("previous Version = %q, want %q", got, "0.118.0")
	}
	if got := firstHistory.NextVersionedCapabilities["Version"]; got != "0.124.0" {
		t.Fatalf("next Version = %q, want %q", got, "0.124.0")
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
	if got := secondHistory.ChangedFields; !reflect.DeepEqual(got, []string{"User-Agent", "Version"}) {
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

	body := `{"name":"claude-conflict.json","proxy_url":null,"note":null,"disabled":false,"extra_headers":{"User-Agent":"manual-override"}}`
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
