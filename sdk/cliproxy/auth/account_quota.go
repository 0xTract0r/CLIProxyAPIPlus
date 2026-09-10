package auth

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// This file implements openspec/changes/add-adaptive-account-scheduling
// tasks.md Phase 0 task 0.3: parse the already-persisted, already-polled
// Auth.Metadata["quota_snapshot"]["usage"] payload (refreshed on a ~45min
// cycle by internal/api/handlers/management/quota_snapshots.go) into
// structured per-window utilization, and expose a single "current quota
// headroom" query function for the Phase 1 weight function (design.md D1:
// weight = tier capacity x (1 - utilization%) x freshness).
//
// Scope boundary (see design.md D1 / tasks.md 0.3 vs 1.1): this file only
// reads the most recent snapshot already sitting in metadata. It
// deliberately does NOT do the token-delta estimation between two snapshots
// that design.md D1 describes ("两次快照之间用逐请求 token 数增量估算") --
// that refinement belongs to Phase 1 (tasks.md 1.1), which can layer a
// token-based adjustment on top of the structured windows this file exposes.
//
// Confirmed schema (design.md §1.1, docs/repo-memory-ledger.md §7.1, and
// live-verified via sdk/cliproxy/service_fork_anticorr.go's existing
// claudeUsageCreditsEnabled which already reads quota_snapshot.usage.extra_usage):
// Claude's quota_snapshot.usage (the raw https://api.anthropic.com/api/oauth/usage
// response body) is a flat object whose usage-window entries look like
// {"five_hour":{"utilization":8.0,"resets_at":"2026-01-22T09:00:00Z"},
//
//	"seven_day":{...},"seven_day_sonnet":{...},"extra_usage":{"is_enabled":false}}.
//
// utilization is a 0-100 percentage, not a 0-1 fraction. This file detects
// window objects generically (any object under "usage" carrying a numeric
// "utilization" field) rather than hardcoding the window name set, so it
// keeps working if Anthropic adds e.g. "seven_day_opus" without a code
// change, and it naturally skips non-window sibling objects like
// "extra_usage" (no "utilization" key) without special-casing them by name.
//
// Unconfirmed: Codex's quota_snapshot.usage (the raw
// https://chatgpt.com/backend-api/wham/usage response body) has not been
// captured against a real production Codex account in this repo, and
// community reverse-engineering (see gaps in the handoff for this slice)
// suggests it nests windows under "rate_limit.primary_window" /
// "secondary_window" using a "percent_left" field, not a top-level
// "utilization" field -- so the generic parser below will most likely find
// zero windows for a Codex auth today and correctly report "unknown"
// (ok=false) rather than silently misreading percent_left as utilization%.
// See the gaps note returned by this slice for what Phase 1 / a follow-up
// needs to close this (design.md O4).
const (
	accountQuotaSnapshotMetadataKey = "quota_snapshot"
	accountQuotaUsageKey            = "usage"
	accountQuotaUtilizationKey      = "utilization"
	accountQuotaResetsAtKey         = "resets_at"
)

// AccountQuotaWindow is one parsed usage window (e.g. Claude's "five_hour",
// "seven_day", "seven_day_sonnet") recovered from an auth's persisted
// quota_snapshot.usage.
type AccountQuotaWindow struct {
	// Name is the upstream window key verbatim (e.g. "five_hour", "seven_day").
	Name string
	// UtilizationPercent is the upstream-reported utilization as a 0-100
	// percentage (Anthropic /api/oauth/usage semantics), clamped to [0,100]
	// defensively against a malformed upstream value.
	UtilizationPercent float64
	// ResetsAt is when this window's utilization resets, if the upstream
	// payload included a parseable timestamp. Zero value if absent/unparseable.
	ResetsAt time.Time
}

// Headroom returns this window's available quota fraction, 1 -
// UtilizationPercent/100, clamped to [0,1].
func (w AccountQuotaWindow) Headroom() float64 {
	h := 1 - w.UtilizationPercent/100
	if h < 0 {
		return 0
	}
	if h > 1 {
		return 1
	}
	return h
}

// AccountQuotaUtilization is the structured form of one auth's parsed
// quota_snapshot.usage payload: every window the parser could identify,
// keyed by AccountQuotaWindow.Name.
type AccountQuotaUtilization struct {
	// Provider is auth.Provider at parse time, carried through for callers
	// that fan this out across providers (e.g. a mixed-provider weight pass).
	Provider string
	// Windows holds every usage window the parser could identify in the
	// snapshot. Empty (non-nil) map is a valid, meaningful result: it means
	// the snapshot existed but no window objects were recognized in it (see
	// ParseAccountQuotaUtilization's ok return for how callers should read that).
	Windows map[string]AccountQuotaWindow
}

