package auth

import "time"

// This file implements openspec/changes/fix-account-origin-anchors: a
// read-only projection of the Anthropic account/organization creation
// timestamp already persisted inside an auth's polled quota snapshot
// (Metadata["quota_snapshot"]["profile"]), surfaced alongside the existing
// anchor_candidates block (see
// internal/api/handlers/management/auth_files_adaptive_scheduling.go).
//
// This is a pure reader: it never mints a value, never falls back to
// first_production_at / RuntimeIdentityState / time.Now, and never mutates
// auth or its Metadata. It only exposes data Anthropic itself already
// reported about the account, which the quota poller (quota_snapshots.go)
// already fetches via GET /api/oauth/profile and persists verbatim.

// AccountCreatedAt returns the Anthropic-reported account creation instant
// for auth, read from its most recently persisted quota_snapshot.profile:
//   - primary source: profile.account.created_at (the Anthropic account's own
//     creation time);
//   - fallback: profile.organization.subscription_created_at (the
//     organization/subscription creation time), used only when the primary
//     source is absent or unparseable.
//
// ok is false when auth is nil, no quota_snapshot.profile has been persisted
// yet, or neither field parses as a timestamp. Callers MUST treat ok=false as
// "unknown" -- never substitute first_production_at, RuntimeIdentityState's
// created_at, or the current wall-clock time, all of which describe when
// this system started tracking the account, not when Anthropic created it.
func AccountCreatedAt(auth *Auth) (time.Time, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return time.Time{}, false
	}
	profile := nestedMetadataObject(auth.Metadata, accountQuotaSnapshotMetadataKey, "profile")
	if len(profile) == 0 {
		return time.Time{}, false
	}
	if ts, ok := parseTimeValue(nestedMetadataString(profile, "account", "created_at")); ok && !ts.IsZero() {
		return ts, true
	}
	if ts, ok := parseTimeValue(nestedMetadataString(profile, "organization", "subscription_created_at")); ok && !ts.IsZero() {
		return ts, true
	}
	return time.Time{}, false
}
