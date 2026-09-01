package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// This file implements the "unfolded" subscription-tier read path required by
// openspec/changes/add-adaptive-account-scheduling (spec.md "订阅等级自动识别",
// design.md §1.2/§5.2/§6.1). It is a Phase-0 building block for the adaptive
// account-scheduling selector (later phases, not this file): given an *Auth,
// identify the account's fine-grained subscription tier and look up the
// configured base capacity weight for that tier.
//
// IMPORTANT: this is a *parallel* read path, not a replacement. The existing
// folding functions — registry.NormalizeClaudeSubscriptionPlan (used by
// capability gates like Opus eligibility) and the coarse plan_type derivation
// in Auth.SubscriptionPlanType / quota_snapshots.go's inferClaudePlanType —
// are deliberately left untouched. Those callers need "max" vs "pro"; the
// scheduler needs "max_20x" vs "max_5x" vs "pro", which the existing
// Contains(x,"max")-style folding collapses away (design.md §1.2). Both read
// paths coexist by design.

// ClaudeTier identifies a Claude account's subscription tier at the fidelity
// the adaptive scheduler needs. Unlike the coarse "max"/"pro"/"team" plan
// strings produced by the existing folding functions, ClaudeTier distinguishes
// Max 20x from Max 5x, because the two carry very different real capacity
// (design.md §1.2/§5.2: Max 20x ≈ 20x Pro usage, Max 5x ≈ 5x).
type ClaudeTier int

const (
	// ClaudeTierUnknown is returned when the account's rate_limit_tier is
	// missing or does not match a recognized value. Per spec.md's "Claude Max
	// 5x/20x 区分" scenario, a missing/unrecognized field SHALL fall back to
	// an explicitly-flagged unknown tier — it must never be guessed into one
	// of the known tiers.
	ClaudeTierUnknown ClaudeTier = iota
	// ClaudeMax20x is the highest Claude Max capacity tier
	// (rate_limit_tier == "default_claude_max_20x").
	ClaudeMax20x
	// ClaudeMax5x is the mid Claude Max capacity tier
	// (rate_limit_tier == "default_claude_max_5x").
	ClaudeMax5x
	// ClaudePro is the base Claude Pro tier
	// (rate_limit_tier == "default_claude_pro").
	ClaudePro
)

// String returns a stable, lowercase label for the tier. Safe to log; never
// panics on an out-of-range value (falls back to "unknown").
func (t ClaudeTier) String() string {
	switch t {
	case ClaudeMax20x:
		return "max_20x"
	case ClaudeMax5x:
		return "max_5x"
	case ClaudePro:
		return "pro"
	default:
		return "unknown"
	}
}

// claudeRateLimitTierValues maps the raw `rate_limit_tier` string values
// Anthropic's GET /api/oauth/profile returns (production-verified, design.md
// §1.2: bcd898=default_claude_max_20x, grassorich543=default_claude_max_5x)
// to the ClaudeTier enum. Matching is exact (case-insensitive, trimmed) on
// purpose: a fuzzy/Contains-based match risks silently misclassifying a
// future or malformed value into the wrong tier, which spec.md explicitly
// forbids ("SHALL NOT 误判为某一档"). An unrecognized value correctly falls
// through to ClaudeTierUnknown instead. If Anthropic introduces new tier
// strings (e.g. a future team/enterprise-scoped rate_limit_tier), extend this
// table rather than loosening the match (see design.md O4 residual
// uncertainty: 5x string is calibrated off one dormant production account).
var claudeRateLimitTierValues = map[string]ClaudeTier{
	"default_claude_max_20x": ClaudeMax20x,
	"default_claude_max_5x":  ClaudeMax5x,
	"default_claude_pro":     ClaudePro,
}

// ClaudeSubscriptionTier returns a's fine-grained Claude subscription tier,
// read from the already-persisted `Metadata["quota_snapshot"]["profile"]
// ["organization"]["rate_limit_tier"]` field (design.md §1.2/§6.1 — this data
// is already on disk today, just never consumed at this granularity; no new
// fetch, no new persistence). Returns ClaudeTierUnknown for a nil Auth, a
// missing quota_snapshot, or any rate_limit_tier value not in
// claudeRateLimitTierValues — it never panics and never misjudges an
// unrecognized value into a specific tier.
//
// This does NOT go through registry.NormalizeClaudeSubscriptionPlan or any
// other existing folding function (spec.md requirement) — do not route this
// value through Normalize* on the caller side either, or the whole point of
// this read path is lost.
func (a *Auth) ClaudeSubscriptionTier() ClaudeTier {
	if a == nil {
		return ClaudeTierUnknown
	}
	raw := nestedMetadataString(a.Metadata, "quota_snapshot", "profile", "organization", "rate_limit_tier")
	if raw == "" {
		return ClaudeTierUnknown
	}
	if tier, ok := claudeRateLimitTierValues[strings.ToLower(raw)]; ok {
		return tier
	}
	return ClaudeTierUnknown
}