// ParseAccountQuotaUtilization extracts structured per-window utilization
// from auth.Metadata["quota_snapshot"]["usage"].
//
// ok=false means the snapshot could not be read at all -- either
// Metadata["quota_snapshot"] or its "usage" sub-object is missing/malformed,
// or "usage" parsed but contained zero recognizable window objects. Callers
// MUST treat ok=false as "unknown", never as "0% utilized" / "100% headroom":
// a freshly-added or not-yet-probed account, a provider whose quota endpoint
// core does not poll (anything other than claude/codex), or a transient
// probe failure (quota_refresh_status=error, see quota_snapshots.go) all
// look identical from this function's point of view, and none of them mean
// "this account is empty and safe to flood".
func ParseAccountQuotaUtilization(auth *Auth) (AccountQuotaUtilization, bool) {
	result := AccountQuotaUtilization{}
	if auth == nil {
		return result, false
	}
	result.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))

	snapshot, ok := metadataObject(auth.Metadata[accountQuotaSnapshotMetadataKey])
	if !ok {
		return result, false
	}
	usage, ok := metadataObject(snapshot[accountQuotaUsageKey])
	if !ok {
		return result, false
	}

	windows := parseAccountQuotaWindows(usage)
	result.Windows = windows
	return result, len(windows) > 0
}

// parseAccountQuotaWindows scans every top-level entry of a quota_snapshot
// usage object and treats any entry that is itself an object carrying a
// numeric "utilization" field as a usage window. This deliberately does not
// hardcode the window name set (see the file-level doc comment).
func parseAccountQuotaWindows(usage map[string]any) map[string]AccountQuotaWindow {
	windows := make(map[string]AccountQuotaWindow)
	for key, raw := range usage {
		obj, ok := metadataObject(raw)
		if !ok {
			continue
		}
		utilization, ok := accountQuotaNumericValue(obj[accountQuotaUtilizationKey])
		if !ok {
			// Not a usage-window object (e.g. "extra_usage": {"is_enabled": false}).
			continue
		}
		if utilization < 0 {
			utilization = 0
		}
		if utilization > 100 {
			utilization = 100
		}
		resetsAt, _ := parseTimeValue(obj[accountQuotaResetsAtKey])
		windows[key] = AccountQuotaWindow{
			Name:               key,
			UtilizationPercent: utilization,
			ResetsAt:           resetsAt,
		}
	}
	return windows
}

// AccountQuotaHeadroomResult is the outcome of AccountQuotaHeadroom: the
// single tightest (lowest-headroom) known window across an auth's parsed
// quota snapshot, which is the window that should actually gate a weighting
// decision -- design.md D1 ("利用率越高、余量越少、权重越低") is expressed
// per-account, and an account is only as available as its most-exhausted
// window (a Claude account at 90% of its five_hour window is not safe to
// route more traffic to just because its seven_day window still has room).
type AccountQuotaHeadroomResult struct {
	// Headroom is the binding window's 1 - utilization%/100, clamped to [0,1].
	Headroom float64
	// Window is the binding window's Name (e.g. "five_hour"), so callers can
	// log/surface which window is actually constraining this account.
	Window string
	// ResetsAt is the binding window's reset time, if the upstream reported one.
	ResetsAt time.Time
}

// AccountQuotaHeadroom returns the tightest (minimum) known headroom across
// an auth's parsed quota windows -- "多窗口取最紧" per this slice's brief,
// matching design.md D1's weighting axis.
//
// ok=false means "no usable quota_snapshot at all" (see
// ParseAccountQuotaUtilization's ok semantics). Whether to then treat the
// account conservatively (assume low headroom, since design.md's stated bias
// elsewhere -- e.g. §6.2's token-bucket-restarts-conservative rationale -- is
// "unknown should never be read as safe to flood") or neutrally (assume full
// headroom, e.g. for a provider this subsystem does not poll quota for at
// all, where "unknown" is simply the permanent, expected state) is a Phase 1
// weight-function policy decision, not something this parsing-only function
// should bake in silently by picking a single numeric fallback -- Phase 1
// has the tier/provider context (design.md O5/O6 and tasks.md 1.1) needed to
// pick correctly per-provider, this function does not.
func AccountQuotaHeadroom(auth *Auth) (AccountQuotaHeadroomResult, bool) {
	utilization, ok := ParseAccountQuotaUtilization(auth)
	if !ok {
		return AccountQuotaHeadroomResult{}, false
	}

	tightest := AccountQuotaHeadroomResult{Headroom: 1}
	found := false
	for _, window := range utilization.Windows {
		headroom := window.Headroom()
		if !found || headroom < tightest.Headroom {
			tightest = AccountQuotaHeadroomResult{
				Headroom: headroom,
				Window:   window.Name,
				ResetsAt: window.ResetsAt,
			}
			found = true
		}
	}
	if !found {
		return AccountQuotaHeadroomResult{}, false
	}
	return tightest, true
}

