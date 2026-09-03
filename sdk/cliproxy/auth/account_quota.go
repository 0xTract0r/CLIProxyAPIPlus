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
//  "seven_day":{...},"seven_day_sonnet":{...},"extra_usage":{"is_enabled":false}}.
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
