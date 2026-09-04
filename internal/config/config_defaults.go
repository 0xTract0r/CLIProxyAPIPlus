package config

import "time"

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultErrorLogsMaxFiles     = 10
	DefaultLogsCompressAfterDays = 7
	DefaultLogsDeleteAfterDays   = 30
	// DefaultLoggingDisplayTimezoneOffsetHours 是「人看的日志」默认显示时区偏移（小时）。
	// 默认 8 = UTC+8（东八区）。只影响日志显示/解析，不影响出站时间字段。
	DefaultLoggingDisplayTimezoneOffsetHours = 8
)

const (
	DefaultQuotaSnapshotRefreshInterval            = 45 * time.Minute
	DefaultQuotaSnapshotRefreshJitter              = 10 * time.Minute
	DefaultQuotaSnapshotRefreshStartupMaxStaleness = 24 * time.Hour

	DefaultQuotaSnapshotRefreshIntervalString            = "45m"
	DefaultQuotaSnapshotRefreshJitterString              = "10m"
	DefaultQuotaSnapshotRefreshStartupMaxStalenessString = "24h"
)

const (
	ClaudeSonnetLongContextPolicyFailWithHint  = "fail_with_hint"
	ClaudeSonnetLongContextPolicyRouteToOpus1M = "route_to_opus_1m"
	ClaudeSonnetLongContextPolicyCompact       = "compact_required"
)

// Routing strategy values (RoutingConfig.Strategy). RoundRobin and FillFirst
// are the pre-existing values (named here as constants for callers that
// previously used inline string literals — see
// sdk/cliproxy/service_config.go:newRoutingSelector, which normalizes
// arbitrary user input onto these). Adaptive is new
// (openspec/changes/add-adaptive-account-scheduling): it opts into
// tier/quota-aware weighted selection driven by AccountSchedulingConfig
// (Config.AccountScheduling). Wiring is live: newRoutingSelector recognizes
// Adaptive and constructs coreauth.NewAdaptiveSelector for it, so an "adaptive"
// strategy value takes effect immediately — it is no longer a no-op that falls
// back to round-robin. Before enabling it, backfill every account's
// first_production_at anchor, or an un-anchored account is treated as brand-new
// "cold" and throttled to the tightest warm-up stage.
const (
	RoutingStrategyRoundRobin = "round-robin"
	RoutingStrategyFillFirst  = "fill-first"
	RoutingStrategyAdaptive   = "adaptive"
)

// Adaptive account-scheduling defaults
// (openspec/changes/add-adaptive-account-scheduling/design.md §5). All are
// config-overridable via Config.AccountScheduling; these are only the
// fallback values applied when a config omits the corresponding field. See
// account_scheduling.go for the config schema and DefaultAccountSchedulingConfig
// (curve defaults live there since they are composite values, not scalars).
const (
	// DefaultAccountMatureRPMLimit / Burst / ConcurrencyLimit (design §5.3):
	// calibrated above the real observed peak (~40 rpm sustained, up to ~43
	// rpm burst) on a 4.5-month unbanned production Claude Max 20x account —
	// deliberately generous so this ceiling only intervenes on pathological
	// bursts, never on normal healthy-account throughput.
	DefaultAccountMatureRPMLimit         = 45
	DefaultAccountMatureBurst            = 10
	DefaultAccountMatureConcurrencyLimit = 4

	// DefaultAccountTierWeightUnknown is the fallback base-capacity weight
	// used when a subscription tier cannot be identified (spec.md requires
	// falling back to a coarse tier rather than misjudging it into a specific
	// one). Design §5.2 does not specify this value numerically (O4 is still
	// open); set to the lowest known-tier baseline
	// (Claude pro / Codex plus = 1) so an unidentified account is neither
	// starved (weight 0) nor favored over a confirmed entry-tier account.
	// Revisit once O4 (tier map calibration against a real 5x/team/business
	// account) closes.
	DefaultAccountTierWeightUnknown = 1.0

	// Claude tier base-capacity weights (design §5.2): relative multiples of
	// the Max subscription tiers vs. the Pro baseline, matching real
	// subscription pricing multiples (Max 20x / Max 5x).
	DefaultAccountTierWeightClaudeMax20x = 20.0
	DefaultAccountTierWeightClaudeMax5x  = 5.0
	DefaultAccountTierWeightClaudePro    = 1.0

	// Codex tier base-capacity weights (design §5.2, explicitly a placeholder
	// pending O4 real-quota calibration): Codex Pro ($200) is modeled as ~10x
	// Plus ($20) usage, Plus is the baseline.
	DefaultAccountTierWeightCodexPro  = 10.0
	DefaultAccountTierWeightCodexPlus = 1.0
)