// -----------------------------------------------------------------------------
// OBS observability layer (harden-account-scheduling-limiter design §4.0).
//
// This block adds three DERIVED, observability-only metrics on top of the quota
// windows parsed above: an EWMA burn rate (ΔUtilization% per hour of the binding
// window), a projected exhaustion time, and a dry-run pacing factor. They are
// "只算不拦" (compute-only): NOTHING here multiplies into any rpm / concurrency /
// selection weight / gate -- the burn state is persisted for the management
// projection and future calibration, and the pacing factor is emitted as a
// dry-run number only. It NEVER changes selection or limiting behaviour.
//
// Why the EWMA state lives in the account_scheduling object (not quota_snapshot):
// the ~3.5min quota refresh replaces the whole quota_snapshot object wholesale,
// which would wipe any per-refresh history stored inside it. account_scheduling
// is a TOP-LEVEL metadata key that Auth.Clone carries through untouched (see
// account_scheduling_metadata.go), so the {prev_util, prev_at, burn_rate_ewma,
// projected_exhaustion_at} tuple survives across refresh cycles.
// -----------------------------------------------------------------------------

// account_scheduling sub-keys holding the persisted EWMA burn state. They live
// inside the same namespaced object as rate_scale / tier_source and are written
// exclusively through setAccountSchedulingValue (never bare top-level keys).
const (
	accountSchedulingBurnPrevWindowKey       = "burn_prev_window"
	accountSchedulingBurnPrevUtilKey         = "burn_prev_util_percent"
	accountSchedulingBurnPrevAtKey           = "burn_prev_at"
	accountSchedulingBurnRateEWMAKey         = "burn_rate_ewma_per_hour"
	accountSchedulingBurnProjectedExhaustKey = "burn_projected_exhaustion_at"
)

// Calibration knobs (design §5 marks these "待校准"). They are package vars, not
// consts, so a future config-wiring slice can source them without an API change
// (mirroring SessionActiveWindow in the management projection). Changing them
// affects only the observability numbers, never any real limit.
var (
	// BurnRateEWMAAlpha is the EWMA smoothing factor in [0,1]: higher reacts
	// faster to the latest sample, lower is smoother. 0.3 is a deliberately
	// smooth starting point pending real burn-curve calibration.
	BurnRateEWMAAlpha = 0.3
	// PacingFactorK is the target pace factor at exactly the fair burn rate. At
	// k=1 the dry-run factor is 1 (no pace-down) while burn <= fair, and drops
	// below 1 only when the account burns faster than its fair share to reset.
	PacingFactorK = 1.0
	// PacingFactorFloor is the lower clamp on the dry-run pacing factor, so a
	// runaway burn can never drive the (dry-run) factor to zero.
	PacingFactorFloor = 0.1
)

const (
	// burnRateEpsilon treats any |burn| at or below this as "not burning".
	burnRateEpsilon = 1e-9
	// burnProjectionMaxHours caps how far out projected_exhaustion_at may be. A
	// tiny burn over full headroom would otherwise project centuries away (and
	// risk time.Duration overflow); beyond this horizon we report "no projection"
	// (null) rather than a meaningless far-future timestamp.
	burnProjectionMaxHours = 365 * 24
)

