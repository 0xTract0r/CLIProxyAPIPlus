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

	t.Run("codex auth carries no account_scheduling projection (claude-only)", func(t *testing.T) {
		// Requirement change: the account-scheduling projection is claude-only
		// (mirroring the serving-side codex->0 AccountTierBaseWeight). A non-Claude
		// account must carry neither the current key nor the legacy dual-emit name,
		// so nothing downstream renders a bogus subscription_tier="unknown" / warm-up
		// state on a codex/grok/gemini card.
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
		if _, ok := entry["account_scheduling"].(gin.H); ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want absent for a non-claude account", entry["account_scheduling"])
		}
		if _, present := entry["account_scheduling"]; present {
			t.Fatalf("entry[\"account_scheduling\"] present = %#v, want absent for a non-claude account", entry["account_scheduling"])
		}
		if _, present := entry["adaptive_scheduling"]; present {
			t.Fatalf("entry[\"adaptive_scheduling\"] present = %#v, want absent for a non-claude account", entry["adaptive_scheduling"])
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

// TestBuildAuthFileEntry_AdaptiveScheduling_HealthGate covers the ANCHOR-Q4
// (design §10.5/§10.7) projection: account_scheduling must additively surface the
// health-gated warm-up ramp state -- in_distress plus the warmup_health_stage_cap /
// warmup_last_distress_at pair -- following the same "unknown is not a number"
// contract as the blocks above. An account that has never shown distress reports
// in_distress=false and an explicit null cap / null last-distress (present keys, not
// absent, and never coerced to 0 / "just now"). A distressed, health-capped account
// reports in_distress=true, an integer cap, and an RFC3339 last-distress string.
func TestBuildAuthFileEntry_AdaptiveScheduling_HealthGate(t *testing.T) {
	h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}
	anchor := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("healthy account reports false / null health-gate fields", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-healthgate-healthy",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{"first_production_at": anchor},
		}

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}

		if got, ok := view["in_distress"].(bool); !ok || got {
			t.Fatalf("in_distress = %#v, want false", view["in_distress"])
		}
		capVal, capPresent := view["warmup_health_stage_cap"]
		if !capPresent {
			t.Fatal("warmup_health_stage_cap key absent, want present with null value")
		}
		if capVal != nil {
			t.Fatalf("warmup_health_stage_cap = %#v, want nil for an account with no cap", capVal)
		}
		ldVal, ldPresent := view["warmup_last_distress_at"]
		if !ldPresent {
			t.Fatal("warmup_last_distress_at key absent, want present with null value")
		}
		if ldVal != nil {
			t.Fatalf("warmup_last_distress_at = %#v, want nil for an account that never showed distress", ldVal)
		}
	})

	t.Run("distressed capped account reports true / typed health-gate fields", func(t *testing.T) {
		lastDistress := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-healthgate-distressed",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  time.Now(),
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"first_production_at": anchor,
				// Persisted health-gate state lives inside the namespaced
				// account_scheduling object (design §10.5), the same sub-keys the
				// core writers use.
				coreauth.AccountSchedulingMetadataKey: map[string]any{
					"warmup_health_stage_cap": 2,
					"warmup_last_distress_at": lastDistress,
				},
			},
		}
		// BackoffLevel >= the default threshold (1) trips the in_distress signal.
		auth.Quota.BackoffLevel = 2

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}

		if got, ok := view["in_distress"].(bool); !ok || !got {
			t.Fatalf("in_distress = %#v, want true (BackoffLevel 2 >= threshold 1)", view["in_distress"])
		}
		if got, ok := view["warmup_health_stage_cap"].(int); !ok || got != 2 {
			t.Fatalf("warmup_health_stage_cap = %#v, want int 2", view["warmup_health_stage_cap"])
		}
		if got, ok := view["warmup_last_distress_at"].(string); !ok || got != lastDistress {
			t.Fatalf("warmup_last_distress_at = %#v, want %q", view["warmup_last_distress_at"], lastDistress)
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

// TestBuildAuthFileEntry_AdaptiveScheduling_AnchorCandidates covers the additive,
// read-only anchor_candidates projection: account_scheduling must surface the two
// candidate timestamps the frontend offers as one-click picks for the
// first_production_at anchor -- first_auth_at (from
// account_settings.runtime_identity_state.current.created_at) and last_activity_at
// (from claude_device_high_water.last_seen_at). It follows the "omit rather than
// emit a zero/empty time" contract: a field is present only when its source parses
// as a non-zero RFC3339 timestamp, and the whole object is omitted when both
// sources are absent. It mints nothing and never touches first_production_at.
func TestBuildAuthFileEntry_AdaptiveScheduling_AnchorCandidates(t *testing.T) {
	h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}
	now := time.Now().UTC()

	t.Run("both sources present surface exact normalized values", func(t *testing.T) {
		firstAuth := now.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
		lastServed := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-anchor-candidates-1",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"account_settings": map[string]any{
					"runtime_identity_state": map[string]any{
						"current": map[string]any{
							"created_at": firstAuth,
						},
					},
				},
				coreauth.ClaudeDeviceHighWaterMetadataKey: map[string]any{
					"user_agent":   "claude-cli/2.1.211 (external, cli)",
					"last_seen_at": lastServed,
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		view, ok := entry["account_scheduling"].(gin.H)
		if !ok {
			t.Fatalf("entry[\"account_scheduling\"] = %#v, want gin.H", entry["account_scheduling"])
		}
		candidates, ok := view["anchor_candidates"].(gin.H)
		if !ok {
			t.Fatalf("anchor_candidates = %#v, want gin.H", view["anchor_candidates"])
		}
		if got := candidates["first_auth_at"]; got != firstAuth {
			t.Fatalf("anchor_candidates.first_auth_at = %#v, want %q (runtime_identity_state.current.created_at)", got, firstAuth)
		}
		if got := candidates["last_activity_at"]; got != lastServed {
			t.Fatalf("anchor_candidates.last_activity_at = %#v, want %q (claude_device_high_water.last_seen_at)", got, lastServed)
		}
	})

	t.Run("only first_auth_at present omits last_activity_at", func(t *testing.T) {
		firstAuth := now.Add(-15 * 24 * time.Hour).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-anchor-candidates-firstonly",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				"account_settings": map[string]any{
					"runtime_identity_state": map[string]any{
						"current": map[string]any{
							"created_at": firstAuth,
						},
					},
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		view := entry["account_scheduling"].(gin.H)
		candidates, ok := view["anchor_candidates"].(gin.H)
		if !ok {
			t.Fatalf("anchor_candidates = %#v, want gin.H", view["anchor_candidates"])
		}
		if got := candidates["first_auth_at"]; got != firstAuth {
			t.Fatalf("anchor_candidates.first_auth_at = %#v, want %q", got, firstAuth)
		}
		if _, present := candidates["last_activity_at"]; present {
			t.Fatalf("anchor_candidates.last_activity_at present = %#v, want omitted when high-water is absent", candidates["last_activity_at"])
		}
	})

	t.Run("only last_activity_at present omits first_auth_at", func(t *testing.T) {
		lastServed := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
		auth := &coreauth.Auth{
			ID:         "claude-anchor-candidates-lastonly",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata: map[string]any{
				coreauth.ClaudeDeviceHighWaterMetadataKey: map[string]any{
					"user_agent":   "claude-cli/2.1.211 (external, cli)",
					"last_seen_at": lastServed,
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		view := entry["account_scheduling"].(gin.H)
		candidates, ok := view["anchor_candidates"].(gin.H)
		if !ok {
			t.Fatalf("anchor_candidates = %#v, want gin.H", view["anchor_candidates"])
		}
		if got := candidates["last_activity_at"]; got != lastServed {
			t.Fatalf("anchor_candidates.last_activity_at = %#v, want %q", got, lastServed)
		}
		if _, present := candidates["first_auth_at"]; present {
			t.Fatalf("anchor_candidates.first_auth_at present = %#v, want omitted when runtime identity created_at is absent", candidates["first_auth_at"])
		}
	})

	t.Run("no sources omit the whole anchor_candidates object", func(t *testing.T) {
		auth := &coreauth.Auth{
			ID:         "claude-anchor-candidates-none",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   map[string]any{},
		}

		entry := h.buildAuthFileEntry(auth)
		view := entry["account_scheduling"].(gin.H)
		if _, present := view["anchor_candidates"]; present {
			t.Fatalf("anchor_candidates present = %#v, want omitted when neither source exists", view["anchor_candidates"])
		}
	})

	t.Run("codex auth carries no anchor_candidates (claude-only gate)", func(t *testing.T) {
		// The whole account_scheduling projection (and therefore anchor_candidates)
		// is claude-only; a codex account, even with a runtime_identity_state and a
		// device high-water present, must not surface account_scheduling at all.
		auth := &coreauth.Auth{
			ID:         "codex-anchor-candidates-1",
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			UpdatedAt:  now,
			Attributes: map[string]string{"runtime_only": "true", "plan_type": "pro"},
			Metadata: map[string]any{
				"account_settings": map[string]any{
					"runtime_identity_state": map[string]any{
						"current": map[string]any{
							"created_at": now.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
						},
					},
				},
			},
		}

		entry := h.buildAuthFileEntry(auth)
		if _, present := entry["account_scheduling"]; present {
			t.Fatalf("entry[\"account_scheduling\"] present = %#v, want absent for a non-claude account", entry["account_scheduling"])
		}
	})
}
