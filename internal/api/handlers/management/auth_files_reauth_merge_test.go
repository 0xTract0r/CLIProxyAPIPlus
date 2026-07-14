package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func writeStubAuthFile(path string) error {
	return os.WriteFile(path, []byte(`{"type":"codex","email":"user@example.com"}`), 0o600)
}

// TestSaveTokenRecord_PreservesUserFieldsOnReauth asserts that re-authentication
// for an existing OAuth account does not wipe operator-configured fields such
// as proxy_url / note / headers / refresh_disabled / account_settings.
func TestSaveTokenRecord_PreservesUserFieldsOnReauth(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "codex-user@example.com.json",
		FileName: "codex-user@example.com.json",
		Provider: "codex",
		ProxyURL: "http://proxy.local:7897",
		Attributes: map[string]string{
			"path":         "/tmp/codex-user@example.com.json",
			"header:X-Foo": "bar",
			"note":         "managed account",
		},
		Metadata: map[string]any{
			"type":             "codex",
			"email":            "user@example.com",
			"account_id":       "acct-1",
			"proxy_url":        "http://proxy.local:7897",
			"note":             "managed account",
			"refresh_disabled": true,
			"websockets":       true,
			"disabled":         false,
			"headers": map[string]any{
				"X-Foo": "bar",
			},
			"account_settings": map[string]any{
				"schema_version":  1,
				"refresh_enabled": false,
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "codex-user@example.com.json",
		FileName: "codex-user@example.com.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-1",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	if got, _ := record.Metadata["proxy_url"].(string); got != "http://proxy.local:7897" {
		t.Fatalf("proxy_url = %q, want %q", got, "http://proxy.local:7897")
	}
	if record.ProxyURL != "http://proxy.local:7897" {
		t.Fatalf("record.ProxyURL = %q, want %q", record.ProxyURL, "http://proxy.local:7897")
	}
	if got, _ := record.Metadata["note"].(string); got != "managed account" {
		t.Fatalf("note = %q, want %q", got, "managed account")
	}
	if got, _ := record.Metadata["refresh_disabled"].(bool); !got {
		t.Fatalf("refresh_disabled = %v, want true", got)
	}
	if got, _ := record.Metadata["websockets"].(bool); !got {
		t.Fatalf("websockets = %v, want true", got)
	}
	headers, ok := record.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers metadata missing or wrong type: %T", record.Metadata["headers"])
	}
	if got := headers["X-Foo"]; got != "bar" {
		t.Fatalf("headers.X-Foo = %v, want %q", got, "bar")
	}
	settings, ok := record.Metadata["account_settings"].(map[string]any)
	if !ok {
		t.Fatalf("account_settings missing or wrong type: %T", record.Metadata["account_settings"])
	}
	if got, _ := settings["refresh_enabled"].(bool); got {
		t.Fatalf("account_settings.refresh_enabled = %v, want false", got)
	}
	if got := record.Attributes["header:X-Foo"]; got != "bar" {
		t.Fatalf("attributes.header:X-Foo = %q, want %q", got, "bar")
	}
	if got := record.Attributes["note"]; got != "managed account" {
		t.Fatalf("attributes.note = %q, want %q", got, "managed account")
	}
}

// TestSaveTokenRecord_PreservesClaudeDeviceIDOnReauth asserts that a bare
// re-login (record has no claude_device_id, OAuth callback does not know
// about it) inherits the previously persisted explicit device_id override
// from the existing record with the same identity, via
// mergeUserDefinedAuthMetadataInto's generic operator-metadata inheritance.
func TestSaveTokenRecord_PreservesClaudeDeviceIDOnReauth(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	explicit := "a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2c3a1b2"
	previous := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path":                              "/tmp/claude-user@example.com.json",
			coreauth.ClaudeDeviceIDAttributeKey: explicit,
		},
		Metadata: map[string]any{
			"type":                             "claude",
			"email":                            "user@example.com",
			coreauth.ClaudeDeviceIDMetadataKey: explicit,
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"access_token":  "NEW_TOKEN",
			"refresh_token": "NEW_REFRESH",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	if got, _ := record.Metadata[coreauth.ClaudeDeviceIDMetadataKey].(string); got != explicit {
		t.Fatalf("metadata.claude_device_id = %q, want inherited %q", got, explicit)
	}
}

// TestSaveTokenRecord_DoesNotLeakOldMetadataAcrossAccounts ensures that the
// fallback lookup by email only triggers when there is an actual identity
// match, so different operators on the same host do not accidentally inherit
// each other's overrides during initial login.
func TestSaveTokenRecord_DoesNotLeakOldMetadataAcrossAccounts(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "codex-someone-else@example.com.json",
		FileName: "codex-someone-else@example.com.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/codex-someone-else@example.com.json",
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "someone-else@example.com",
			"proxy_url": "http://proxy.other:7897",
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "codex-fresh@example.com.json",
		FileName: "codex-fresh@example.com.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "fresh@example.com",
			"account_id": "acct-new",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}
	if got, _ := record.Metadata["proxy_url"].(string); got != "" {
		t.Fatalf("expected no proxy_url for fresh account, got %q", got)
	}
}

