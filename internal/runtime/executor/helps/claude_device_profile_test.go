package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestResolveClaudeDeviceProfile_OnlineRegistryNeverExceedsRealObservation
// pins the high-water model (requirement ⑥, plan A): online-update (npm latest)
// is never a ceiling. For a zero-observation account it must NOT inflate the
// outbound version to npm latest; the floor stays at the static/operator
// baseline. Once a real client is observed, npm is only ever a floor reference
// capped to that real observed high-water (min(npm, observed)).
func TestResolveClaudeDeviceProfile_OnlineRegistryNeverExceedsRealObservation(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	// npm latest is far ahead of any client this deployment has ever seen.
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "2.9.9",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:       managedHeaderProfileSourceNPM,
				SourceURL:    claudeCodeNPMURL,
				CheckedAt:    "2026-04-29T12:00:00Z",
				Completeness: "partial-cli-version-only",
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	}

	// Zero observation on this account and globally: npm must NOT be used as a
	// ceiling; the floor stays at the static baseline constant.
	zeroObs := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-zero-observation-auth",
		Provider: "claude",
	}, "", nil, cfg)
	if got := zeroObs.VersionString(); got != "2.1.63" {
		t.Fatalf("zero-observation version = %q, want static floor 2.1.63 (npm must not be a ceiling)", got)
	}
	if got := zeroObs.UserAgent; !strings.Contains(got, "claude-cli/2.1.63") {
		t.Fatalf("zero-observation UserAgent = %q, want static floor, not npm latest", got)
	}

	// Now observe a real first-party client below npm latest on a different
	// account; npm must be capped to that real observed high-water (2.1.100),
	// never lifted to npm latest 2.9.9.
	_ = ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-real-client-auth",
		Provider: "claude",
	}, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.100 (external, cli)"},
	}, cfg)

	cappedFallback := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-zero-observation-auth",
		Provider: "claude",
	}, "", nil, cfg)
	if got := cappedFallback.VersionString(); got != "2.1.100" {
		t.Fatalf("fallback version = %q, want global observed high-water 2.1.100 (npm capped to real)", got)
	}
	// Platform/software fingerprint stays pinned to the static baseline.
	if got := cappedFallback.OS; got != defaultClaudeFingerprintOS {
		t.Fatalf("OS = %q, want pinned baseline", got)
	}
	if got := cappedFallback.Arch; got != defaultClaudeFingerprintArch {
		t.Fatalf("Arch = %q, want pinned baseline", got)
	}
}

func TestResolveClaudeDeviceProfile_PrefersObservedClaudeCLIOverNewerOnlineFallback(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "2.1.144",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:       managedHeaderProfileSourceNPM,
				SourceURL:    claudeCodeNPMURL,
				CheckedAt:    "2026-05-19T12:00:00Z",
				Completeness: "partial-cli-version-only",
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-observed-auth",
		Provider: "claude",
	}, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": {"0.80.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
	}, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if got := profile.UserAgent; got != "claude-cli/2.1.142 (external, cli)" {
		t.Fatalf("UserAgent = %q, want observed local Claude CLI version", got)
	}
	if got := profile.PackageVersion; got != "0.80.0" {
		t.Fatalf("PackageVersion = %q, want observed package version", got)
	}
	if got := profile.RuntimeVersion; got != "v24.5.0" {
		t.Fatalf("RuntimeVersion = %q, want observed runtime version", got)
	}
	if got := profile.OS; got != defaultClaudeFingerprintOS {
		t.Fatalf("OS = %q, want pinned baseline", got)
	}
	if got := profile.Arch; got != defaultClaudeFingerprintArch {
		t.Fatalf("Arch = %q, want pinned baseline", got)
	}
}

