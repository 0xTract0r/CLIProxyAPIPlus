package auth

import (
	"strings"
	"time"
)

// FirstProductionAtMetadataKey is the persisted auth.Metadata key for an
// account's append-only "first production" anchor: the wall-clock instant
// this credential was first actually used to serve a real request, stamped
// once and never overwritten again.
//
// This is the sole freshness anchor the adaptive account-scheduling change
// (openspec/changes/add-adaptive-account-scheduling) uses to derive an
// account's age for warm-up curve lookups and tier/quota weighting (design.md
// D3/6.1-6.2, spec.md "新账号养号期渐进放量" / "稳定的新鲜度锚点"). It is
// deliberately NOT the auth file's mtime or CreatedAt timestamp: both of
// those get touched by unrelated token/quota refresh writes and by a
// credential being re-authenticated (re-auth replaces the backing file), so
// neither is stable across a credential's lifetime the way an append-only
// metadata field is (spec.md explicitly forbids using anything that
// token/quota refresh can overwrite).
//
// Persisted shape is an RFC3339 timestamp string in UTC, matching the
// convention already used for other Metadata-carried timestamps in this
// package (see FarmContainerAliveAtMetadataKey in custom_headers.go and
// metadataKeyQuarantinedAt in conductor_auto_quarantine.go). It lives in the
// same auth.Metadata map as quota_snapshot/rate_limit_tier, so it persists to
// whichever backend already stores Metadata (file store or Postgres store) --
// no new storage subsystem is introduced (design.md §6.4).
const FirstProductionAtMetadataKey = "first_production_at"

// AuthFirstProductionAt returns the persisted first-production anchor for
// auth, if one has been recorded. It never mutates auth or its Metadata --
// use EnsureAuthFirstProductionAt when the caller wants to mint an anchor on
// first read.
//
// ok is false when auth is nil, Metadata is empty, the key is absent, or the
// stored value cannot be parsed as a timestamp. Callers MUST treat that as
// "freshness unknown", not as "born just now" or "already mature": both a
// legacy credential that predates this change and a freshly loaded credential
// that has simply never yet been ensured look identical to a missing key, and
// only the caller (Phase 1/3 weight and warm-up logic, not this file) knows
// the right conservative fallback for its own use case.
func AuthFirstProductionAt(auth *Auth) (time.Time, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return time.Time{}, false
	}
	return parseFirstProductionAtValue(auth.Metadata[FirstProductionAtMetadataKey])
}

// EnsureAuthFirstProductionAt returns the account's first-production anchor,
// minting and persisting one -- stamped at `now`, in UTC, RFC3339-encoded --
// into auth.Metadata the first time it is called for a given credential, and
// leaving an existing valid anchor completely untouched on every subsequent
// call. This is the append-only "read-or-mint" primitive spec.md's "稳定的新
// 鲜度锚点" scenario requires: the anchor is set exactly once and never
// overwritten by a later call, regardless of how many times selection/scoring
// code re-derives an account's age.
//
// A previously stored value that fails to parse (corrupt/foreign-shaped data)
// is treated the same as "absent": it does not count as an existing anchor,
// so this call mints a fresh one and overwrites the corrupt value. This is a
// self-healing measure, not a violation of append-only semantics -- data that
// was never a valid anchor cannot be "preserved" as one.
//
// minted reports whether this call is the one that wrote the anchor (true =
// just minted from `now`; false = an existing anchor was returned unchanged).
//
// auth.Metadata already coexists with many other keys written by unrelated
// subsystems (quota_snapshot, claude_device_id, farm_enrolled, ...); this
// function only ever reads/writes FirstProductionAtMetadataKey and leaves
// every other key in the map exactly as it found it, so it is always safe to
// call regardless of what else has been stored on the record.
//
// now is caller-supplied (not time.Now()) so tests stay deterministic and so
// production call sites control exactly which real-world event ("first time
// this credential won a selection", "first time it completed a request",
// etc. -- decided by the Phase 1 integration that calls this, not this file)
// the anchor is stamped from.
//
// Callers that mutate a live, shared *Auth (rather than a private clone about
// to be persisted) are expected to already hold whatever lock protects
// concurrent Metadata mutation on that record, matching every other Metadata
// mutator in this package (see setAutoQuarantineMetadata in
// conductor_auto_quarantine.go).
func EnsureAuthFirstProductionAt(auth *Auth, now time.Time) (anchor time.Time, minted bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if existing, ok := parseFirstProductionAtValue(auth.Metadata[FirstProductionAtMetadataKey]); ok {
		return existing, false
	}
	stamped := now.UTC()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[FirstProductionAtMetadataKey] = stamped.Format(time.RFC3339)
	return stamped, true
}

// AccountAge returns how long it has been since auth's first-production
// anchor, as of `now`. ok is false when no anchor is recorded yet (see
// AuthFirstProductionAt) -- this function never mints one; callers that want
// read-or-mint semantics should call EnsureAuthFirstProductionAt first and
// derive age from its result themselves.
//
// A negative result (anchor in the future, e.g. clock skew or a test fixture
// mistake) is clamped to zero rather than returned negative: no caller of
// this package has a meaningful use for a negative account age, and warm-up
// curve lookups (Phase 3) expect a non-negative day count.
func AccountAge(auth *Auth, now time.Time) (time.Duration, bool) {
	anchor, ok := AuthFirstProductionAt(auth)
	if !ok {
		return 0, false
	}
	age := now.Sub(anchor)
	if age < 0 {
		age = 0
	}
	return age, true
}

// AccountAgeDays is AccountAge truncated to whole days, matching the day
// granularity config.AccountWarmupStage.MinAgeDays/MaxAgeDays are expressed
// in (internal/config/account_scheduling.go), so Phase 3 warm-up-curve stage
// lookups can compare an account's age against those bounds directly without
// re-deriving day math from a time.Duration themselves.
func AccountAgeDays(auth *Auth, now time.Time) (int, bool) {
	age, ok := AccountAge(auth, now)
	if !ok {
		return 0, false
	}
	return int(age / (24 * time.Hour)), true
}

// parseFirstProductionAtValue normalizes a raw auth.Metadata value for
// FirstProductionAtMetadataKey into a time.Time.
//
// It accepts two shapes: the persisted shape (an RFC3339 string, written by
// EnsureAuthFirstProductionAt and round-tripped through JSON on disk/DB), and
// a live time.Time (so an *Auth constructed directly in memory -- e.g. by a
// test, or by future in-process code that never goes through a marshal/
// unmarshal round trip -- behaves identically to the persisted-and-reloaded
// shape). Any other shape, a blank/whitespace-only string, a zero time.Time,
// or a string that fails RFC3339 parsing is treated as "not set".
func parseFirstProductionAtValue(raw any) (time.Time, bool) {
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v, true
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339, trimmed)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	default:
		return time.Time{}, false
	}
}
