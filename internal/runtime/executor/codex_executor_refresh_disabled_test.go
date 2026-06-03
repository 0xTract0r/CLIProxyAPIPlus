package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestCodexExecutorRefresh_SkipsWhenRefreshDisabled asserts that the Codex
// executor short-circuits before reaching the outbound OAuth refresh when the
// auth record has refresh disabled via metadata or account_settings. This
// guards against unaware callers (e.g. retry-on-401 paths) bypassing the
// auto-refresh scheduler.
func TestCodexExecutorRefresh_SkipsWhenRefreshDisabled(t *testing.T) {
	exec := &CodexExecutor{cfg: &config.Config{}}
	auth := &cliproxyauth.Auth{
		ID:       "codex-refresh-disabled.json",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token":    "not-a-real-token",
			"refresh_disabled": true,
		},
	}
	updated, err := exec.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("expected no error when refresh disabled, got %v", err)
	}
	if updated != auth {
		t.Fatalf("expected the same auth pointer back when refresh skipped")
	}

	authViaSettings := &cliproxyauth.Auth{
		ID:       "codex-refresh-disabled-via-settings.json",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "not-a-real-token",
			"account_settings": map[string]any{
				"refresh_enabled": false,
			},
		},
	}
	updated, err = exec.Refresh(context.Background(), authViaSettings)
	if err != nil {
		t.Fatalf("expected no error when account_settings.refresh_enabled=false, got %v", err)
	}
	if updated != authViaSettings {
		t.Fatalf("expected the same auth pointer back when refresh skipped via account_settings")
	}
}

func TestRefreshFailureLogFieldsUsesAccountRemark(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-user@example.com-plus.json",
		FileName: "codex-user@example.com-plus.json",
		Provider: "codex",
		Label:    "user@example.com",
		Attributes: map[string]string{
			"note": "Codex A01",
		},
		Metadata: map[string]any{
			"note": "metadata note should not win",
		},
	}

	fields := refreshFailureLogFields(auth)
	if fields["account_remark"] != "Codex A01" {
		t.Fatalf("account_remark = %#v, want Codex A01", fields["account_remark"])
	}
	if fields["provider"] != "codex" {
		t.Fatalf("provider = %#v, want codex", fields["provider"])
	}
	if fields["auth_id"] == "" || fields["auth_file"] == "" {
		t.Fatalf("expected auth_id/auth_file to remain available for local correlation: %#v", fields)
	}
}
