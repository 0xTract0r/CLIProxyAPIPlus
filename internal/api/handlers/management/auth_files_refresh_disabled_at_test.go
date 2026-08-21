package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestBuildAuthFileEntry_RefreshDisabledAt covers the R5-1a (telemetry-farm)
// additive refresh_disabled_at field: buildAuthFileEntry must project the
// automatic reauth/ban timestamp that markRefreshReauthRequiredWithReason
// writes into Metadata["refresh_disabled_at"] so the farm-orchestrator
// passthrough and the management UI can render *when* an account was banned
// instead of a hardcoded "—". It must follow the same non-empty/non-zero gate
// as quarantined_at: emitted only when the metadata key exists and the string
// is non-empty and not a Go zero time, and absent for a healthy account, a
// missing key, an empty / whitespace-only dirty value, or the RFC3339-encoded
// Go zero time ("0001-01-01T00:00:00Z").
func TestBuildAuthFileEntry_RefreshDisabledAt(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	t.Run("auto-disabled auth exposes the reauth/ban timestamp from metadata", func(t *testing.T) {
		// Drive the timestamp through the real markRefreshReauthRequiredWithReason
		// mutator (via the exported MarkRefreshReauthRequired) rather than
		// hand-writing the metadata, so this test also pins the exact
		// RFC3339 string shape core actually produces.
		now := time.Date(2026, 7, 1, 12, 34, 56, 0, time.UTC)
		auth := &coreauth.Auth{
			ID:         "claude-banned-1",
			Provider:   "claude",
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{},
		}
		auth.MarkRefreshReauthRequired(now)

		want, _ := auth.Metadata["refresh_disabled_at"].(string)
		if want == "" {
			t.Fatal("fixture setup: MarkRefreshReauthRequired did not write refresh_disabled_at")
		}

		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		got, ok := entry["refresh_disabled_at"].(string)
		if !ok || got == "" {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %#v, want a non-empty RFC3339 timestamp", entry["refresh_disabled_at"])
		}
		if got != want {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %q, want %q (must mirror Metadata verbatim)", got, want)
		}
	})

	t.Run("healthy auth has no refresh_disabled_at", func(t *testing.T) {
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
		if _, ok := entry["refresh_disabled_at"]; ok {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %v, want field absent for a healthy auth", entry["refresh_disabled_at"])
		}
	})

	t.Run("nil metadata has no refresh_disabled_at", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-nil-meta-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["refresh_disabled_at"]; ok {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %v, want field absent for nil metadata", entry["refresh_disabled_at"])
		}
	})

	t.Run("empty/whitespace dirty value is dropped", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-dirty-1",
			Provider:   "claude",
			Status:     coreauth.StatusError,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"refresh_disabled_at": "   ",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["refresh_disabled_at"]; ok {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %v, want field absent for an empty/whitespace value", entry["refresh_disabled_at"])
		}
	})

	t.Run("RFC3339-encoded Go zero time is dropped", func(t *testing.T) {
		// The string form of a Go zero time is non-empty, so the raw
		// TrimSpace!="" gate would leak it. buildAuthFileEntry must mirror the
		// quarantined_at IsZero() gate and drop it, matching the docstring/inline
		// comment claim that a Go zero time ("0001-01-01T00:00:00Z") is never
		// surfaced.
		auth := &coreauth.Auth{
			ID:         "claude-zero-time-1",
			Provider:   "claude",
			Status:     coreauth.StatusError,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"refresh_disabled_at": "0001-01-01T00:00:00Z",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["refresh_disabled_at"]; ok {
			t.Fatalf("entry[\"refresh_disabled_at\"] = %v, want field absent for a Go zero time", entry["refresh_disabled_at"])
		}
	})
}