func TestResolveClaudeDeviceProfile_AllowsObservedCLIWhenConfiguredFallbackIsNewer(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		FileName: "claude-configured-fallback.json",
		Provider: "claude",
	}
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.144 (external, cli)",
			PackageVersion: "0.96.0",
			RuntimeVersion: "v24.5.0",
		},
	}

	profile := ResolveClaudeDeviceProfile(auth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.140 (external, cli)"},
		"X-Stainless-Package-Version": {"0.92.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	if got := profile.UserAgent; got != "claude-cli/2.1.140 (external, cli)" {
		t.Fatalf("UserAgent = %q, want observed local Claude CLI version", got)
	}
	observations := ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.140" {
		t.Fatalf("observation version = %q, want 2.1.140", got)
	}
}

func TestClaudeDeviceProfileObservations_TracksRecentClientVersionsPerAuth(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-observation-auth", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.140 (external, cli)"},
		"X-Stainless-Package-Version": {"0.80.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": {"0.81.0"},
		"X-Stainless-Runtime-Version": {"v24.6.0"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.140 (external, cli)"},
		"X-Stainless-Package-Version": {"0.80.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)

	observations := ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 2 {
		t.Fatalf("observations length = %d, want 2: %#v", len(observations), observations)
	}
	byVersion := make(map[string]ClaudeDeviceProfileObservation)
	for _, observation := range observations {
		byVersion[observation.Version] = observation
	}
	if got := byVersion["2.1.140"].RequestCount; got != 2 {
		t.Fatalf("2.1.140 request_count = %d, want 2: %#v", got, byVersion["2.1.140"])
	}
	if got := byVersion["2.1.142"].RequestCount; got != 1 {
		t.Fatalf("2.1.142 request_count = %d, want 1: %#v", got, byVersion["2.1.142"])
	}
	if byVersion["2.1.140"].LastSeenAt == "" || byVersion["2.1.140"].FirstSeenAt == "" {
		t.Fatalf("expected first/last seen timestamps: %#v", byVersion["2.1.140"])
	}
}

func TestClaudeDeviceProfileObservations_FileBackedAuthVisibleWithoutAPIKey(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "runtime-id-from-loader", FileName: "claude-file-auth.json", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(auth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	observations := ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.142" {
		t.Fatalf("version = %q, want 2.1.142", got)
	}
	if got := observations[0].RequestCount; got != 1 {
		t.Fatalf("request_count = %d, want 1", got)
	}

	sameFileDifferentRuntimeID := &cliproxyauth.Auth{ProxyURL: "direct", ID: "management-id-from-loader", FileName: "claude-file-auth.json", Provider: "claude"}
	observations = ClaudeDeviceProfileObservations(sameFileDifferentRuntimeID, "")
	if len(observations) != 1 {
		t.Fatalf("same file observations length = %d, want 1: %#v", len(observations), observations)
	}
}

func TestClaudeDeviceProfileObservations_FileNameAuthIDAliases(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-file-auth.json", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.144 (external, cli)"},
		"X-Stainless-Package-Version": {"0.96.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "claude-file-auth.json", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.144" {
		t.Fatalf("version = %q, want 2.1.144", got)
	}
}

func TestClaudeDeviceProfileObservations_LabelAlias(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ProxyURL: "direct", Label: "bcd898@example.com", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", Label: "bcd898@example.com", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.142" {
		t.Fatalf("version = %q, want 2.1.142", got)
	}
}

func TestClaudeDeviceProfileObservations_GlobalFallbackForUnidentifiedAuth(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ProxyURL: "direct", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "provider-token", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.141 (external, cli)"},
		"X-Stainless-Package-Version": {"0.93.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "claude-file-auth.json", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.141" {
		t.Fatalf("version = %q, want 2.1.141", got)
	}
}

// TestResolveClaudeDeviceProfile_HighWaterCapsToObservedNotNpm covers requirement
// ⑥ plan A case (a): an account whose only real observation is claude-cli/2.1.173
// has a high-water of exactly 2.1.173. Even with online-update enabled and npm
// latest ahead (2.5.0), the cached high-water must NOT be inflated past the real
// observed value on subsequent requests.
func TestResolveClaudeDeviceProfile_HighWaterCapsToObservedNotNpm(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "2.5.0",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:    managedHeaderProfileSourceNPM,
				SourceURL: claudeCodeNPMURL,
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &online},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-173-auth", Provider: "claude"}

	// First request: real client 2.1.173 is observed and becomes the high-water.
	first := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.173 (external, cli)"},
	}, cfg)
	if got := first.VersionString(); got != "2.1.173" {
		t.Fatalf("first request version = %q, want observed 2.1.173", got)
	}

	// Subsequent request without a client UA: must reuse the cached 2.1.173
	// high-water, never the newer npm latest 2.5.0.
	cached := ResolveClaudeDeviceProfile(auth, "", nil, cfg)
	if got := cached.VersionString(); got != "2.1.173" {
		t.Fatalf("cached version = %q, want observed high-water 2.1.173 (npm must not inflate)", got)
	}

	// A second real client at 2.1.170 (older) must not lower the high-water.
	lower := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.170 (external, cli)"},
	}, cfg)
	if got := lower.VersionString(); got != "2.1.173" {
		t.Fatalf("after older client, version = %q, want high-water 2.1.173 (only-up)", got)
	}
}

