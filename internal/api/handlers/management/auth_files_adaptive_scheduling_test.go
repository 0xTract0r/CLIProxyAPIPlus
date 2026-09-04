package management

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	usagepkg "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestBuildAuthFileEntry_AdaptiveScheduling covers tasks.md 5.2
// (add-adaptive-account-scheduling): buildAuthFileEntry must additively project
// the Phase 0 scheduling primitives (fine-grained subscription tier, structured
// quota utilization/headroom, first_production_at anchor, current warm-up +
// rate-limit stage) under entry["account_scheduling"], reading only
// already-persisted record data and surfacing "unknown" state explicitly
// (JSON null / "unknown" label) rather than coercing it to a guessed value.
func TestBuildAuthFileEntry_AdaptiveScheduling(t *testing.T) {
	h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}

	t.Run("claude auth with tier, quota snapshot and anchor exposes all fields", func(t *testing.T) {
		anchor := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-adaptive-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"first_production_at": anchor,
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "default_claude_max_20x",
						},
					},
					"usage": map[string]any{
						"five_hour": map[string]any{"utilization": 90.0},
						"seven_day": map[string]any{"utilization": 20.0},
					},
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}

		if got := view["subscription_tier"]; got != "max_20x" {
			t.Fatalf("subscription_tier = %#v, want %q", got, "max_20x")
		}

		// §8.4: auto-detected tier (no tier_override) -> tier_source "auto".
		if got := view["tier_source"]; got != "auto" {
			t.Fatalf("tier_source = %#v, want %q for an auto-detected tier", got, "auto")
		}
		// §8.3: default rate_scale (no override, default config) -> 1.0.
		if got, ok := view["rate_scale"].(float64); !ok || math.Abs(got-1.0) > 1e-9 {
			t.Fatalf("rate_scale = %#v, want 1.0", view["rate_scale"])
		}

		if got, gotOK := view["first_production_at"].(string); !gotOK || got != anchor {
			t.Fatalf("first_production_at = %#v, want %q", view["first_production_at"], anchor)
		}

		quota, quotaOK := view["quota_utilization"].(gin.H)
		if !quotaOK {
			t.Fatalf("quota_utilization = %#v, want a structured object", view["quota_utilization"])
		}
		windows, windowsOK := quota["windows"].(map[string]gin.H)
		if !windowsOK {
			t.Fatalf("quota_utilization.windows = %#v, want map[string]gin.H", quota["windows"])
		}
		if _, ok := windows["five_hour"]; !ok {
			t.Fatalf("quota_utilization.windows missing five_hour: %#v", windows)
		}
		if _, ok := windows["seven_day"]; !ok {
			t.Fatalf("quota_utilization.windows missing seven_day: %#v", windows)
		}
		binding, bindingOK := quota["binding_window"].(gin.H)
		if !bindingOK {
			t.Fatalf("quota_utilization.binding_window = %#v, want gin.H", quota["binding_window"])
		}
		if got := binding["window"]; got != "five_hour" {
			t.Fatalf("binding_window.window = %#v, want %q (tightest window)", got, "five_hour")
		}
		if got, ok := binding["headroom"].(float64); !ok || math.Abs(got-0.1) > 1e-6 {
			t.Fatalf("binding_window.headroom = %#v, want ~0.1", binding["headroom"])
		}

		warmup, warmupOK := view["warmup"].(gin.H)
		if !warmupOK {
			t.Fatalf("warmup = %#v, want gin.H", view["warmup"])
		}
		if got := warmup["stage"]; got != "w2" {
			t.Fatalf("warmup.stage = %#v, want %q for a 10-day-old account on the default curve", got, "w2")
		}
		if got, ok := warmup["mature"].(bool); !ok || got {
			t.Fatalf("warmup.mature = %#v, want false", warmup["mature"])
		}
		if got, ok := warmup["age_days"].(int); !ok || got != 10 {
			t.Fatalf("warmup.age_days = %#v, want 10", warmup["age_days"])
		}
		if _, ok := warmup["rpm_limit"]; !ok {
			t.Fatalf("warmup missing rpm_limit: %#v", warmup)
		}
		if _, ok := warmup["daily_budget"]; !ok {
			t.Fatalf("warmup missing daily_budget: %#v", warmup)
		}
	})

	t.Run("un-anchored claude auth surfaces unknown/null, never guessed", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-adaptive-cold-1",
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
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}

		if got := view["subscription_tier"]; got != "unknown" {
			t.Fatalf("subscription_tier = %#v, want %q", got, "unknown")
		}
		// The key must be present with an explicit nil value (JSON null), not
		// absent and not a zero-headroom object.
		val, present := view["quota_utilization"]
		if !present {
			t.Fatal("quota_utilization key absent, want present with null value")
		}
		if val != nil {
			t.Fatalf("quota_utilization = %#v, want nil (unknown must not read as full headroom)", val)
		}
		fpVal, fpPresent := view["first_production_at"]
		if !fpPresent {
			t.Fatal("first_production_at key absent, want present with null value")
		}
		if fpVal != nil {
			t.Fatalf("first_production_at = %#v, want nil for an un-anchored account", fpVal)
		}

		warmup, warmupOK := view["warmup"].(gin.H)
		if !warmupOK {
			t.Fatalf("warmup = %#v, want gin.H", view["warmup"])
		}
		if got := warmup["stage"]; got != "cold" {
			t.Fatalf("warmup.stage = %#v, want %q for an un-anchored account", got, "cold")
		}
		if got, ok := warmup["age_days"]; !ok || got != nil {
			t.Fatalf("warmup.age_days = %#v, want present-and-nil for an un-anchored account", warmup["age_days"])
		}
	})

	t.Run("codex auth reports its own plan tier", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "codex-adaptive-1",
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true", "plan_type": "pro"},
			Metadata:   map[string]any{},
		}

		entry := h.buildAuthFileEntry(auth)
		if entry == nil {
			t.Fatal("buildAuthFileEntry() = nil, want an entry")
		}
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}
		if got := view["subscription_tier"]; got != "pro" {
			t.Fatalf("subscription_tier = %#v, want %q for a codex pro account", got, "pro")
		}
		if got := view["tier_source"]; got != "auto" {
			t.Fatalf("tier_source = %#v, want %q for an auto-detected codex tier", got, "auto")
		}
	})

	t.Run("namespaced tier_override and rate_scale surface as override and scaled value", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-adaptive-override-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				// No rate_limit_tier at all: the shown tier comes purely from the
				// manual override, so tier_source must read "override".
				coreauth.AccountSchedulingMetadataKey: map[string]any{
					"tier_override": "max_5x",
					"rate_scale":    0.5,
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}
		if got := view["subscription_tier"]; got != "max_5x" {
			t.Fatalf("subscription_tier = %#v, want %q (from tier_override)", got, "max_5x")
		}
		if got := view["tier_source"]; got != "override" {
			t.Fatalf("tier_source = %#v, want %q for a manual override", got, "override")
		}
		if got, ok := view["rate_scale"].(float64); !ok || math.Abs(got-0.5) > 1e-9 {
			t.Fatalf("rate_scale = %#v, want 0.5 from the per-account override", view["rate_scale"])
		}
	})
}

