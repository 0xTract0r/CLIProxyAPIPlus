package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestResolveClaudeDeviceProfile_ZeroInMemoryFloorsToOwnPersistedHighWaterTriple
// pins scenario D.1(2) at the helps unit level and isolates the per-account
// persisted-metadata safety net inside claudeFallbackBaseline: an account whose OWN
// auth.Metadata carries a persisted high-water triple ABOVE the frozen floor,
// resolved with an EMPTY in-memory observation map (the exact post-restart state)
// and WITHOUT any global seeding, must floor its outbound to that persisted version
// — as a COMPLETE atomic triple — instead of collapsing to the hardcoded floor
// (2.1.211).
//
// This is distinct from:
//   - Phase B startup aggregation (service.seedClaudeObservedHighWaterFromLoadedAuths),
//     which warms the GLOBAL fallback for OTHER zero-observation accounts, and
//   - the executor request wrapper (resolveClaudeDeviceProfileForRequest), which
//     calls SeedClaudeObservedHighWaterFromAuth first.
//
// helps.ResolveClaudeDeviceProfile itself never seeds the global observation map, so
// the ONLY path that can lift the version above the floor here is the per-account
// persisted read (claudePersistedHighWaterProfile) inside claudeFallbackBaseline.
// The test asserts the global map stays empty afterward to prove that isolation:
// the lift came solely from the account's own persisted triple, and a lower/absent
// global observation could never have supplied it.
func TestResolveClaudeDeviceProfile_ZeroInMemoryFloorsToOwnPersistedHighWaterTriple(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	// A real current-generation version comfortably above the hardcoded floor.
	// authWithPersistedHighWater (defined in claude_device_high_water_reseed_test.go)
	// persists pkg 0.80.0 / runtime v24.6.0 — both differ from the baseline default
	// constants, so an atomic-triple lift is observable.
	const persistedVersion = "2.1.230"
	auth := authWithPersistedHighWater("acct-own-persisted", persistedVersion)

	// Precondition: no live/global in-memory observation exists (cold post-restart).
	if _, has := globalClaudeObservedHighWaterVersion(); has {
		t.Fatalf("precondition: in-memory observation map must be empty after reset")
	}

	profile := ResolveClaudeDeviceProfile(auth, "", nil, &config.Config{})

	if got := profile.VersionString(); got != persistedVersion {
		t.Fatalf("version = %q, want own persisted high-water %q (per-account persisted read must lift above floor, not collapse to 2.1.211)", got, persistedVersion)
	}
	// The whole software triple is adopted atomically from the SAME persisted
	// observation, never spliced (high-water UA + stale baseline pkg/runtime).
	if got := profile.PackageVersion; got != "0.80.0" {
		t.Fatalf("PackageVersion = %q, want persisted 0.80.0 (atomic triple)", got)
	}
	if got := profile.RuntimeVersion; got != "v24.6.0" {
		t.Fatalf("RuntimeVersion = %q, want persisted v24.6.0 (atomic triple)", got)
	}
	if profile.PackageVersion == defaultClaudeFingerprintPackageVersion || profile.RuntimeVersion == defaultClaudeFingerprintRuntimeVersion {
		t.Fatalf("persisted high-water UA spliced with stale baseline pkg/runtime: %s / %s / %s (forbidden mismatched triple)", profile.UserAgent, profile.PackageVersion, profile.RuntimeVersion)
	}
	// Platform stays pinned to the proxy baseline (decoupled from software fingerprint).
	if profile.OS != defaultClaudeFingerprintOS || profile.Arch != defaultClaudeFingerprintArch {
		t.Fatalf("platform = %s/%s, want pinned baseline %s/%s", profile.OS, profile.Arch, defaultClaudeFingerprintOS, defaultClaudeFingerprintArch)
	}

	// Isolation proof: ResolveClaudeDeviceProfile must NOT have seeded the global
	// observation map; the per-account persisted read is the sole lift path here.
	if _, has := globalClaudeObservedHighWaterVersion(); has {
		t.Fatalf("ResolveClaudeDeviceProfile must not seed the global observation map; per-account persisted read is the sole lift path in this test")
	}
}
