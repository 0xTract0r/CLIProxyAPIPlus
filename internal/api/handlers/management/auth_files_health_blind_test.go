package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestBuildAuthFileEntry_FarmHealthBlind covers the B1
// (farm-account-liveness-detection) additive top-level projection of the
// persisted "health-blind" signal. buildAuthFileEntry must surface the
// Metadata[farm_health_blind] flag (written as a bool by stampQuotaHealthBlind
// while FARM_LIVENESS_DETECTION_ENABLED is armed) plus its companion
// farm_health_blind_at timestamp onto the /v0/management/auth-files entry JSON,
// so the umbrella farm-orchestrator passthrough can read the health-blind state
// directly instead of it being trapped Metadata-only.
//
// Contract:
//   - farm_health_blind is ALWAYS present as a bool (mirrors farm_enrolled being
//     an unconditionally-written top-level boolean); false when detection is off
//     or the flag is unset (safe no-op).
//   - farm_health_blind_at is omitempty and follows the same non-empty/non-zero
//     gate as refresh_disabled_at: emitted only when the metadata key exists and
//     the string is non-empty and not a Go zero time ("0001-01-01T00:00:00Z").
//   - the projection READS the persisted flag; it must not recompute via the
//     serving-independent provisioned gate.
func TestBuildAuthFileEntry_FarmHealthBlind(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	t.Run("health-blind auth exposes the flag and first-observed timestamp", func(t *testing.T) {
		// Drive the metadata exactly as stampQuotaHealthBlind persists it: a bool
		// flag plus an RFC3339 UTC "first observed" timestamp.
		observedAt := time.Date(2026, 9, 4, 8, 15, 30, 0, time.UTC).Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-health-blind-1",
			Provider:   "claude",
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				farmHealthBlindMetadataKey:   true,
				farmHealthBlindAtMetadataKey: observedAt,
			},
		}

		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		got, ok := entry["farm_health_blind"].(bool)
		if !ok {
			t.Fatalf("entry[\"farm_health_blind\"] = %#v, want a bool", entry["farm_health_blind"])
		}
		if !got {
			t.Fatalf("entry[\"farm_health_blind\"] = false, want true for a health-blind auth")
		}
		gotAt, ok := entry["farm_health_blind_at"].(string)
		if !ok || gotAt == "" {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %#v, want a non-empty RFC3339 timestamp", entry["farm_health_blind_at"])
		}
		if gotAt != observedAt {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %q, want %q (must mirror Metadata verbatim)", gotAt, observedAt)
		}
	})

	t.Run("string flag value is normalized to true", func(t *testing.T) {
		// Persistence round-trips (or a hand-edited auth file) may store the flag
		// as a string; mirror farm_enrolled's parseBoolAny tolerance.
		auth := &coreauth.Auth{
			ID:         "claude-health-blind-str-1",
			Provider:   "claude",
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				farmHealthBlindMetadataKey: "true",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		got, ok := entry["farm_health_blind"].(bool)
		if !ok || !got {
			t.Fatalf("entry[\"farm_health_blind\"] = %#v, want true for a string \"true\" flag", entry["farm_health_blind"])
		}
	})

	t.Run("flag unset projects false with no timestamp", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-not-blind-1",
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
		got, ok := entry["farm_health_blind"].(bool)
		if !ok {
			t.Fatalf("entry[\"farm_health_blind\"] = %#v, want a bool", entry["farm_health_blind"])
		}
		if got {
			t.Fatalf("entry[\"farm_health_blind\"] = true, want false when the flag is unset")
		}
		if _, ok := entry["farm_health_blind_at"]; ok {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %v, want field absent when the flag is unset", entry["farm_health_blind_at"])
		}
	})

	t.Run("nil metadata projects false with no timestamp", func(t *testing.T) {
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
		got, ok := entry["farm_health_blind"].(bool)
		if !ok || got {
			t.Fatalf("entry[\"farm_health_blind\"] = %#v, want false for nil metadata", entry["farm_health_blind"])
		}
		if _, ok := entry["farm_health_blind_at"]; ok {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %v, want field absent for nil metadata", entry["farm_health_blind_at"])
		}
	})

	t.Run("RFC3339-encoded Go zero time is dropped", func(t *testing.T) {
		// The string form of a Go zero time is non-empty, so a raw TrimSpace!=""
		// gate would leak it. The projection must mirror the refresh_disabled_at
		// IsZero() gate and drop it while still exposing the bool flag.
		auth := &coreauth.Auth{
			ID:         "claude-health-blind-zero-time-1",
			Provider:   "claude",
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				farmHealthBlindMetadataKey:   true,
				farmHealthBlindAtMetadataKey: "0001-01-01T00:00:00Z",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if got, ok := entry["farm_health_blind"].(bool); !ok || !got {
			t.Fatalf("entry[\"farm_health_blind\"] = %#v, want true (flag still surfaced)", entry["farm_health_blind"])
		}
		if _, ok := entry["farm_health_blind_at"]; ok {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %v, want field absent for a Go zero time", entry["farm_health_blind_at"])
		}
	})

	t.Run("empty/whitespace dirty timestamp is dropped", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-health-blind-dirty-1",
			Provider:   "claude",
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				farmHealthBlindMetadataKey:   true,
				farmHealthBlindAtMetadataKey: "   ",
			},
		}
		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		if _, ok := entry["farm_health_blind_at"]; ok {
			t.Fatalf("entry[\"farm_health_blind_at\"] = %v, want field absent for an empty/whitespace value", entry["farm_health_blind_at"])
		}
	})
}
