package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func writeStubAuthFile(path string) error {
	return os.WriteFile(path, []byte(`{"type":"codex","email":"user@example.com"}`), 0o600)
}

func TestSaveTokenRecord_PreservesUserFieldsOnReauth(t *testing.T) {
	authDir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "codex-user@example.com.json",
		FileName: "codex-user@example.com.json",
		Provider: "codex",
		ProxyURL: "http://proxy.local:7897",
		Attributes: map[string]string{
			"path":                   filepath.Join(authDir, "codex-user@example.com.json"),
			"header:Accept-Language": "en-US",
			"note":                   "managed account",
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
				"Accept-Language": "en-US",
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

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "codex-user@example.com.json",
		FileName: "codex-user@example.com.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":       "codex",
			"email":      "user@example.com",
			"account_id": "acct-1",
		},
	}
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	// Check persisted metadata and runtime restoration through an independent cold load.
	cold := coreauth.NewManager(store, nil, nil)
	if err := cold.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, ok := cold.GetByID(record.ID)
	if !ok {
		t.Fatal("updated credential missing after reload")
	}
	record = restored

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
	if got := headers["Accept-Language"]; got != "en-US" {
		t.Fatalf("headers.Accept-Language = %v, want %q", got, "en-US")
	}
	settings, ok := record.Metadata["account_settings"].(map[string]any)
	if !ok {
		t.Fatalf("account_settings missing or wrong type: %T", record.Metadata["account_settings"])
	}
	if got, _ := settings["refresh_enabled"].(bool); got {
		t.Fatalf("account_settings.refresh_enabled = %v, want false", got)
	}
	if got := record.Attributes["header:Accept-Language"]; got != "en-US" {
		t.Fatalf("attributes.header:Accept-Language = %q, want %q", got, "en-US")
	}
	if got := ordinaryAccountNote(record); got != "managed account" {
		t.Fatalf("management note = %q, want %q", got, "managed account")
	}
}

func TestSaveTokenRecord_DoesNotLeakOldMetadataAcrossAccounts(t *testing.T) {
	authDir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	previous := &coreauth.Auth{
		ID:       "codex-someone-else@example.com.json",
		FileName: "codex-someone-else@example.com.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "codex-someone-else@example.com.json"),
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

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	record := &coreauth.Auth{
		ID:       "codex-fresh@example.com.json",
		FileName: "codex-fresh@example.com.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":       "codex",
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

func TestSaveTokenRecord_KeepsOperatorDisableAcrossReauth(t *testing.T) {
	authDir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
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

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
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