// AccountBurnState is the persisted EWMA burn observability tuple for one auth,
// recovered from its account_scheduling sub-object. Has* flags follow the
// "unknown is not a number" contract: a field with Has*=false has no meaningful
// value yet (e.g. only one sample seen so far) and MUST be surfaced as null, not 0.
type AccountBurnState struct {
	// PrevWindow is the binding-window name at the previous sample (used to
	// detect a window switch, which makes a cross-window delta meaningless).
	PrevWindow string
	// PrevUtilizationPercent is the previous binding-window utilization% (0-100).
	PrevUtilizationPercent float64
	// PrevAt is when the previous sample was taken.
	PrevAt time.Time
	// HasPrev reports whether a usable previous sample (PrevAt) exists.
	HasPrev bool
	// BurnRatePerHour is the smoothed EWMA burn rate in utilization-percentage
	// points per hour of the binding window.
	BurnRatePerHour float64
	// HasBurnRate reports whether a burn rate has been computed (>=2 same-window
	// samples). False until then.
	HasBurnRate bool
	// ProjectedExhaustionAt is now+headroom/burn at the last write, when burning.
	ProjectedExhaustionAt time.Time
	// HasProjection reports whether a projection exists (positive burn, bounded horizon).
	HasProjection bool
}

// ReadAccountBurnState recovers the persisted EWMA burn state from an auth's
// account_scheduling sub-object. A nil/empty auth or absent state yields a
// zero-value struct with every Has*=false (all "unknown").
func ReadAccountBurnState(auth *Auth) AccountBurnState {
	var st AccountBurnState
	if auth == nil || len(auth.Metadata) == 0 {
		return st
	}
	if raw, ok := accountSchedulingRawValue(auth.Metadata, accountSchedulingBurnPrevAtKey); ok {
		if ts, ok := parseFirstProductionAtValue(raw); ok {
			st.PrevAt = ts
			st.HasPrev = true
		}
	}
	if raw, ok := accountSchedulingRawValue(auth.Metadata, accountSchedulingBurnPrevUtilKey); ok {
		if v, ok := accountQuotaNumericValue(raw); ok {
			st.PrevUtilizationPercent = v
		}
	}
	st.PrevWindow = accountSchedulingString(auth.Metadata, accountSchedulingBurnPrevWindowKey)
	if raw, ok := accountSchedulingRawValue(auth.Metadata, accountSchedulingBurnRateEWMAKey); ok {
		if v, ok := accountQuotaNumericValue(raw); ok {
			st.BurnRatePerHour = v
			st.HasBurnRate = true
		}
	}
	if raw, ok := accountSchedulingRawValue(auth.Metadata, accountSchedulingBurnProjectedExhaustKey); ok {
		if ts, ok := parseFirstProductionAtValue(raw); ok {
			st.ProjectedExhaustionAt = ts
			st.HasProjection = true
		}
	}
	return st
}

// UpdateAccountBurnObservability samples the freshly-refreshed quota snapshot on
// updatedAuth against the previous persisted sample on prevAuth, updates the EWMA
// burn rate + projected exhaustion, and persists the new state into updatedAuth's
// account_scheduling sub-object via setAccountSchedulingValue. It is called from
// the single quota refresh write-back point (quota_snapshots.go) where a fresh
// utilization%, a fixed cadence, a live auth and a persist all coincide.
//
// Observability-only: it derives numbers and NEVER touches any selection / limit /
// gate field. When the fresh snapshot has no usable quota window (unknown -- e.g.
// a Codex account, or a probe that returned nothing parseable) it leaves any
// existing burn state untouched rather than fabricating a burn rate from unknown.
func UpdateAccountBurnObservability(prevAuth, updatedAuth *Auth, now time.Time) {
	if updatedAuth == nil {
		return
	}
	headroom, ok := AccountQuotaHeadroom(updatedAuth)
	if !ok {
		// Unknown quota: safe default is to leave state as-is, never invent a rate.
		return
	}
	if updatedAuth.Metadata == nil {
		updatedAuth.Metadata = make(map[string]any)
	}

	newUtil := clampUtilizationPercent((1 - headroom.Headroom) * 100)
	newWindow := headroom.Window

	// Capture the previous sample BEFORE overwriting the baseline below (prevAuth
	// and updatedAuth may be the same pointer in a test / in-process caller).
	prev := ReadAccountBurnState(prevAuth)

	// Always advance the baseline to this fresh sample.
	setAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnPrevWindowKey, newWindow)
	setAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnPrevUtilKey, newUtil)
	setAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnPrevAtKey, now.UTC().Format(time.RFC3339))

	// A rate is only meaningful between two same-window samples with a forward
	// clock. First sample ever, a window switch, or a non-advancing clock -> drop
	// any stale rate/projection so they read null until two same-window samples exist.
	if !prev.HasPrev || !strings.EqualFold(prev.PrevWindow, newWindow) {
		clearAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnRateEWMAKey)
		clearAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnProjectedExhaustKey)
		return
	}
	deltaHours := now.Sub(prev.PrevAt).Hours()
	if deltaHours <= 0 {
		clearAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnRateEWMAKey)
		clearAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnProjectedExhaustKey)
		return
	}

	instant := (newUtil - prev.PrevUtilizationPercent) / deltaHours
	if instant < 0 {
		// Utilization dropped (mid-window reset / upstream jitter): there is no
		// positive burn to project. Feed 0 so the EWMA decays toward "not burning".
		instant = 0
	}
	ewma := instant
	if prev.HasBurnRate {
		ewma = BurnRateEWMAAlpha*instant + (1-BurnRateEWMAAlpha)*prev.BurnRatePerHour
	}
	setAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnRateEWMAKey, ewma)

	// projected_exhaustion_at = now + remainingHeadroomPoints / burn, only while
	// actually burning and within a bounded horizon.
	remainingPoints := headroom.Headroom * 100
	if ewma > burnRateEpsilon && remainingPoints > 0 {
		if hours := remainingPoints / ewma; hours <= burnProjectionMaxHours {
			projected := now.UTC().Add(time.Duration(hours * float64(time.Hour)))
			setAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnProjectedExhaustKey, projected.Format(time.RFC3339))
			return
		}
	}
	clearAccountSchedulingValue(updatedAuth.Metadata, accountSchedulingBurnProjectedExhaustKey)
}

