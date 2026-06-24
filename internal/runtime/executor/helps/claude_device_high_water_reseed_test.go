package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// authWithPersistedHighWater builds an *cliproxyauth.Auth whose Metadata carries a
// persisted claude_device_high_water triple in the map[string]string shape that
// the token store round-trips back on restart (decoded-from-disk form). This
// mirrors the post-restart state: in-memory observation map empty, but the auth
// still carries the previously persisted real high-water.
func authWithPersistedHighWater(id, version string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "claude",
		ProxyURL: "direct",
		Metadata: map[string]any{
			cliproxyauth.ClaudeDeviceHighWaterMetadataKey: map[string]string{
				"user_agent":      "claude-cli/" + version + " (external, cli)",
				"version":         version,
				"package_version": "0.80.0",
				"runtime_version": "v24.6.0",
				"os":              "MacOS",
				"arch":            "arm64",
				"source":          "observed",
			},
		},
	}
}

// TestSeedClaudeObservedHighWaterFromAuth_DisarmsStaleGuardAfterRestart is the
// regression guard for the restart-first-request stale-guard false positive: with
// an empty in-memory observation map (fresh process) and stabilize on / no operator
// baseline UA, the stale-guard predicate fires even though the auth carries a real
// persisted high-water that the outbound floor path already uses. Re-seeding the
// persisted triple into the in-memory observation map must align the predicate's
// view with disk so the misleading "falls back to frozen floor 2.1.63" warning no
// longer fires, while keeping only-up semantics.
func TestSeedClaudeObservedHighWaterFromAuth_DisarmsStaleGuardAfterRestart(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	stabilize := true
	offline := false
	staleProneCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
		},
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &offline},
	}

	// Post-restart: in-memory observation map is empty, so the guard reports
	// stale even though the persisted high-water (2.1.185) is what the outbound
	// path actually emits. This is exactly the false positive being fixed.
	if _, has := globalClaudeObservedHighWaterVersion(); has {
		t.Fatalf("precondition: in-memory observation map must be empty after reset")
	}
	if !ClaudeDeviceProfileStaleGuardActive(staleProneCfg) {
		t.Fatalf("precondition: stale guard must be active before re-seed (empty in-memory map)")
	}

	auth := authWithPersistedHighWater("acct-restart", "2.1.185")
	if !SeedClaudeObservedHighWaterFromAuth(auth) {
		t.Fatalf("SeedClaudeObservedHighWaterFromAuth returned false for an auth with a valid persisted high-water")
	}

	// After re-seed the in-memory view equals the disk/outbound view: the global
	// observed high-water reflects 2.1.185 and the guard no longer reports stale.
	seeded, has := globalClaudeObservedHighWaterVersion()
	if !has {
		t.Fatalf("global observed high-water missing after re-seed")
	}
	if got := formatClaudeCLIVersion(seeded); got != "2.1.185" {
		t.Fatalf("re-seeded global high-water = %q, want %q", got, "2.1.185")
	}
	if ClaudeDeviceProfileStaleGuardActive(staleProneCfg) {
		t.Fatalf("stale guard must be disarmed after re-seeding persisted high-water (no more frozen-floor false positive)")
	}
}

// TestSeedClaudeObservedHighWaterFromAuth_OnlyUp confirms re-seeding never lowers
// the global observed high-water: seeding a persisted value lower than an existing
// live observation must leave the high-water at the higher live value.
func TestSeedClaudeObservedHighWaterFromAuth_OnlyUp(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	// A live first-party observation lands at 2.1.200 on some account.
	_ = ResolveClaudeDeviceProfile(
		&cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-live", Provider: "claude"},
		"",
		map[string][]string{"User-Agent": {"claude-cli/2.1.200 (external, cli)"}},
		&config.Config{},
	)
	live, has := globalClaudeObservedHighWaterVersion()
	if !has || formatClaudeCLIVersion(live) != "2.1.200" {
		t.Fatalf("precondition: live observation must seed global high-water to 2.1.200, got has=%v ver=%q", has, formatClaudeCLIVersion(live))
	}

	// Re-seeding a lower persisted triple (2.1.185) must not lower the high-water.
	if !SeedClaudeObservedHighWaterFromAuth(authWithPersistedHighWater("acct-restart", "2.1.185")) {
		t.Fatalf("SeedClaudeObservedHighWaterFromAuth returned false for a valid persisted high-water")
	}
	after, has := globalClaudeObservedHighWaterVersion()
	if !has {
		t.Fatalf("global observed high-water missing after re-seed")
	}
	if got := formatClaudeCLIVersion(after); got != "2.1.200" {
		t.Fatalf("re-seed must be only-up: global high-water = %q, want %q (lower persisted value must not lower it)", got, "2.1.200")
	}
}

// TestSeedClaudeObservedHighWaterFromAuth_NoPersistedReturnsFalse confirms that an
// auth with no persisted high-water is a no-op (returns false, leaves the map
// empty), so the stale guard correctly stays active until a real client is seen.
func TestSeedClaudeObservedHighWaterFromAuth_NoPersistedReturnsFalse(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	if SeedClaudeObservedHighWaterFromAuth(nil) {
		t.Fatalf("nil auth must return false")
	}
	if SeedClaudeObservedHighWaterFromAuth(&cliproxyauth.Auth{ID: "acct-empty", Provider: "claude"}) {
		t.Fatalf("auth without persisted high-water must return false")
	}
	if _, has := globalClaudeObservedHighWaterVersion(); has {
		t.Fatalf("no-op seed must leave the in-memory observation map empty")
	}
}
