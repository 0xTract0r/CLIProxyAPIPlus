package management

import (
	"context"
	"net/http"
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

	t.Run("auto-quarantined claude auth exposes reauth_url too (T3)", func(t *testing.T) {
		// T3 (telemetry-farm-ux-hardening): AutoQuarantined is a separate
		// automatic lock from IsReauthRequiredMetadata (see markAutoQuarantine
		// in conductor.go), but reauth is its recovery path too, so
		// buildAuthFileEntry must surface the same reauth_url for it.
		auth := &coreauth.Auth{
			ID:              "claude-quarantined-1",
			Provider:        "claude",
			Status:          coreauth.StatusQuarantined,
			AutoQuarantined: true,
			UpdatedAt:       time.Now(),
			Attributes:      map[string]string{"runtime_only": "true"},
			Metadata:        map[string]any{},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		got, _ := entry["reauth_url"].(string)
		want := coreauth.ReauthAlertURL(auth.ID)
		if got == "" {
			t.Fatal("entry[\"reauth_url\"] missing for an auto-quarantined claude auth")
		}
		if got != want {
			t.Fatalf("entry[\"reauth_url\"] = %q, want %q", got, want)
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

// TestBuildAuthFileEntry_AutoQuarantinedFields covers the T3
// (telemetry-farm-ux-hardening) external contract: a quarantined claude
// account must expose auto_quarantined=true plus quarantine_reason and
// quarantined_at (T3's whole point is letting the farm-orchestrator
// passthrough and management UI distinguish this from an operator's explicit
// Disabled=true without depending on the exact "status" string), and a
// healthy account must expose auto_quarantined=false with the optional
// detail fields entirely absent.
func TestBuildAuthFileEntry_AutoQuarantinedFields(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	t.Run("quarantined claude auth exposes the full T3 contract", func(t *testing.T) {
		// Route the quarantine fields through the real markAutoQuarantine
		// mutator (via Manager.MarkResult) rather than hand-setting the
		// exported fields, so this test also pins the actual reason code and
		// timestamp format markAutoQuarantine produces. Deliberately does NOT
		// pre-set AutoQuarantined/Status/StatusMessage: markAutoQuarantine's
		// own idempotency guard (!auth.AutoQuarantined) would otherwise skip
		// running and leave QuarantineReason empty.
		auth := &coreauth.Auth{
			ID:         "claude-quarantined-fields",
			Provider:   "claude",
			ProxyURL:   "http://test-proxy:8080",
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{},
		}
		manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
		terminalAuthErr := &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`}
		manager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "claude", Success: false, Error: terminalAuthErr})
		manager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "claude", Success: false, Error: terminalAuthErr})
		quarantined, ok := manager.GetByID(auth.ID)
		if !ok || quarantined == nil || !quarantined.AutoQuarantined {
			t.Fatalf("precondition failed: auth not quarantined, got=%+v ok=%v", quarantined, ok)
		}

		entry := h.buildAuthFileEntry(quarantined)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if got, ok := entry["auto_quarantined"].(bool); !ok || !got {
			t.Fatalf("entry[\"auto_quarantined\"] = %#v, want true", entry["auto_quarantined"])
		}
		reason, ok := entry["quarantine_reason"].(string)
		if !ok || reason == "" {
			t.Fatalf("entry[\"quarantine_reason\"] = %#v, want a non-empty reason", entry["quarantine_reason"])
		}
		if quarantinedAtStr, ok := entry["quarantined_at"].(string); !ok || quarantinedAtStr == "" {
			t.Fatalf("entry[\"quarantined_at\"] = %#v, want a non-empty RFC3339 timestamp", entry["quarantined_at"])
		}
		reauthURL, ok := entry["reauth_url"].(string)
		if !ok || reauthURL == "" {
			t.Fatalf("entry[\"reauth_url\"] = %#v, want a non-empty reauth link", entry["reauth_url"])
		}
	})

	t.Run("healthy claude auth exposes auto_quarantined=false without detail fields", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-healthy-fields",
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
		if got, ok := entry["auto_quarantined"].(bool); !ok || got {
			t.Fatalf("entry[\"auto_quarantined\"] = %#v, want false", entry["auto_quarantined"])
		}
		if _, ok := entry["quarantine_reason"]; ok {
			t.Fatalf("entry[\"quarantine_reason\"] = %v, want field absent for a healthy auth", entry["quarantine_reason"])
		}
		if _, ok := entry["quarantined_at"]; ok {
			t.Fatalf("entry[\"quarantined_at\"] = %v, want field absent for a healthy auth", entry["quarantined_at"])
		}
	})
}
