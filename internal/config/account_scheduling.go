package config

import "fmt"

// AccountSchedulingConfig configures the adaptive account-scheduling subsystem
// consumed by the "adaptive" routing.strategy value (see RoutingConfig.Strategy
// and RoutingStrategyAdaptive): tier/quota-aware weighted account selection,
// per-account outbound rate limiting, and new-account warm-up throttling.
//
// This section only takes effect once routing.strategy is set to "adaptive";
// the existing "round-robin" / "fill-first" strategies ignore it entirely
// (openspec/changes/add-adaptive-account-scheduling/design.md D7: opt-in,
// backward compatible, safe to roll back at any time by switching strategy
// back). This struct is config schema only — it defines *what* is
// configurable and its defaults; the selector/token-bucket/warm-up logic that
// *reads* it lands in later phases of the same change (see tasks.md Phase
// 1-4).
//
// See design.md §5 for the numeric defaults below (the warm-up curve and the
// mature-account ceiling are both derived from a real production account's
// observed usage trajectory, design §1.3 — not guessed) and §6 for why no new
// persistence layer is introduced (every field here is either config-driven
// or derives, at read time, from the existing auth-JSON-persisted
// `first_production_at` anchor added in Phase 0; nothing here requires a
// database or new durable file format).
type AccountSchedulingConfig struct {
	// WarmupCurve defines the age-based (days since an account's
	// first-production anchor) throttling stages a new account climbs through
	// before it is considered "mature". Stages MUST be contiguous, sorted by
	// ascending MinAgeDays, and only the last stage may leave MaxAgeDays unset
	// (0 = unbounded); see Validate.
	//
	// Once an account's age exceeds the last stage's MaxAgeDays, it is
	// "mature" and MatureLimits applies instead — mature status is
	// deliberately NOT modeled as a trailing WarmupCurve entry, so the
	// mature-account ceiling has exactly one source of truth (MatureLimits),
	// not two tables that could drift out of sync.
	//
	// When empty (the config omits warmup-curve entirely), the defaults from
	// DefaultAccountWarmupCurve are used. When the config supplies any stage,
	// the supplied list REPLACES the defaults wholesale (no per-stage merge
	// with defaults) — the curve is meant to be authored and reviewed as a
	// whole, not patched field-by-field.
	WarmupCurve []AccountWarmupStage `yaml:"warmup-curve,omitempty" json:"warmup-curve,omitempty"`

	// MatureLimits is the per-account outbound safety ceiling applied once an
	// account is past the last WarmupCurve stage (or, per Phase 0/1 tier
	// identification — not decided by this config scaffold — has no usable
	// freshness anchor and is conservatively treated as mature rather than
	// perpetually throttled). Deliberately generous: calibrated above real
	// observed safe peak usage (design §5.3, ~40 rpm sustained peak on a
	// 4.5-month unbanned production account), so in practice it only
	// intervenes on pathological bursts and never caps a healthy account's
	// paid-for throughput.
	MatureLimits AccountMatureLimitsConfig `yaml:"mature-limits,omitempty" json:"mature-limits,omitempty"`

	// TierWeights maps subscription tier -> base capacity weight. The
	// adaptive selector (Phase 1, not this file) is documented to consume it
	// as: weight = base capacity x (1 - quota utilization%) x freshness
	// factor. Weights are only meaningful WITHIN a single provider — a Claude
	// weight is never compared against a Codex weight, since the two
	// providers' quota semantics are unrelated (design §5.2).
	TierWeights AccountTierWeightsConfig `yaml:"tier-weights,omitempty" json:"tier-weights,omitempty"`

	// RateScale is the global default per-account safety-test speed multiplier
	// (design §8.3, spec.md "per-账号安全测试速率乘子"). It scales every account's
	// DERIVED rate ceilings -- rpm / burst / concurrency / daily budget -- AFTER
	// the tier/warm-up derivation, and is deliberately INDEPENDENT of selection
	// weight (it never changes WHICH account is picked, only how fast the picked
	// account may go). 1.0 (the default) is a no-op; a value < 1 throttles every
	// account below its tier/warm-up ceiling for low-risk testing, > 1 lifts it.
	// A per-account metadata override (account_scheduling.rate_scale) takes
	// precedence over this global default. MUST be > 0 (see Validate); the
	// per-account 0-floor that keeps a fractional scale from wedging a limit to
	// zero lives in the read path (sdk/cliproxy/auth/account_rate_scale.go), not
	// here.
	RateScale float64 `yaml:"rate-scale,omitempty" json:"rate-scale,omitempty"`
}

