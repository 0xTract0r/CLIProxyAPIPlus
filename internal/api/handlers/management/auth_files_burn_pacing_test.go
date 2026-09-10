package management

import (
	"math"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestBuildAuthFileEntry_BurnPacingObservability covers the harden-account-
// scheduling-limiter OBS projection: buildAccountSchedulingView must additively
// project burn_rate_per_hour / projected_exhaustion_at / pacing_factor_dryrun,
// following the sibling "unknown is null" contract (null, not 0, when there is no
// burn history) and never gating.
func TestBuildAuthFileEntry_BurnPacingObservability(t *testing.T) {
	h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}

	t.Run("no burn history surfaces the three fields as null", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-burn-null",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"usage": map[string]any{
						"seven_day": map[string]any{"utilization": 60.0},
					},
				},
			},
		}
		view := schedulingView(t, h, auth)
		for _, key := range []string{"burn_rate_per_hour", "projected_exhaustion_at", "pacing_factor_dryrun"} {
			if got, present := view[key]; !present || got != nil {
				t.Fatalf("view[%q] = %#v (present=%v), want explicit nil", key, got, present)
			}
		}
	})

	t.Run("persisted burn state projects rate, projection and dry-run pacing", func(t *testing.T) {
		resetsAt := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
		projected := time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-burn-value",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"usage": map[string]any{
						// headroom 0.4 => 40 remaining points; reset in ~4h => fair ~10/h.
						"seven_day": map[string]any{"utilization": 60.0, "resets_at": resetsAt},
					},
				},
				"account_scheduling": map[string]any{
					"burn_rate_ewma_per_hour":      20.0,
					"burn_prev_window":             "seven_day",
					"burn_prev_util_percent":       60.0,
					"burn_projected_exhaustion_at": projected,
				},
			},
		}
		view := schedulingView(t, h, auth)

		if got, ok := view["burn_rate_per_hour"].(float64); !ok || !approxEqualF(got, 20.0, 1e-9) {
			t.Fatalf("burn_rate_per_hour = %#v, want 20.0", view["burn_rate_per_hour"])
		}
		if got, ok := view["projected_exhaustion_at"].(string); !ok || got != projected {
			t.Fatalf("projected_exhaustion_at = %#v, want %q", view["projected_exhaustion_at"], projected)
		}
		// fair ~10 / burn 20 = ~0.5, dry-run only (never gating).
		if got, ok := view["pacing_factor_dryrun"].(float64); !ok || !approxEqualF(got, 0.5, 0.05) {
			t.Fatalf("pacing_factor_dryrun = %#v, want ~0.5", view["pacing_factor_dryrun"])
		}
	})
}

func schedulingView(t *testing.T, h *Handler, auth *coreauth.Auth) gin.H {
	t.Helper()
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("buildAuthFileEntry() = nil")
	}
	view, ok := entry["account_scheduling"].(gin.H)
	if !ok {
		t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
	}
	return view
}

func approxEqualF(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}
