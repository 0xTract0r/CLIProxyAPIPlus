package auth

import (
	"testing"
	"time"
)

// fixedGateNow is a deterministic reference time for the container-liveness
// tests. Heartbeats are expressed relative to it.
var fixedGateNow = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// withFixedGateClock overrides the package-level provisionedGateNow indirection
// so forkRequireProvisionedBlocked's liveness branch compares against a
// deterministic clock, and restores the real clock on cleanup.
func withFixedGateClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := provisionedGateNow
	provisionedGateNow = func() time.Time { return now }
	t.Cleanup(func() { provisionedGateNow = prev })
}

// claudeAuthEnrolledWithAliveAt builds a farm-enrolled Claude account whose live
// Attributes carry the given container-liveness heartbeat (RFC3339 string). An
// empty aliveAt leaves the heartbeat attribute absent (missing heartbeat case).
func claudeAuthEnrolledWithAliveAt(aliveAt string) *Auth {
	auth := &Auth{ID: "claude-acct", Provider: "claude", Status: StatusActive}
	auth.Metadata = map[string]any{FarmEnrolledMetadataKey: true}
	if aliveAt != "" {
		auth.Attributes = map[string]string{FarmContainerAliveAtAttributeKey: aliveAt}
	}
	return auth
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// TestBlockReasonContainerNotAlive_DistinctBit is the collision guard for the
// reserved fork-only block reasons: the container-liveness reason (1<<17) must
// never collide with the provisioning reason (1<<16) or any upstream iota
// reason. It also documents that the constant is intentionally referenced.
func TestBlockReasonContainerNotAlive_DistinctBit(t *testing.T) {
	if blockReasonContainerNotAlive == blockReasonUnprovisioned {
		t.Fatalf("blockReasonContainerNotAlive == blockReasonUnprovisioned; the two fork reasons must be distinct")
	}
	for _, r := range []blockReason{blockReasonNone, blockReasonCooldown, blockReasonDisabled, blockReasonOther} {
		if blockReasonContainerNotAlive == r {
			t.Fatalf("blockReasonContainerNotAlive collides with an upstream iota reason %v", r)
		}
	}
	if blockReasonContainerNotAlive != 1<<17 {
		t.Fatalf("blockReasonContainerNotAlive = %d, want %d (1<<17)", blockReasonContainerNotAlive, 1<<17)
	}
}

// TestFarmRequireContainerAliveEnabled_Parsing confirms the alive-gate env
// truthy parsing matches the sibling FARM_REQUIRE_PROVISIONED toggle.
func TestFarmRequireContainerAliveEnabled_Parsing(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "Yes", "on", " on "}
	for _, v := range truthy {
		t.Run("on/"+v, func(t *testing.T) {
			t.Setenv(FarmRequireContainerAliveEnvVar, v)
			if !farmRequireContainerAliveEnabled() {
				t.Fatalf("farmRequireContainerAliveEnabled = false for %q, want true", v)
			}
		})
	}
	falsy := []string{"", "0", "false", "off", "no", "garbage"}
	for _, v := range falsy {
		t.Run("off/"+v, func(t *testing.T) {
			t.Setenv(FarmRequireContainerAliveEnvVar, v)
			if farmRequireContainerAliveEnabled() {
				t.Fatalf("farmRequireContainerAliveEnabled = true for %q, want false", v)
			}
		})
	}
}

// TestAuthContainerRecentlyAlive covers the freshness predicate directly (it
// takes an explicit now, so no clock injection is needed here).
func TestAuthContainerRecentlyAlive(t *testing.T) {
	now := fixedGateNow
	cases := []struct {
		name    string
		auth    *Auth
		want    bool
		comment string
	}{
		{name: "fresh", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: rfc3339(now.Add(-1 * time.Minute))}}, want: true},
		{name: "boundary_exactly_at_threshold", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: rfc3339(now.Add(-FarmContainerAliveStaleThreshold))}}, want: true},
		{name: "expired_just_over_threshold", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: rfc3339(now.Add(-FarmContainerAliveStaleThreshold - time.Second))}}, want: false},
		{name: "expired_far", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: rfc3339(now.Add(-30 * time.Minute))}}, want: false},
		{name: "future_clock_skew_is_fresh", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: rfc3339(now.Add(2 * time.Minute))}}, want: true},
		{name: "missing_attribute", auth: &Auth{Attributes: map[string]string{"other": "x"}}, want: false},
		{name: "empty_value", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: "  "}}, want: false},
		{name: "invalid_value", auth: &Auth{Attributes: map[string]string{FarmContainerAliveAtAttributeKey: "not-a-timestamp"}}, want: false},
		{name: "nil_attributes", auth: &Auth{}, want: false},
		{name: "nil_auth", auth: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authContainerRecentlyAlive(tc.auth, now); got != tc.want {
				t.Fatalf("authContainerRecentlyAlive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestForkRequireContainerAlive_BothFlagsOffIsNoop is the strict no-op guard: an
// enrolled Claude account with a stale/missing heartbeat must NOT be blocked
// when neither farm flag is armed, so serving stays byte-identical.
func TestForkRequireContainerAlive_BothFlagsOffIsNoop(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "")
	t.Setenv(FarmRequireContainerAliveEnvVar, "")
	withFixedGateClock(t, fixedGateNow)

	auth := claudeAuthEnrolledWithAliveAt("") // enrolled, no heartbeat
	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true with both flags off, want false (strict no-op)")
	}
	blocked, reason, next := isAuthBlockedForModel(auth, "", fixedGateNow)
	if blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("isAuthBlockedForModel = (%v,%v,%v) with both flags off, want (false,none,zero)", blocked, reason, next)
	}
}