// AccountWarmupStage describes one age-based warm-up throttling tier.
// Boundaries are expressed as an account's age (in whole days) since its
// first-production anchor: [MinAgeDays, MaxAgeDays). MinAgeDays is inclusive;
// MaxAgeDays is exclusive, with 0 meaning unbounded (only valid on the final
// stage of a curve).
type AccountWarmupStage struct {
	// Name is a short human-readable label for this stage (e.g. "w1",
	// "w3-4"), surfaced in logs/management API so operators can tell which
	// stage an account is currently throttled under.
	Name string `yaml:"name" json:"name"`

	// MinAgeDays is the inclusive lower bound of account age, in days since
	// first-production, this stage applies to.
	MinAgeDays int `yaml:"min-age-days" json:"min-age-days"`

	// MaxAgeDays is the exclusive upper bound of account age this stage
	// applies to. 0 means unbounded; only the last stage in a curve may leave
	// this unset (see Validate).
	MaxAgeDays int `yaml:"max-age-days,omitempty" json:"max-age-days,omitempty"`

	// DailyBudget is the max requests/day this stage allows — design.md
	// documents the daily budget as the primary warm-up throttle signal, with
	// RPMLimit as a coarse burst backstop. 0 means unbounded (quota/weight-
	// driven instead of a fixed daily cap); only meaningful for a terminal/
	// mature context, so a finite warm-up stage should always set this > 0.
	DailyBudget int `yaml:"daily-budget,omitempty" json:"daily-budget,omitempty"`

	// RPMLimit is the requests-per-minute burst-smoothing ceiling for this
	// stage — a coarse backstop against pathological bursts; DailyBudget is
	// the primary throttle (design §5: rpm is only a coarse anti-burst
	// backstop, not the main control signal).
	RPMLimit int `yaml:"rpm-limit" json:"rpm-limit"`

	// ConcurrencyLimit is the max concurrent in-flight requests this stage
	// allows for one account.
	ConcurrencyLimit int `yaml:"concurrency-limit" json:"concurrency-limit"`
}

// AccountMatureLimitsConfig defines the per-account safety ceiling applied
// once an account is past its warm-up curve. See AccountSchedulingConfig.MatureLimits.
type AccountMatureLimitsConfig struct {
	// RPMLimit is the steady-state requests-per-minute ceiling.
	RPMLimit int `yaml:"rpm-limit,omitempty" json:"rpm-limit,omitempty"`

	// Burst is the token-bucket burst allowance layered on top of RPMLimit,
	// absorbing short legitimate spikes (e.g. a client firing several tool
	// calls back-to-back) without tripping the steady-state ceiling.
	Burst int `yaml:"burst,omitempty" json:"burst,omitempty"`

	// ConcurrencyLimit is the max concurrent in-flight requests for one
	// mature account.
	ConcurrencyLimit int `yaml:"concurrency-limit,omitempty" json:"concurrency-limit,omitempty"`
}

// AccountTierWeightsConfig groups per-provider base capacity weight tables.
// See AccountSchedulingConfig.TierWeights.
type AccountTierWeightsConfig struct {
	// Claude maps Claude Max/Pro subscription tiers, identified from the
	// unfolded `quota_snapshot.profile.organization.rate_limit_tier` (Phase 0
	// — not this file), to base capacity weights.
	Claude ClaudeTierWeights `yaml:"claude,omitempty" json:"claude,omitempty"`

	// Codex maps Codex `chatgpt_plan_type` tiers to base capacity weights.
	Codex CodexTierWeights `yaml:"codex,omitempty" json:"codex,omitempty"`
}

