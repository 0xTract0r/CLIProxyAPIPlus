package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestBuildAuthFileEntry_ReauthURL covers the #163 additive reauth_url field:
// it must appear only for a Claude auth record that carries the automatic
// terminal reauth-required lock (coreauth.IsReauthRequiredMetadata), pointing
// at the exact existing GET /v0/management/anthropic-auth-url?auth_name=<id>
// endpoint (reusing coreauth.ReauthAlertURL, not a hand-rolled URL), and must
// stay absent for a healthy Claude auth, a non-Claude auth under the same
// metadata shape, and an operator's explicit refresh-disable (which never
// sets the automatic lock markers).
func TestBuildAuthFileEntry_ReauthURL(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	t.Run("locked claude auth exposes reauth_url", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-locked-1",
			Provider:   "claude",
			Status:     coreauth.StatusError,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"reauth_required":         true,
				"refresh_status":          "reauth_required",
				"refresh_error_code":      "invalid_grant",
				"refresh_disabled_reason": "reauth_required",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		got, _ := entry["reauth_url"].(string)
		want := coreauth.ReauthAlertURL(auth.ID)
		if got == "" {
			t.Fatal("entry[\"reauth_url\"] missing for a locked claude auth")
		}
		if got != want {
			t.Fatalf("entry[\"reauth_url\"] = %q, want %q (must match coreauth.ReauthAlertURL)", got, want)
		}
	})

	t.Run("healthy claude auth has no reauth_url", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-healthy-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["reauth_url"]; ok {
			t.Fatalf("entry[\"reauth_url\"] = %v, want field absent for a healthy auth", entry["reauth_url"])
		}
	})

	t.Run("locked non-claude auth has no reauth_url (no auth-scoped endpoint for it)", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "codex-locked-1",
			Provider:   "codex",
			Status:     coreauth.StatusError,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"reauth_required": true,
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["reauth_url"]; ok {
			t.Fatalf("entry[\"reauth_url\"] = %v, want field absent for non-claude provider", entry["reauth_url"])
		}
	})

	t.Run("operator-disabled claude auth has no reauth_url (not the automatic lock)", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-operator-disabled-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"account_settings": map[string]any{
					"refresh_enabled": false,
				},
			},
		}
		if coreauth.IsReauthRequiredMetadata(auth.Metadata) {
			t.Fatal("fixture setup: operator disable must not satisfy IsReauthRequiredMetadata")
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["reauth_url"]; ok {
			t.Fatalf("entry[\"reauth_url\"] = %v, want field absent for operator-disabled auth", entry["reauth_url"])
		}
	})
}