// TestSaveTokenRecord_RenamedFileNameInheritsAndRemovesOrphan covers the
// plan-type change scenario: Codex re-auth that promotes the plan from "plus"
// to "pro" picks a new credential filename, so we must still inherit user
// fields from the old file and clean up the leftover orphan on disk.
func TestSaveTokenRecord_RenamedFileNameInheritsAndRemovesOrphan(t *testing.T) {
	authDir := t.TempDir()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	oldPath := filepath.Join(authDir, "codex-user@example.com-plus.json")
	if err := writeStubAuthFile(oldPath); err != nil {
		t.Fatalf("failed to seed orphan file: %v", err)
	}
	previous := &coreauth.Auth{
		ID:       "codex-user@example.com-plus.json",
		FileName: "codex-user@example.com-plus.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": oldPath,
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "user@example.com",
			"proxy_url": "http://proxy.local:7897",
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "codex-user@example.com-pro.json",
		FileName: "codex-user@example.com-pro.json",
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-pro",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	if got, _ := record.Metadata["proxy_url"].(string); got != "http://proxy.local:7897" {
		t.Fatalf("renamed record proxy_url = %q, want %q", got, "http://proxy.local:7897")
	}
	if _, errStat := os.Stat(oldPath); errStat == nil {
		t.Fatalf("expected orphan file %s to be removed", oldPath)
	}
}

// TestSaveTokenRecord_DropsStaleQuotaRuntimeStateOnReauth asserts that a
// re-auth round-trip inherits operator-controlled metadata but deliberately
// drops derived quota runtime state (quota_refresh_status / quota_refresh_error
// / quota_next_refresh_after). Keeping a stale quota_refresh_status=reauth_required
// across re-auth was the root cause of a recovered Claude credential continuing
// to show "needs re-auth" in the management Quota page (T008).
func TestSaveTokenRecord_DropsStaleQuotaRuntimeStateOnReauth(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":  "claude",
			"email": "user@example.com",
			// operator-controlled, must still be inherited
			"note":             "managed account",
			"refresh_disabled": true,
			// derived quota runtime state, must NOT survive re-auth
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  "Claude credential unauthorized; reauthenticate this credential to refresh quota.",
			quotaNextRefreshMetadataKey:   "2026-06-02T16:32:30Z",
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"access_token":  "NEW_TOKEN",
			"refresh_token": "NEW_REFRESH",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	// Operator metadata still inherited.
	if got, _ := record.Metadata["note"].(string); got != "managed account" {
		t.Fatalf("note = %q, want inherited %q", got, "managed account")
	}
	if got, _ := record.Metadata["refresh_disabled"].(bool); !got {
		t.Fatalf("refresh_disabled = %v, want inherited true", got)
	}

	// Stale quota runtime state dropped so the recovered credential is not stuck.
	for _, key := range []string{
		quotaRefreshStatusMetadataKey,
		quotaRefreshErrorMetadataKey,
		quotaNextRefreshMetadataKey,
	} {
		if _, ok := record.Metadata[key]; ok {
			t.Fatalf("metadata[%q] survived re-auth, want dropped: %#v", key, record.Metadata[key])
		}
	}
}