// AccountPacingObservability is the read-side, dry-run observability view derived
// from the persisted burn state plus the live binding-window headroom/reset. Every
// value is dry-run: PacingFactorDryRun is NEVER multiplied into a real rpm/limit.
type AccountPacingObservability struct {
	BurnRatePerHour       float64
	HasBurnRate           bool
	ProjectedExhaustionAt time.Time
	HasProjection         bool
	// PacingFactorDryRun is p = clamp(k*fair/burn, p_floor, 1), fair =
	// remainingHeadroomPoints / hoursUntilReset. Dry-run only.
	PacingFactorDryRun float64
	HasPacingFactor    bool
}

// AccountPacingObservabilityFor computes the dry-run pacing observability for an
// auth at time now. burn rate + projection are passed through from the persisted
// state; the pacing factor is derived fresh from the binding window's live
// headroom and resets_at. Any of the three is reported as unknown (Has*=false)
// rather than a fabricated number:
//   - no burn history (fewer than two same-window samples) -> pacing unknown;
//   - binding window missing resets_at, or already at/past reset -> pacing unknown
//     ("resets_at 缺失窗口跳过").
//
// It is a pure read: it reads only persisted state and never mutates auth or gates.
func AccountPacingObservabilityFor(auth *Auth, now time.Time) AccountPacingObservability {
	var out AccountPacingObservability
	st := ReadAccountBurnState(auth)
	out.BurnRatePerHour = st.BurnRatePerHour
	out.HasBurnRate = st.HasBurnRate
	out.ProjectedExhaustionAt = st.ProjectedExhaustionAt
	out.HasProjection = st.HasProjection

	if !st.HasBurnRate {
		return out
	}
	headroom, ok := AccountQuotaHeadroom(auth)
	if !ok || headroom.ResetsAt.IsZero() {
		return out
	}
	hoursUntilReset := headroom.ResetsAt.Sub(now).Hours()
	if hoursUntilReset <= 0 {
		return out
	}
	fair := (headroom.Headroom * 100) / hoursUntilReset
	var p float64
	if st.BurnRatePerHour <= burnRateEpsilon {
		// Burn has decayed to ~0: not pacing down.
		p = 1
	} else {
		p = PacingFactorK * fair / st.BurnRatePerHour
	}
	out.PacingFactorDryRun = clampPacingFactor(p)
	out.HasPacingFactor = true
	return out
}

func clampUtilizationPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func clampPacingFactor(p float64) float64 {
	if p < PacingFactorFloor {
		return PacingFactorFloor
	}
	if p > 1 {
		return 1
	}
	return p
}

// accountQuotaNumericValue parses a JSON-decoded value (float64, json.Number,
// int-family, or numeric string) as a float64. It intentionally does not
// accept bool -- a stray "utilization": true would otherwise silently parse
// as 1.0/100 rather than being rejected as "not a window".
func accountQuotaNumericValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