// CodexTier identifies a Codex account's subscription tier from the raw
// `chatgpt_plan_type` value. Codex has no 5x/20x-style capacity split like
// Claude Max does — the "5x/20x" language sometimes used for Codex describes
// Pro's internal $100/$200 pricing multiple over Plus, not a distinct
// chatgpt_plan_type value (design.md §1.2) — so CodexTier and ClaudeTier are
// intentionally separate enums, never compared or mixed (spec.md: "SHALL NOT
// 把 Codex 等级与 Claude Max 的 5x/20x 语义混用同一套枚举").
type CodexTier int

const (
	// CodexTierUnknown is returned when plan_type is missing or does not
	// match a recognized value (e.g. an unmapped "team"/"business" value —
	// see design.md O4: those are not yet given dedicated capacity weights).
	CodexTierUnknown CodexTier = iota
	// CodexPro is chatgpt_plan_type == "pro".
	CodexPro
	// CodexPlus is chatgpt_plan_type == "plus".
	CodexPlus
)

// String returns a stable, lowercase label for the tier.
func (t CodexTier) String() string {
	switch t {
	case CodexPro:
		return "pro"
	case CodexPlus:
		return "plus"
	default:
		return "unknown"
	}
}

// codexPlanTypeValues maps the raw chatgpt_plan_type values this fork
// currently gives dedicated capacity weights to. Team/business and any other
// future value intentionally fall back to CodexTierUnknown (design.md O4)
// rather than being guessed into pro or plus.
var codexPlanTypeValues = map[string]CodexTier{
	"pro":  CodexPro,
	"plus": CodexPlus,
}

// CodexSubscriptionTier returns a's Codex subscription tier, read from
// `Attributes["plan_type"]`. That field is populated at OAuth time directly
// from the id_token's `chatgpt_plan_type` claim (see
// sdk/auth/codex_device.go's buildAuthRecord and
// internal/watcher/synthesizer/file.go, which restores it from the persisted
// auth file across restarts) — it is the raw, unfolded value, not routed
// through any Normalize* folding. Returns CodexTierUnknown for a nil Auth, a
// nil/empty Attributes map, or any plan_type value not in
// codexPlanTypeValues.
func (a *Auth) CodexSubscriptionTier() CodexTier {
	if a == nil || len(a.Attributes) == 0 {
		return CodexTierUnknown
	}
	raw := strings.ToLower(strings.TrimSpace(a.Attributes["plan_type"]))
	if raw == "" {
		return CodexTierUnknown
	}
	if tier, ok := codexPlanTypeValues[raw]; ok {
		return tier
	}
	return CodexTierUnknown
}

// ClaudeTierBaseWeight returns the configured base capacity weight for tier
// from weights (internalconfig.AccountSchedulingConfig.TierWeights.Claude —
// see internal/config/account_scheduling.go and design.md §5.2). Falls back
// to weights.Unknown for ClaudeTierUnknown, so a caller never has to special-
// case "tier not recognized" separately from "weight not configured".
func ClaudeTierBaseWeight(tier ClaudeTier, weights internalconfig.ClaudeTierWeights) float64 {
	switch tier {
	case ClaudeMax20x:
		return weights.Max20x
	case ClaudeMax5x:
		return weights.Max5x
	case ClaudePro:
		return weights.Pro
	default:
		return weights.Unknown
	}
}

// CodexTierBaseWeight returns the configured base capacity weight for tier
// from weights (internalconfig.AccountSchedulingConfig.TierWeights.Codex).
// Falls back to weights.Unknown for CodexTierUnknown.
func CodexTierBaseWeight(tier CodexTier, weights internalconfig.CodexTierWeights) float64 {
	switch tier {
	case CodexPro:
		return weights.Pro
	case CodexPlus:
		return weights.Plus
	default:
		return weights.Unknown
	}
}

// AccountTierBaseWeight returns the configured base capacity weight for a's
// current subscription tier, dispatching on a.Provider so the later-phase
// selector does not need to duplicate the Claude-vs-Codex branch at every
// call site. Weights are only meaningful within a single provider (design.md
// §5.2: a Claude weight is never compared against a Codex weight), so a
// provider this function does not recognize returns 0 rather than guessing —
// callers computing a weighted score across providers must not rely on this
// path for those providers.
func (a *Auth) AccountTierBaseWeight(weights internalconfig.AccountTierWeightsConfig) float64 {
	if a == nil {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "claude":
		return ClaudeTierBaseWeight(a.ClaudeSubscriptionTier(), weights.Claude)
	case "codex":
		return CodexTierBaseWeight(a.CodexSubscriptionTier(), weights.Codex)
	default:
		return 0
	}
}

// nestedMetadataString walks meta through a sequence of nested-object keys
// and returns the string found at the final key, or "" if any hop along the
// path is missing, not an object (until the last hop), or the final value is
// not a string. Reuses the package's existing metadataObject helper (see
// types.go) so this accepts the same tolerant shapes (map[string]any,
// map[string]string, or anything JSON-marshalable into one) that the rest of
// the Auth metadata-reading code already tolerates.
func nestedMetadataString(meta map[string]any, path ...string) string {
	current := meta
	for i, key := range path {
		if len(current) == 0 {
			return ""
		}
		raw, ok := current[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			s, ok := raw.(string)
			if !ok {
				return ""
			}
			return strings.TrimSpace(s)
		}
		nested, ok := metadataObject(raw)
		if !ok {
			return ""
		}
		current = nested
	}
	return ""
}