// ClaudeTierWeights holds base capacity weights for Claude subscription
// tiers. Values are relative multiples within the Claude provider only (see
// AccountSchedulingConfig.TierWeights); design §5.2 anchors them to real
// subscription pricing multiples (Max 20x / Max 5x / Pro).
type ClaudeTierWeights struct {
	Max20x float64 `yaml:"max-20x,omitempty" json:"max-20x,omitempty"`
	Max5x  float64 `yaml:"max-5x,omitempty" json:"max-5x,omitempty"`
	Pro    float64 `yaml:"pro,omitempty" json:"pro,omitempty"`

	// Unknown is the fallback weight used when rate_limit_tier is
	// missing/unrecognized (spec.md requires falling back to a coarse tier
	// and explicitly flagging it as unknown, rather than misjudging it into
	// a specific tier — never silently assume max_20x or pro).
	Unknown float64 `yaml:"unknown,omitempty" json:"unknown,omitempty"`
}

// CodexTierWeights holds base capacity weights for Codex chatgpt_plan_type
// tiers. Values are relative multiples within the Codex provider only (see
// AccountSchedulingConfig.TierWeights). design §5.2 marks the Pro/Plus
// multiple as a placeholder pending real-quota calibration (open item O4);
// team/business plan types are not yet given dedicated defaults and fall
// back to Unknown until O4 closes.
type CodexTierWeights struct {
	Pro  float64 `yaml:"pro,omitempty" json:"pro,omitempty"`
	Plus float64 `yaml:"plus,omitempty" json:"plus,omitempty"`

	// Unknown is the fallback weight for any chatgpt_plan_type value not
	// explicitly mapped above (e.g. "team", "business", or a missing/future
	// value) — same "don't misclassify into a known tier" rule as
	// ClaudeTierWeights.Unknown.
	Unknown float64 `yaml:"unknown,omitempty" json:"unknown,omitempty"`
}

// DefaultAccountSchedulingConfig returns the design §5 defaults for the
// adaptive account-scheduling subsystem. Callers pre-populate a fresh Config
// with this before YAML unmarshal (mirroring DefaultCredentialInFlightConfig
// in credential_in_flight.go) so that yaml.v3's field-level merge semantics
// let an operator override a single nested field (e.g. just
// tier-weights.claude.max-20x) without having to restate the entire section.
func DefaultAccountSchedulingConfig() AccountSchedulingConfig {
	return AccountSchedulingConfig{
		WarmupCurve:  DefaultAccountWarmupCurve(),
		MatureLimits: DefaultAccountMatureLimits(),
		TierWeights:  DefaultAccountTierWeights(),
		RateScale:    DefaultAccountSchedulingRateScale,
	}
}

// DefaultAccountWarmupCurve returns the design §5.1 warm-up stages, derived
// from a real production Claude Max 20x account's observed ramp (design
// §1.3). Age boundaries (7/14/30/45/60 days) match the table's stated weekly
// groupings; the terminal "mature" state is intentionally NOT included here
// (see AccountSchedulingConfig.WarmupCurve doc) — anything aged 60+ days uses
// DefaultAccountMatureLimits instead.
func DefaultAccountWarmupCurve() []AccountWarmupStage {
	return []AccountWarmupStage{
		{Name: "w1", MinAgeDays: 0, MaxAgeDays: 7, DailyBudget: 200, RPMLimit: 3, ConcurrencyLimit: 1},
		{Name: "w2", MinAgeDays: 7, MaxAgeDays: 14, DailyBudget: 500, RPMLimit: 5, ConcurrencyLimit: 1},
		{Name: "w3-4", MinAgeDays: 14, MaxAgeDays: 30, DailyBudget: 2000, RPMLimit: 12, ConcurrencyLimit: 2},
		{Name: "w5-6", MinAgeDays: 30, MaxAgeDays: 45, DailyBudget: 4500, RPMLimit: 20, ConcurrencyLimit: 2},
		{Name: "w7-8", MinAgeDays: 45, MaxAgeDays: 60, DailyBudget: 6500, RPMLimit: 30, ConcurrencyLimit: 3},
	}
}

// DefaultAccountMatureLimits returns the design §5.3 mature-account ceiling.
func DefaultAccountMatureLimits() AccountMatureLimitsConfig {
	return AccountMatureLimitsConfig{
		RPMLimit:         DefaultAccountMatureRPMLimit,
		Burst:            DefaultAccountMatureBurst,
		ConcurrencyLimit: DefaultAccountMatureConcurrencyLimit,
	}
}

