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
// metadataKeyQuarantinedAt in conductor_auto_quarantine.go). It persists to
// whichever backend already stores Metadata (file store or Postgres store) --
// no new storage subsystem is introduced (design.md §6.4).
//
// As of the §8.5 namespace unification this const names the SUB-KEY inside the
// top-level account_scheduling object (AccountSchedulingMetadataKey): the anchor
// is WRITTEN to account_scheduling.first_production_at and READ via dual-read
// (namespaced sub-key preferred, legacy bare first_production_at key as
// fallback -- see readFirstProductionAt). It remains a top-level-reachable key
// either way, so it survives the quota refresh Clone (design.md §6.4).
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
	return readFirstProductionAt(auth.Metadata)
}

// readFirstProductionAt dual-reads the first-production anchor (design §8.5 /
// spec.md "老裸键 dual-read 迁移"): it prefers the namespaced
// account_scheduling.first_production_at sub-key and falls back to the legacy
// bare top-level first_production_at key so credentials written before the §8.5
// namespace unification keep resolving. A present-but-unparseable value in the
// new location transparently falls back to a valid legacy value rather than
// masking it. ok is false when neither location holds a parseable timestamp.
func readFirstProductionAt(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	if obj, ok := accountSchedulingObject(meta); ok {
		if parsed, ok := parseFirstProductionAtValue(obj[FirstProductionAtMetadataKey]); ok {
			return parsed, true
		}
	}
	return parseFirstProductionAtValue(meta[FirstProductionAtMetadataKey])
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
	if existing, ok := readFirstProductionAt(auth.Metadata); ok {
		return existing, false
	}
	stamped := now.UTC()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	// Write ONLY to the namespaced location (design §8.5: "写入只走新位置");
	// dual-read above keeps honoring any legacy bare key, so this is a
	// non-destructive migration -- an existing legacy anchor is never rewritten,
	// and a brand-new anchor lands under account_scheduling.first_production_at.
	setAccountSchedulingValue(auth.Metadata, FirstProductionAtMetadataKey, stamped.Format(time.RFC3339))
	return stamped, true
}

// SetAccountFirstProductionAt is the OPERATOR-EXPLICIT override channel for the
// first-production anchor -- deliberately a SEPARATE path from
// EnsureAuthFirstProductionAt's append-only auto-mint. Where
// EnsureAuthFirstProductionAt stamps the anchor exactly once (on the credential's
// first real serving success) and never overwrites it afterward, this writer
// UNCONDITIONALLY sets the anchor to the caller-supplied instant, overwriting any
// existing value.
//
// Its sole purpose is a one-shot operational migration: an account that was
// already aged/in-production BEFORE adaptive scheduling was turned on has no
// anchor yet, so the auto-mint would stamp it as brand-new the first time it
// serves under adaptive and clamp it to the most restrictive warm-up stage. An
// operator uses this to backfill such an account's TRUE first-production date so
// its warm-up stage / maturity reflect reality. Callers are expected to only
// backfill dates they have actually confirmed (see the management handler doc):
// setting an anchor earlier than the truth makes an account look more mature than
// it is (less warm-up), which is the direction that carries account-safety risk.
//
// It writes ONLY to the namespaced account_scheduling.first_production_at sub-key
// -- the exact location EnsureAuthFirstProductionAt writes (design §8.5) -- in
// UTC, RFC3339-encoded, so an operator-set anchor and an auto-minted anchor are
// byte-for-byte the same shape and round-trip identically through
// readFirstProductionAt / the quota-refresh Clone. Because both paths read/write
// the same key, an explicit set is subsequently HONORED by the auto-mint (the
// next EnsureAuthFirstProductionAt sees a present anchor and returns it with
// minted=false, never clobbering the operator's value): the two paths cannot
// fight over the anchor.
//
// t must be a non-zero, not-future instant; validating that is the caller's job
// (see PatchAuthFileAccountScheduling), mirroring SetAccountTierOverride /
// SetAccountRateScale which also trust a pre-validated value. Callers mutating a
// live, shared *Auth must hold the same lock every other Metadata mutator in this
// package expects.
func (a *Auth) SetAccountFirstProductionAt(t time.Time) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	setAccountSchedulingValue(a.Metadata, FirstProductionAtMetadataKey, t.UTC().Format(time.RFC3339))
}

// ClearAccountFirstProductionAt removes the first-production anchor from BOTH the
// namespaced account_scheduling object and the legacy bare top-level key (see
// clearAccountSchedulingValue for why both must be cleared -- otherwise a stale
// legacy bare value would resurface via readFirstProductionAt's dual-read).
//
// It is the inverse of SetAccountFirstProductionAt and deliberately RE-OPENS the
// append-only auto-mint path: with no anchor present in either location, the next
// serving success routed through EnsureAuthFirstProductionAt mints a fresh anchor
// stamped at that moment. An operator uses it to undo an override and hand
// freshness tracking back to the automatic mechanism.
func (a *Auth) ClearAccountFirstProductionAt() {
	if a == nil || a.Metadata == nil {
		return
	}
	clearAccountSchedulingValue(a.Metadata, FirstProductionAtMetadataKey)
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