// TestResolveClaudeDeviceProfile_ZeroObservationDoesNotReportNpmLatest covers
// case (b): an account with no observation (and no global observation) and
// online-update ON must fall back to the static floor, never npm latest.
func TestResolveClaudeDeviceProfile_ZeroObservationDoesNotReportNpmLatest(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{Version: "2.7.0"}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &online},
	}

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-empty-auth", Provider: "claude"}, "", nil, cfg)
	if got := profile.VersionString(); got != "2.1.63" {
		t.Fatalf("zero-observation version = %q, want static floor 2.1.63, never npm latest", got)
	}
}

// TestResolveClaudeDeviceProfile_OnlineUpdateDisabledByDefaultBehavior covers
// case (c): with a config that does not enable online-update (the new default),
// the fetch override must never be consulted and a zero-observation account stays
// at the static floor.
func TestResolveClaudeDeviceProfile_OnlineUpdateDisabledByDefaultBehavior(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	oldOverride := ManagedHeaderOnlineFetchOverride
	consulted := false
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		consulted = true
		return managedHeaderOnlineVersion{Version: "2.8.0"}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	// online-update left unset (nil) => disabled, matching the new loader default.
	cfg := &config.Config{}

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-default-auth", Provider: "claude"}, "", nil, cfg)
	if consulted {
		t.Fatalf("online registry must not be consulted when online-update is disabled")
	}
	if got := profile.VersionString(); got != "2.1.63" {
		t.Fatalf("version = %q, want static floor 2.1.63 with online-update off", got)
	}
}

// TestResolveClaudeDeviceProfile_OnlyUpStillHoldsForNewerRealClient covers case
// (d): a genuinely newer real client raises the high-water (only-up), and an
// older real client afterwards does not lower it.
func TestResolveClaudeDeviceProfile_OnlyUpStillHoldsForNewerRealClient(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-onlyup-auth", Provider: "claude"}

	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.100 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.100" {
		t.Fatalf("version = %q, want 2.1.100", got)
	}

	// Newer real client raises the high-water.
	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.180 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.180" {
		t.Fatalf("version = %q, want raised high-water 2.1.180", got)
	}

	// Older real client must not lower it.
	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.150 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.180" {
		t.Fatalf("version = %q, want retained high-water 2.1.180", got)
	}

	// No-client fallback keeps the high-water.
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.180" {
		t.Fatalf("fallback version = %q, want retained high-water 2.1.180", got)
	}
}