// TestForkRequireContainerAlive_AliveArmedOnly exercises the container-liveness
// sub-gate in isolation (provisioning flag OFF): a fresh heartbeat is allowed;
// expired/missing/invalid heartbeats fail closed.
func TestForkRequireContainerAlive_AliveArmedOnly(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "") // provisioning sub-gate off
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	cases := []struct {
		name        string
		aliveAt     string
		wantBlocked bool
	}{
		{name: "fresh_allows", aliveAt: rfc3339(fixedGateNow.Add(-1 * time.Minute)), wantBlocked: false},
		{name: "expired_blocks", aliveAt: rfc3339(fixedGateNow.Add(-10 * time.Minute)), wantBlocked: true},
		{name: "missing_blocks", aliveAt: "", wantBlocked: true},
		{name: "invalid_blocks", aliveAt: "garbage", wantBlocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := claudeAuthEnrolledWithAliveAt("")
			if tc.aliveAt != "" {
				auth.Attributes = map[string]string{FarmContainerAliveAtAttributeKey: tc.aliveAt}
			}
			if got := forkRequireProvisionedBlocked(auth); got != tc.wantBlocked {
				t.Fatalf("forkRequireProvisionedBlocked = %v, want %v", got, tc.wantBlocked)
			}
		})
	}
}

// TestForkRequireContainerAlive_UnenrolledImmune confirms account-scoping: with
// the alive gate armed, a never-enrolled Claude account (which today means every
// pre-existing/production account) is passed through unconditionally even with
// no heartbeat.
func TestForkRequireContainerAlive_UnenrolledImmune(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "")
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	auth := &Auth{ID: "claude-acct", Provider: "claude", Status: StatusActive} // no farm_enrolled, no heartbeat
	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an unenrolled account, want false (gate must be a no-op for non-enrolled accounts)")
	}
	// Explicit-false enrollment must behave identically to metadata-absent.
	auth.Metadata = map[string]any{FarmEnrolledMetadataKey: false}
	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for an explicitly-unenrolled account, want false")
	}
}

// TestForkRequireContainerAlive_NonClaudeImmune confirms the alive gate is
// Claude-scoped: a farm-enrolled Codex account with no heartbeat is never
// fail-closed by it.
func TestForkRequireContainerAlive_NonClaudeImmune(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "")
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	auth := &Auth{ID: "codex-acct", Provider: "codex", Status: StatusActive, Metadata: map[string]any{FarmEnrolledMetadataKey: true}}
	if forkRequireProvisionedBlocked(auth) {
		t.Fatalf("forkRequireProvisionedBlocked = true for a non-Claude account, want false (alive gate is Claude-scoped)")
	}
}

// TestForkRequireContainerAlive_BothSubGatesArmed confirms the two sub-gates
// compose independently: an enrolled Claude account is blocked if it fails
// EITHER armed sub-gate, and allowed only when it satisfies both.
func TestForkRequireContainerAlive_BothSubGatesArmed(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "1")
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	fresh := rfc3339(fixedGateNow.Add(-1 * time.Minute))
	stale := rfc3339(fixedGateNow.Add(-10 * time.Minute))

	// bound device_id + fresh heartbeat -> allowed
	ok := claudeAuthEnrolledWithOverride(validProvisionedDeviceID)
	ok.Attributes[FarmContainerAliveAtAttributeKey] = fresh
	if forkRequireProvisionedBlocked(ok) {
		t.Fatalf("bound+fresh account was blocked, want allowed")
	}

	// bound device_id but stale heartbeat -> blocked by liveness sub-gate
	staleAuth := claudeAuthEnrolledWithOverride(validProvisionedDeviceID)
	staleAuth.Attributes[FarmContainerAliveAtAttributeKey] = stale
	if !forkRequireProvisionedBlocked(staleAuth) {
		t.Fatalf("bound but stale-heartbeat account was allowed, want blocked (liveness sub-gate)")
	}

	// fresh heartbeat but no device_id binding -> blocked by provisioning sub-gate
	unboundFresh := claudeAuthEnrolledWithOverride("")
	unboundFresh.Attributes = map[string]string{FarmContainerAliveAtAttributeKey: fresh}
	if !forkRequireProvisionedBlocked(unboundFresh) {
		t.Fatalf("unprovisioned but fresh-heartbeat account was allowed, want blocked (provisioning sub-gate)")
	}
}