// DefaultAccountTierWeights returns the design §5.2 tier -> base capacity
// weight tables.
func DefaultAccountTierWeights() AccountTierWeightsConfig {
	return AccountTierWeightsConfig{
		Claude: ClaudeTierWeights{
			Max20x:  DefaultAccountTierWeightClaudeMax20x,
			Max5x:   DefaultAccountTierWeightClaudeMax5x,
			Pro:     DefaultAccountTierWeightClaudePro,
			Unknown: DefaultAccountTierWeightUnknown,
		},
		Codex: CodexTierWeights{
			Pro:     DefaultAccountTierWeightCodexPro,
			Plus:    DefaultAccountTierWeightCodexPlus,
			Unknown: DefaultAccountTierWeightUnknown,
		},
	}
}

// Validate checks internal consistency of the adaptive account-scheduling
// config. It is schema-level validation only (well-formedness, no gaps/
// overlaps, positive limits) — it does not and cannot validate anything
// about live account state, which is Phase 1-4's concern.
func (c AccountSchedulingConfig) Validate() error {
	if errStages := validateAccountWarmupCurve(c.WarmupCurve); errStages != nil {
		return errStages
	}
	if c.MatureLimits.RPMLimit <= 0 {
		return fmt.Errorf("account-scheduling.mature-limits.rpm-limit must be positive")
	}
	if c.MatureLimits.Burst < 0 {
		return fmt.Errorf("account-scheduling.mature-limits.burst must not be negative")
	}
	if c.MatureLimits.ConcurrencyLimit <= 0 {
		return fmt.Errorf("account-scheduling.mature-limits.concurrency-limit must be positive")
	}
	if c.RateScale <= 0 {
		return fmt.Errorf("account-scheduling.rate-scale must be positive")
	}
	weights := map[string]float64{
		"tier-weights.claude.max-20x": c.TierWeights.Claude.Max20x,
		"tier-weights.claude.max-5x":  c.TierWeights.Claude.Max5x,
		"tier-weights.claude.pro":     c.TierWeights.Claude.Pro,
		"tier-weights.claude.unknown": c.TierWeights.Claude.Unknown,
		"tier-weights.codex.pro":      c.TierWeights.Codex.Pro,
		"tier-weights.codex.plus":     c.TierWeights.Codex.Plus,
		"tier-weights.codex.unknown":  c.TierWeights.Codex.Unknown,
	}
	// Deterministic iteration for stable error messages.
	for _, key := range []string{
		"tier-weights.claude.max-20x", "tier-weights.claude.max-5x", "tier-weights.claude.pro",
		"tier-weights.claude.unknown", "tier-weights.codex.pro", "tier-weights.codex.plus",
		"tier-weights.codex.unknown",
	} {
		if weights[key] <= 0 {
			return fmt.Errorf("account-scheduling.%s must be positive", key)
		}
	}
	return nil
}

func validateAccountWarmupCurve(stages []AccountWarmupStage) error {
	for i, stage := range stages {
		if stage.Name == "" {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].name must not be empty", i)
		}
		if stage.MinAgeDays < 0 {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].min-age-days must not be negative", i)
		}
		if stage.MaxAgeDays != 0 && stage.MaxAgeDays <= stage.MinAgeDays {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].max-age-days must be greater than min-age-days (or 0 for unbounded)", i)
		}
		if stage.MaxAgeDays == 0 && i != len(stages)-1 {
			return fmt.Errorf("account-scheduling.warmup-curve[%d] leaves max-age-days unbounded but is not the last stage", i)
		}
		if stage.DailyBudget < 0 {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].daily-budget must not be negative", i)
		}
		if stage.RPMLimit <= 0 {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].rpm-limit must be positive", i)
		}
		if stage.ConcurrencyLimit <= 0 {
			return fmt.Errorf("account-scheduling.warmup-curve[%d].concurrency-limit must be positive", i)
		}
		if i == 0 && stage.MinAgeDays != 0 {
			return fmt.Errorf("account-scheduling.warmup-curve[0].min-age-days must be 0 so every account age is covered")
		}
		if i > 0 {
			prev := stages[i-1]
			if stage.MinAgeDays != prev.MaxAgeDays {
				return fmt.Errorf("account-scheduling.warmup-curve[%d].min-age-days must equal warmup-curve[%d].max-age-days (stages must be contiguous, no gaps or overlaps)", i, i-1)
			}
		}
	}
	return nil
}