// TestResolveClaudeDeviceProfile_PerAccountHighWaterNotLiftedByGlobal confirms an
// account that has its OWN real observation uses its own per-account high-water as
// the ceiling and is NOT lifted to a higher version observed on a different
// account. The global observed high-water is only a zero-observation fallback.
func TestResolveClaudeDeviceProfile_PerAccountHighWaterNotLiftedByGlobal(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	authA := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-a", Provider: "claude"}
	authB := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-b", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(authA, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.100 (external, cli)"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(authB, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.180 (external, cli)"},
	}, cfg)

	// Account A's fallback must stay at its own high-water 2.1.100, never the
	// higher 2.1.180 observed on account B.
	if got := ResolveClaudeDeviceProfile(authA, "", nil, cfg).VersionString(); got != "2.1.100" {
		t.Fatalf("acct-a fallback = %q, want own high-water 2.1.100 (not global 2.1.180)", got)
	}

	// A brand-new account with no observation of its own DOES use the global
	// observed high-water (2.1.180) as a safe fallback ceiling.
	authNew := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-new", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(authNew, "", nil, cfg).VersionString(); got != "2.1.180" {
		t.Fatalf("acct-new fallback = %q, want global observed high-water 2.1.180", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingRejectsFabricatedHighUA covers
// sanity-ceiling case (a): a holder of a valid downstream key sends a fabricated
// high version (claude-cli/999.0.0). It must be rejected at the source: it must
// NOT be emitted as the outbound version, must NOT enter this account's observed
// high-water, and must NOT enter the global observed high-water (so it can never
// pollute other zero-observation accounts).
func TestResolveClaudeDeviceProfile_SanityCeilingRejectsFabricatedHighUA(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	attacker := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-forged-ua-auth", Provider: "claude"}

	// Forged high UA: must not be adopted as the outbound version.
	forged := ResolveClaudeDeviceProfile(attacker, "", map[string][]string{
		"User-Agent": {"claude-cli/999.0.0 (external, cli)"},
	}, cfg)
	if got := forged.VersionString(); got != "2.1.63" {
		t.Fatalf("forged 999.0.0 outbound version = %q, want static floor 2.1.63 (must be rejected)", got)
	}

	// Forged version must not be recorded into this account's observations.
	if obs := ClaudeDeviceProfileObservations(attacker, ""); len(obs) != 0 {
		t.Fatalf("forged 999.0.0 recorded as observation %#v, want none (per-account high-water must stay empty)", obs)
	}

	// Forged version must not pollute the global observed high-water: a fresh
	// account with no observation of its own must NOT inherit 999.x.
	fresh := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-fresh-after-forgery-auth", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(fresh, "", nil, cfg).VersionString(); got != "2.1.63" {
		t.Fatalf("fresh account fallback = %q, want static floor 2.1.63 (forged global high-water must not leak)", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingAcceptsRealRecentVersion covers
// sanity-ceiling case (b): a genuine recent version (2.1.180), comfortably below
// the static sanity ceiling, is accepted normally — the ceiling must not
// false-reject real clients.
func TestResolveClaudeDeviceProfile_SanityCeilingAcceptsRealRecentVersion(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-real-recent-auth", Provider: "claude"}

	real := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.180 (external, cli)"},
	}, cfg)
	if got := real.VersionString(); got != "2.1.180" {
		t.Fatalf("real recent 2.1.180 version = %q, want accepted 2.1.180 (no false reject)", got)
	}

	// It must be recorded as the account's observed high-water.
	obs := ClaudeDeviceProfileObservations(auth, "")
	if len(obs) != 1 || obs[0].Version != "2.1.180" {
		t.Fatalf("observations = %#v, want exactly [2.1.180] recorded", obs)
	}

	// And it must survive as the cached high-water on a no-client request.
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.180" {
		t.Fatalf("cached fallback = %q, want retained high-water 2.1.180", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingLiftedByNpmStillNotPushed covers
// sanity-ceiling case (c): when npm latest is available it raises the validation
// ceiling (here npm latest 4.1.0, above the hardcoded static 4.0.0), but npm is
// used ONLY as an upper bound — it never pushes the outbound version up. With a
// real observation of 2.1.173 the outbound version stays 2.1.173, not npm latest.
// A client at the npm-raised ceiling (4.1.0, above the static 4.0.0 bound) is then
// accepted, proving npm widened acceptance without inflating the floor.
func TestResolveClaudeDeviceProfile_SanityCeilingLiftedByNpmStillNotPushed(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "4.1.0",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:    managedHeaderProfileSourceNPM,
				SourceURL: claudeCodeNPMURL,
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &online},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-npm-ceiling-auth", Provider: "claude"}

	// Real observation 2.1.173 with npm latest 4.1.0 available: npm is a ceiling
	// reference only, so the outbound version stays at the real observed 2.1.173
	// (npm must not push the floor up to 4.1.0).
	first := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.173 (external, cli)"},
	}, cfg)
	if got := first.VersionString(); got != "2.1.173" {
		t.Fatalf("first version = %q, want observed 2.1.173 (npm must not push up)", got)
	}
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.173" {
		t.Fatalf("cached version = %q, want 2.1.173 (npm 4.1.0 must not inflate outbound)", got)
	}

	// The npm-raised sanity ceiling the helper computes must be npm latest 4.1.0,
	// strictly above the hardcoded static ceiling 4.0.0.
	ceiling := claudeObservationSanityCeiling(cfg)
	if want := (claudeCLIVersion{major: 4, minor: 1, patch: 0}); ceiling.Compare(want) != 0 {
		t.Fatalf("npm-raised sanity ceiling = %+v, want 4.1.0", ceiling)
	}
	if ceiling.Compare(claudeStaticSanityCeiling()) <= 0 {
		t.Fatalf("npm-raised ceiling %+v must exceed static ceiling %+v when npm latest is higher", ceiling, claudeStaticSanityCeiling())
	}

	// A genuine client exactly at the npm-raised ceiling (4.1.0), which would be
	// rejected by the static 4.0.0 bound alone, is now accepted because npm widened
	// the validation ceiling. (This account becomes its own high-water at 4.1.0;
	// the earlier account stays pinned at its own observed 2.1.173.)
	edge := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-npm-edge-auth", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(edge, "", map[string][]string{
		"User-Agent": {"claude-cli/4.1.0 (external, cli)"},
	}, cfg).VersionString(); got != "4.1.0" {
		t.Fatalf("edge client 4.1.0 = %q, want accepted at npm-raised ceiling", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingOfflineConstantApplies covers
// sanity-ceiling case (d): with no npm available (online-update off, the
// default), the hardcoded static ceiling still applies — a forged version above
// it (999.x) is rejected while a version at/below it is accepted. This pins the
// deterministic offline behavior.
func TestResolveClaudeDeviceProfile_SanityCeilingOfflineConstantApplies(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	oldOverride := ManagedHeaderOnlineFetchOverride
	consulted := false
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		consulted = true
		return managedHeaderOnlineVersion{Version: "9.9.9"}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		ResetClaudeDeviceProfileCache()
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	// online-update left unset (nil) => disabled, matching the loader default, so
	// the npm override must never be consulted and the offline static ceiling
	// governs.
	cfg := &config.Config{}

	if ceiling := claudeObservationSanityCeiling(cfg); ceiling.Compare(claudeStaticSanityCeiling()) != 0 {
		t.Fatalf("offline sanity ceiling = %+v, want hardcoded static ceiling %+v", ceiling, claudeStaticSanityCeiling())
	}
	if consulted {
		t.Fatalf("npm must not be consulted for the sanity ceiling when online-update is disabled")
	}

	// Forged 999.0.0 is rejected offline.
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-offline-ceiling-auth", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/999.0.0 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.63" {
		t.Fatalf("offline forged 999.0.0 = %q, want static floor 2.1.63 (rejected by offline ceiling)", got)
	}
	if obs := ClaudeDeviceProfileObservations(auth, ""); len(obs) != 0 {
		t.Fatalf("offline forged 999.0.0 recorded %#v, want none", obs)
	}

	// A version exactly at the static ceiling boundary (4.0.0) is accepted offline.
	boundaryAuth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-offline-boundary-auth", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(boundaryAuth, "", map[string][]string{
		"User-Agent": {"claude-cli/4.0.0 (external, cli)"},
	}, cfg).VersionString(); got != "4.0.0" {
		t.Fatalf("boundary 4.0.0 = %q, want accepted at static ceiling", got)
	}
}