// TestForkRequireContainerAlive_SelectorSkipsStale confirms the alive sub-gate
// is wired through the same selection chokepoint: an armed, enrolled, stale
// account is skipped entirely with no time-based retry. The specific reason
// reported by the selector is intentionally not asserted here — the selector
// currently maps every farm-gate skip to blockReasonUnprovisioned, and threading
// the distinct blockReasonContainerNotAlive is out of scope for this slice.
func TestForkRequireContainerAlive_SelectorSkipsStale(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "")
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	auth := claudeAuthEnrolledWithAliveAt(rfc3339(fixedGateNow.Add(-30 * time.Minute)))
	blocked, reason, next := isAuthBlockedForModel(auth, "", fixedGateNow)
	if !blocked {
		t.Fatalf("isAuthBlockedForModel blocked = false for a stale-heartbeat account, want true")
	}
	if reason == blockReasonNone {
		t.Fatalf("isAuthBlockedForModel reason = none for a blocked account, want a non-none block reason")
	}
	if !next.IsZero() {
		t.Fatalf("isAuthBlockedForModel next = %v, want zero (liveness recovery is an external heartbeat, not a timer)", next)
	}
}

// TestApplyRuntimeFieldsFromMetadataMirrorsFarmAliveAt confirms the hydrate path
// mirrors the persisted heartbeat from Metadata into the Attributes mirror the
// gate reads.
func TestApplyRuntimeFieldsFromMetadataMirrorsFarmAliveAt(t *testing.T) {
	aliveAt := rfc3339(fixedGateNow)
	auth := &Auth{
		Metadata: map[string]any{
			FarmContainerAliveAtMetadataKey: aliveAt,
		},
	}
	ApplyRuntimeFieldsFromMetadata(auth)
	if got := auth.Attributes[FarmContainerAliveAtAttributeKey]; got != aliveAt {
		t.Fatalf("Attributes[farm_container_alive_at] = %q, want %q (hydrate must mirror Metadata)", got, aliveAt)
	}
}

// TestApplyRuntimeFieldsFromMetadataClearsStaleFarmAliveAtWhenCleared confirms
// that clearing the persisted heartbeat to an empty string removes a previously
// mirrored Attributes value on a live, already-hydrated Auth object (matching
// the claude_device_id clear semantics), so the gate reliably sees not-alive.
func TestApplyRuntimeFieldsFromMetadataClearsStaleFarmAliveAtWhenCleared(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			FarmContainerAliveAtMetadataKey: "",
			// Keep metadata non-empty so the top-level guard in
			// ApplyRuntimeFieldsFromMetadata still runs.
			"note": "keep-metadata-non-empty",
		},
		Attributes: map[string]string{
			FarmContainerAliveAtAttributeKey: rfc3339(fixedGateNow),
		},
	}
	ApplyRuntimeFieldsFromMetadata(auth)
	if _, ok := auth.Attributes[FarmContainerAliveAtAttributeKey]; ok {
		t.Fatalf("expected stale farm_container_alive_at attribute cleared, got %#v", auth.Attributes)
	}
}

// TestGateReadsHydratedFarmAliveAt is the end-to-end binding: a persisted
// heartbeat hydrated via ApplyRuntimeFieldsFromMetadata drives the armed gate's
// decision, proving the metadata->attribute->gate chain is intact.
func TestGateReadsHydratedFarmAliveAt(t *testing.T) {
	t.Setenv(FarmRequireProvisionedEnvVar, "")
	t.Setenv(FarmRequireContainerAliveEnvVar, "1")
	withFixedGateClock(t, fixedGateNow)

	fresh := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		FarmEnrolledMetadataKey:         true,
		FarmContainerAliveAtMetadataKey: rfc3339(fixedGateNow.Add(-1 * time.Minute)),
	}}
	ApplyRuntimeFieldsFromMetadata(fresh)
	if forkRequireProvisionedBlocked(fresh) {
		t.Fatalf("hydrated fresh-heartbeat account was blocked, want allowed")
	}

	stale := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		FarmEnrolledMetadataKey:         true,
		FarmContainerAliveAtMetadataKey: rfc3339(fixedGateNow.Add(-1 * time.Hour)),
	}}
	ApplyRuntimeFieldsFromMetadata(stale)
	if !forkRequireProvisionedBlocked(stale) {
		t.Fatalf("hydrated stale-heartbeat account was allowed, want blocked")
	}
}