// TestBuildAuthFileEntry_AdaptiveScheduling_SessionCounts covers the P6
// session-aggregation slice: account_scheduling must additively project
// sessions_total/sessions_active/sessions_closed, sourced from
// internal/usage.SessionAggregateForAuthIndex keyed on this account's
// EnsureIndex(). It also covers the no-usage-store-wired path (existing
// callers that construct a bare Handler{cfg: ...} without usageStats), which
// must report explicit zeros rather than omitting the keys or panicking.
func TestBuildAuthFileEntry_AdaptiveScheduling_SessionCounts(t *testing.T) {
	// buildAccountSchedulingView (production code, called via buildAuthFileEntry
	// below) derives its own "now" internally via time.Now() when bucketing
	// sessions into active/closed -- it is not parameterized. A hardcoded
	// calendar date here would only agree with that internal now on the day it
	// was written, so anchor relative offsets to the actual wall clock instead.
	now := time.Now().UTC()

	t.Run("no usage store wired reports explicit zeros", func(t *testing.T) {
		h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}
		auth := &coreauth.Auth{ID: "claude-sessions-nostat", Provider: "claude", Status: coreauth.StatusActive, UpdatedAt: now, Attributes: map[string]string{"runtime_only": "true"}}

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}
		for _, key := range []string{"sessions_total", "sessions_active", "sessions_closed"} {
			got, present := view[key]
			if !present {
				t.Fatalf("%s absent, want present with 0", key)
			}
			if got != 0 {
				t.Fatalf("%s = %#v, want 0", key, got)
			}
		}
	})

	t.Run("aggregates recorded sessions for this account's AuthIndex only", func(t *testing.T) {
		stats := usagepkg.NewRequestStatistics()
		h := &Handler{
			cfg:        &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()},
			usageStats: stats,
		}
		auth := &coreauth.Auth{ID: "claude-sessions-1", Provider: "claude", Status: coreauth.StatusActive, UpdatedAt: now, Attributes: map[string]string{"runtime_only": "true"}}
		authIndex := auth.EnsureIndex()
		if authIndex == "" {
			t.Fatal("auth.EnsureIndex() = \"\", want a non-empty index to key session aggregation on")
		}

		record := func(idx, sessionID string, at time.Time) {
			ctx := coreauth.WithSessionID(context.Background(), sessionID)
			stats.Record(ctx, coreusage.Record{
				APIKey:      "test-key",
				Model:       "gpt-5.4",
				AuthIndex:   idx,
				RequestedAt: at,
				Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			})
		}

		// This account: one recently-active session, one long-idle (closed) one.
		record(authIndex, "s-active", now.Add(-1*time.Minute))
		record(authIndex, "s-closed", now.Add(-45*time.Minute))
		// A different account's session must not leak into this account's count.
		record("some-other-authindex", "s-other-account", now.Add(-1*time.Minute))

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}
		if got := view["sessions_total"]; got != 2 {
			t.Fatalf("sessions_total = %#v, want 2", got)
		}
		if got := view["sessions_active"]; got != 1 {
			t.Fatalf("sessions_active = %#v, want 1", got)
		}
		if got := view["sessions_closed"]; got != 1 {
			t.Fatalf("sessions_closed = %#v, want 1", got)
		}
	})
}