// TestSaveTokenRecord_ClearsAutomaticReauthLockOnReauth asserts the bcd898
// stopgap fix (#154): a credential that was automatically locked by
// markRefreshReauthRequiredWithReason (reauth_required / refresh_status /
// refresh_error_code / refresh_disabled_reason / refresh_disabled /
// refresh_disabled_at / last_refresh_error, plus StatusMessage
// "reauth_required") must have that whole lock cleared by a completed reauth,
// so RefreshDisabled() flips back to false and automatic refresh resumes
// without the operator also having to flip the management UI toggle by hand.
func TestSaveTokenRecord_ClearsAutomaticReauthLockOnReauth(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:            "claude-user@example.com.json",
		FileName:      "claude-user@example.com.json",
		Provider:      "claude",
		Status:        coreauth.StatusError,
		StatusMessage: "reauth_required",
		LastError: &coreauth.Error{
			Code:    "reauth_required",
			Message: "refresh token is no longer valid; sign in again to reconnect this account",
		},
		Metadata: map[string]any{
			"type":  "claude",
			"email": "user@example.com",
			// operator-controlled, must still be inherited across reauth
			"note": "managed account",
			// automatic lock written by markRefreshReauthRequiredWithReason; must
			// be fully cleared by a completed reauth, not carried forward
			"refresh_disabled":        true,
			"refresh_status":          "reauth_required",
			"refresh_error_code":      "invalid_grant",
			"refresh_disabled_reason": "reauth_required",
			"reauth_required":         true,
			"refresh_disabled_at":     "2026-07-01T00:00:00Z",
			"last_refresh_error":      "refresh token is no longer valid; sign in again to reconnect this account",
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"access_token":  "NEW_TOKEN",
			"refresh_token": "NEW_REFRESH",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	// Operator metadata still inherited.
	if got, _ := record.Metadata["note"].(string); got != "managed account" {
		t.Fatalf("note = %q, want inherited %q", got, "managed account")
	}

	// Every automatic lock key must be gone.
	for _, key := range []string{
		"refresh_disabled",
		"refresh_status",
		"refresh_error_code",
		"refresh_disabled_reason",
		"reauth_required",
		"refresh_disabled_at",
		"last_refresh_error",
	} {
		if _, ok := record.Metadata[key]; ok {
			t.Fatalf("metadata[%q] survived reauth, want cleared: %#v", key, record.Metadata[key])
		}
	}
	if record.StatusMessage != "" {
		t.Fatalf("StatusMessage = %q, want cleared", record.StatusMessage)
	}
	if record.LastError != nil {
		t.Fatalf("LastError = %#v, want cleared", record.LastError)
	}
	if record.RefreshDisabled() {
		t.Fatalf("RefreshDisabled() = true after reauth, want false (automatic refresh should resume)")
	}
}

// TestSaveTokenRecord_KeepsOperatorDisableAcrossReauth asserts the other side
// of #154's fix boundary: a credential the operator explicitly disabled via
// account_settings.refresh_enabled = false (no automatic lock markers) must
// stay disabled after reauth. clearStaleReauthLockOnSave must not touch it.
func TestSaveTokenRecord_KeepsOperatorDisableAcrossReauth(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":  "claude",
			"email": "user@example.com",
			"note":  "managed account",
			"account_settings": map[string]any{
				"schema_version":  1,
				"refresh_enabled": false,
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), previous); errRegister != nil {
		t.Fatalf("failed to register previous auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "claude-user@example.com.json",
		FileName: "claude-user@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"access_token":  "NEW_TOKEN",
			"refresh_token": "NEW_REFRESH",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	settings, ok := record.Metadata["account_settings"].(map[string]any)
	if !ok {
		t.Fatalf("account_settings missing or wrong type: %T", record.Metadata["account_settings"])
	}
	if got, _ := settings["refresh_enabled"].(bool); got {
		t.Fatalf("account_settings.refresh_enabled = %v, want false (operator disable must survive reauth)", got)
	}
	if !record.RefreshDisabled() {
		t.Fatalf("RefreshDisabled() = false after reauth, want true (operator explicitly disabled refresh)")
	}
}
