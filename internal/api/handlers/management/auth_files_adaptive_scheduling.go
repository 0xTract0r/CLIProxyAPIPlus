package management

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// SessionActiveWindow / SessionClosedAfter are the default idle-time
// thresholds for the P6 session-count aggregation projected below
// (sessions_active/sessions_closed). They are package vars rather than
// consts so a future config-wiring slice can source them from
// config.AccountSchedulingConfig without an API change -- this NOCLASH slice
// intentionally does not add new fields to internal/config.
var (
	SessionActiveWindow = 10 * time.Minute
	SessionClosedAfter  = 30 * time.Minute
)

// buildAdaptiveSchedulingView projects the Phase 0 adaptive-account-scheduling
// primitives (openspec/changes/add-adaptive-account-scheduling, tasks.md 5.2)
// onto the account-list response as a single additive, namespaced sub-object.
// It is read-only: every value is derived from data already persisted on the
// auth record (Metadata quota_snapshot / first_production_at anchor, Attributes
// plan_type) plus the configured warm-up curve — it never mints an anchor,
// never mutates auth, and never triggers an upstream fetch.
//
// The projection deliberately mirrors the "unknown is not a number" contract of
// the underlying primitives: a missing subscription tier, an unreadable quota
// snapshot, or an un-anchored account are surfaced as an explicit "unknown"
// label / JSON null, never silently coerced into "pro" / "0% utilized" /
// "born just now". Callers (the management UI, the farm-orchestrator
// passthrough) can therefore distinguish "we don't know yet" from a real value.
func (h *Handler) buildAdaptiveSchedulingView(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	now := time.Now()
	view := gin.H{}

	// Fine-grained subscription tier, dispatched on provider. Claude's Max
	// 5x/20x split and Codex's pro/plus are intentionally separate enums (see
	// account_tier.go) and are never mixed; a provider that is neither, or an
	// unrecognized tier string, resolves to the "unknown" label rather than a
	// guessed tier.
	switch providerKey(auth) {
	case "claude":
		view["subscription_tier"] = auth.ClaudeSubscriptionTier().String()
	case "codex":
		view["subscription_tier"] = auth.CodexSubscriptionTier().String()
	default:
		view["subscription_tier"] = coreauth.ClaudeTierUnknown.String()
	}

	// Structured per-window quota utilization / headroom, plus the single
	// binding (tightest) window. ok=false means "no usable quota snapshot at
	// all" — surfaced as an explicit null, never as full headroom.
	if utilization, ok := coreauth.ParseAccountQuotaUtilization(auth); ok {
		windows := make(map[string]gin.H, len(utilization.Windows))
		for name, window := range utilization.Windows {
			windowView := gin.H{
				"utilization_percent": window.UtilizationPercent,
				"headroom":            window.Headroom(),
			}
			if !window.ResetsAt.IsZero() {
				windowView["resets_at"] = window.ResetsAt.UTC().Format(time.RFC3339)
			}
			windows[name] = windowView
		}
		quotaView := gin.H{"windows": windows}
		if headroom, hok := coreauth.AccountQuotaHeadroom(auth); hok {
			bindingView := gin.H{
				"window":   headroom.Window,
				"headroom": headroom.Headroom,
			}
			if !headroom.ResetsAt.IsZero() {
				bindingView["resets_at"] = headroom.ResetsAt.UTC().Format(time.RFC3339)
			}
			quotaView["binding_window"] = bindingView
		}
		view["quota_utilization"] = quotaView
	} else {
		view["quota_utilization"] = nil
	}

	// first_production_at freshness anchor. Read-only reader (never mints): an
	// un-anchored account outputs explicit null per the slice brief.
	if anchor, ok := coreauth.AuthFirstProductionAt(auth); ok {
		view["first_production_at"] = anchor.UTC().Format(time.RFC3339)
	} else {
		view["first_production_at"] = nil
	}

	// Current warm-up stage + effective per-account rate-limit stage. Uses the
	// configured warm-up curve and mature ceiling; a nil handler config falls
	// back to a zero-value scheduling config (empty curve -> every account
	// resolves to "mature", matching AccountWarmupStageForAge's contract).
	var scheduling config.AccountSchedulingConfig
	if h != nil && h.cfg != nil {
		scheduling = h.cfg.AccountScheduling
	}
	warmup := coreauth.AccountWarmupStatusFor(auth, now, scheduling)
	warmupView := gin.H{
		"stage":             warmup.StageName,
		"mature":            warmup.Mature,
		"freshness_factor":  warmup.FreshnessFactor,
		"daily_budget":      warmup.DailyBudget,
		"rpm_limit":         warmup.RPMLimit,
		"concurrency_limit": warmup.ConcurrencyLimit,
	}
	if ageDays, ok := coreauth.AccountAgeDays(auth, now); ok {
		warmupView["age_days"] = ageDays
	} else {
		warmupView["age_days"] = nil
	}
	view["warmup"] = warmupView

	// Per-account session count aggregation (P6): distinct SessionID values
	// observed on this account's recorded request details
	// (internal/usage.SessionAggregateForAuthIndex), bucketed by idle time.
	// Read-only and additive like the blocks above. Unlike quota_utilization/
	// first_production_at, "no sessions observed yet" and "no usage store
	// wired" are not distinguished from each other here -- both report 0,
	// since zero recorded sessions is itself a valid, non-"unknown" answer
	// (there is no missing-snapshot ambiguity the way there is for quota).
	sessionsTotal, sessionsActive, sessionsClosed := 0, 0, 0
	if h != nil && h.usageStats != nil {
		if authIndex := strings.TrimSpace(auth.EnsureIndex()); authIndex != "" {
			aggregate := h.usageStats.SessionAggregateForAuthIndex(authIndex, now, SessionActiveWindow, SessionClosedAfter)
			sessionsTotal, sessionsActive, sessionsClosed = aggregate.Total, aggregate.Active, aggregate.Closed
		}
	}
	view["sessions_total"] = sessionsTotal
	view["sessions_active"] = sessionsActive
	view["sessions_closed"] = sessionsClosed

	return view
}
