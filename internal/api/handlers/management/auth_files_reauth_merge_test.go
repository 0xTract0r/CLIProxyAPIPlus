package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
