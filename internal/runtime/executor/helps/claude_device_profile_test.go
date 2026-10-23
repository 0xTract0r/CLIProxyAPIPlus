package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"strings"
	"testing"
	"time"
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
	if got := zeroObs.VersionString(); got != "2.1.211" {
		t.Fatalf("zero-observation version = %q, want static floor 2.1.211 (npm must not be a ceiling)", got)
	}
	if got := zeroObs.UserAgent; !strings.Contains(got, "claude-cli/2.1.211") {
		t.Fatalf("zero-observation UserAgent = %q, want static floor, not npm latest", got)
	}

	// Now observe a real first-party client above the floor but below npm latest on
	// a different account; npm must be capped to that real observed high-water
	// (2.1.220), never lifted to npm latest 2.9.9.
	_ = ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-real-client-auth",
		Provider: "claude",
	}, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"},
	}, cfg)

	cappedFallback := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-zero-observation-auth",
		Provider: "claude",
	}, "", nil, cfg)
	if got := cappedFallback.VersionString(); got != "2.1.220" {
		t.Fatalf("fallback version = %q, want global observed high-water 2.1.220 (npm capped to real)", got)
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
			Version: "2.1.264",
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

	// Observed real client (2.1.262) is above the floor but below npm latest
	// (2.1.264); the observed version must be preferred over the newer online value.
	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-observed-auth",
		Provider: "claude",
	}, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.262 (external, cli)"},
		"X-Stainless-Package-Version": {"0.80.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
	}, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if got := profile.UserAgent; got != "claude-cli/2.1.262 (external, cli)" {
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

func TestResolveClaudeDeviceProfile_FloorsUpObservedCLIBelowConfiguredFallback(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		FileName: "claude-configured-fallback.json",
		Provider: "claude",
	}
	// The configured fallback baseline (2.1.264) is NEWER than the inbound real
	// client (2.1.260). Unify-to-high-water requires the request to floor UP to the
	// baseline triple and egress at the baseline version — never at the client's own
	// lower version, which would expose one shared account as "one person, many
	// machine versions".
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.264 (external, cli)",
			PackageVersion: "0.96.0",
			RuntimeVersion: "v24.5.0",
		},
	}

	profile := ResolveClaudeDeviceProfile(auth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.260 (external, cli)"},
		"X-Stainless-Package-Version": {"0.92.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	if got := profile.UserAgent; got != "claude-cli/2.1.264 (external, cli)" {
		t.Fatalf("UserAgent = %q, want floor-up to configured baseline claude-cli/2.1.264 (external, cli)", got)
	}
	if got := profile.VersionString(); got != "2.1.264" {
		t.Fatalf("version = %q, want floor-up baseline 2.1.264 (not lower observed 2.1.260)", got)
	}
	// The whole software triple floors up atomically to the configured baseline.
	if got := profile.PackageVersion; got != "0.96.0" {
		t.Fatalf("package version = %q, want configured baseline 0.96.0", got)
	}
	if got := profile.RuntimeVersion; got != "v24.5.0" {
		t.Fatalf("runtime version = %q, want configured baseline v24.5.0", got)
	}
	// Emission floors UP to the configured baseline, but recording is decoupled from
	// the floor-up gate: the real below-baseline client (2.1.260) is still recorded
	// into the per-account diagnostic history — it just never becomes the emitted or
	// cached profile. The prior coupled gate dropped this observation; decoupling
	// restores it.
	observations := ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1 (below-baseline client still recorded for diagnostics): %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.260" {
		t.Fatalf("observation version = %q, want 2.1.260 (real inbound client recorded)", got)
	}
}

// TestClaudeDeviceProfileObservations_TracksRecentClientVersionsPerAuth verifies the
// per-account observation set records each distinct real client version with
// first/last-seen timestamps and per-version request counts. Recording is decoupled
// from the floor-up emission gate: every sanity-passing real client is recorded, so a
// repeat of a below-high-water version (the third request below) IS re-counted — its
// request_count reaches 2 even though that version is never emitted (emission stays
// floored up to the high-water).
func TestClaudeDeviceProfileObservations_TracksRecentClientVersionsPerAuth(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-observation-auth", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.260 (external, cli)"},
		"X-Stainless-Package-Version": {"0.80.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.262 (external, cli)"},
		"X-Stainless-Package-Version": {"0.81.0"},
		"X-Stainless-Runtime-Version": {"v24.6.0"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.260 (external, cli)"},
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
	// The third request repeats 2.1.260, below the 2.1.262 high-water. It is never
	// emitted (emission floors up), but recording is decoupled from the floor-up gate,
	// so the repeat is still counted: request_count reaches 2.
	if got := byVersion["2.1.260"].RequestCount; got != 2 {
		t.Fatalf("2.1.260 request_count = %d, want 2 (below-high-water repeat still recorded under decoupled diagnostics): %#v", got, byVersion["2.1.260"])
	}
	if got := byVersion["2.1.262"].RequestCount; got != 1 {
		t.Fatalf("2.1.262 request_count = %d, want 1: %#v", got, byVersion["2.1.262"])
	}
	if byVersion["2.1.260"].LastSeenAt == "" || byVersion["2.1.260"].FirstSeenAt == "" {
		t.Fatalf("expected first/last seen timestamps: %#v", byVersion["2.1.260"])
	}
}

func TestClaudeDeviceProfileObservations_FileBackedAuthVisibleWithoutAPIKey(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "runtime-id-from-loader", FileName: "claude-file-auth.json", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(auth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.262 (external, cli)"},
		"X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	observations := ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.262" {
		t.Fatalf("version = %q, want 2.1.262", got)
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
		"User-Agent":                  {"claude-cli/2.1.264 (external, cli)"},
		"X-Stainless-Package-Version": {"0.96.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "claude-file-auth.json", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.264" {
		t.Fatalf("version = %q, want 2.1.264", got)
	}
}

func TestClaudeDeviceProfileObservations_LabelAlias(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ProxyURL: "direct", Label: "bcd898@example.com", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.262 (external, cli)"},
		"X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", Label: "bcd898@example.com", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.262" {
		t.Fatalf("version = %q, want 2.1.262", got)
	}
}

func TestClaudeDeviceProfileObservations_GlobalFallbackForUnidentifiedAuth(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ProxyURL: "direct", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "provider-token", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.261 (external, cli)"},
		"X-Stainless-Package-Version": {"0.93.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "claude-file-auth.json", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.261" {
		t.Fatalf("version = %q, want 2.1.261", got)
	}
}

// TestResolveClaudeDeviceProfile_HighWaterCapsToObservedNotNpm covers requirement
// ⑥ plan A case (a): an account whose only real observation is claude-cli/2.1.293
// has a high-water of exactly 2.1.293. Even with online-update enabled and npm
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
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-293-auth", Provider: "claude"}

	// First request: real client 2.1.293 is observed and becomes the high-water.
	first := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.293 (external, cli)"},
	}, cfg)
	if got := first.VersionString(); got != "2.1.293" {
		t.Fatalf("first request version = %q, want observed 2.1.293", got)
	}

	// Subsequent request without a client UA: must reuse the cached 2.1.293
	// high-water, never the newer npm latest 2.5.0.
	cached := ResolveClaudeDeviceProfile(auth, "", nil, cfg)
	if got := cached.VersionString(); got != "2.1.293" {
		t.Fatalf("cached version = %q, want observed high-water 2.1.293 (npm must not inflate)", got)
	}

	// A second real client at 2.1.290 (older, still above floor) must not lower the
	// high-water.
	lower := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.290 (external, cli)"},
	}, cfg)
	if got := lower.VersionString(); got != "2.1.293" {
		t.Fatalf("after older client, version = %q, want high-water 2.1.293 (only-up)", got)
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
	if got := profile.VersionString(); got != "2.1.211" {
		t.Fatalf("zero-observation version = %q, want static floor 2.1.211, never npm latest", got)
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
	if got := profile.VersionString(); got != "2.1.211" {
		t.Fatalf("version = %q, want static floor 2.1.211 with online-update off", got)
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
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.220" {
		t.Fatalf("version = %q, want 2.1.220", got)
	}

	// Newer real client raises the high-water.
	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.300 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.300" {
		t.Fatalf("version = %q, want raised high-water 2.1.300", got)
	}

	// Older real client must not lower it.
	if got := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.270 (external, cli)"},
	}, cfg).VersionString(); got != "2.1.300" {
		t.Fatalf("version = %q, want retained high-water 2.1.300", got)
	}

	// No-client fallback keeps the high-water.
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.300" {
		t.Fatalf("fallback version = %q, want retained high-water 2.1.300", got)
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
		"User-Agent": {"claude-cli/2.1.220 (external, cli)"},
	}, cfg)
	_ = ResolveClaudeDeviceProfile(authB, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.300 (external, cli)"},
	}, cfg)

	// Account A's fallback must stay at its own high-water 2.1.220, never the
	// higher 2.1.300 observed on account B.
	if got := ResolveClaudeDeviceProfile(authA, "", nil, cfg).VersionString(); got != "2.1.220" {
		t.Fatalf("acct-a fallback = %q, want own high-water 2.1.220 (not global 2.1.300)", got)
	}

	// A brand-new account with no observation of its own DOES use the global
	// observed high-water (2.1.300) as a safe fallback ceiling.
	authNew := &cliproxyauth.Auth{ProxyURL: "direct", ID: "acct-new", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(authNew, "", nil, cfg).VersionString(); got != "2.1.300" {
		t.Fatalf("acct-new fallback = %q, want global observed high-water 2.1.300", got)
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
	if got := forged.VersionString(); got != "2.1.211" {
		t.Fatalf("forged 999.0.0 outbound version = %q, want static floor 2.1.211 (must be rejected)", got)
	}

	// Forged version must not be recorded into this account's observations.
	if obs := ClaudeDeviceProfileObservations(attacker, ""); len(obs) != 0 {
		t.Fatalf("forged 999.0.0 recorded as observation %#v, want none (per-account high-water must stay empty)", obs)
	}

	// Forged version must not pollute the global observed high-water: a fresh
	// account with no observation of its own must NOT inherit 999.x.
	fresh := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-fresh-after-forgery-auth", Provider: "claude"}
	if got := ResolveClaudeDeviceProfile(fresh, "", nil, cfg).VersionString(); got != "2.1.211" {
		t.Fatalf("fresh account fallback = %q, want static floor 2.1.211 (forged global high-water must not leak)", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingAcceptsRealRecentVersion covers
// sanity-ceiling case (b): a genuine recent version (2.1.300), comfortably below
// the static sanity ceiling, is accepted normally — the ceiling must not
// false-reject real clients.
func TestResolveClaudeDeviceProfile_SanityCeilingAcceptsRealRecentVersion(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-real-recent-auth", Provider: "claude"}

	real := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.300 (external, cli)"},
	}, cfg)
	if got := real.VersionString(); got != "2.1.300" {
		t.Fatalf("real recent 2.1.300 version = %q, want accepted 2.1.300 (no false reject)", got)
	}

	// It must be recorded as the account's observed high-water.
	obs := ClaudeDeviceProfileObservations(auth, "")
	if len(obs) != 1 || obs[0].Version != "2.1.300" {
		t.Fatalf("observations = %#v, want exactly [2.1.300] recorded", obs)
	}

	// And it must survive as the cached high-water on a no-client request.
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.300" {
		t.Fatalf("cached fallback = %q, want retained high-water 2.1.300", got)
	}
}

// TestResolveClaudeDeviceProfile_SanityCeilingLiftedByNpmStillNotPushed covers
// sanity-ceiling case (c): when npm latest is available it raises the validation
// ceiling (here npm latest 4.1.0, above the hardcoded static 4.0.0), but npm is
// used ONLY as an upper bound — it never pushes the outbound version up. With a
// real observation of 2.1.293 the outbound version stays 2.1.293, not npm latest.
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

	// Real observation 2.1.293 with npm latest 4.1.0 available: npm is a ceiling
	// reference only, so the outbound version stays at the real observed 2.1.293
	// (npm must not push the floor up to 4.1.0).
	first := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.293 (external, cli)"},
	}, cfg)
	if got := first.VersionString(); got != "2.1.293" {
		t.Fatalf("first version = %q, want observed 2.1.293 (npm must not push up)", got)
	}
	if got := ResolveClaudeDeviceProfile(auth, "", nil, cfg).VersionString(); got != "2.1.293" {
		t.Fatalf("cached version = %q, want 2.1.293 (npm 4.1.0 must not inflate outbound)", got)
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
	}, cfg).VersionString(); got != "2.1.211" {
		t.Fatalf("offline forged 999.0.0 = %q, want static floor 2.1.211 (rejected by offline ceiling)", got)
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

// TestResolveClaudeDeviceProfile_SubTwoZeroFabricatedVersionFlooredToBaseline
// confirms the floor-up EMIT gate covers nonsensical sub-2.0 versions (0.x/1.x),
// which no genuine native-generation claude-cli emits: 1.5.0 is far below the
// baseline high-water, so it is a non-upgrade — floored to the static baseline
// (2.1.211) on egress rather than emitted verbatim. Recording is decoupled from
// emission and carries no lower bound (only the sanity ceiling), so the raw 1.5.0
// observation is still logged into the diagnostic history even though it is never
// emitted.
func TestResolveClaudeDeviceProfile_SubTwoZeroFabricatedVersionFlooredToBaseline(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-sub2-auth", Provider: "claude"}

	profile := ResolveClaudeDeviceProfile(auth, "", map[string][]string{
		"User-Agent": {"claude-cli/1.5.0 (external, cli)"},
	}, cfg)
	if got := profile.VersionString(); got != "2.1.211" {
		t.Fatalf("sub-2.0 forged 1.5.0 version = %q, want floored to baseline 2.1.211 (garbage low version floored up, never emitted)", got)
	}
	// Emission floors up, but the raw observation is still recorded for diagnostics
	// (recording is gated only by the sanity ceiling, with no lower bound).
	obs := ClaudeDeviceProfileObservations(auth, "")
	if len(obs) != 1 {
		t.Fatalf("sub-2.0 forged 1.5.0 observations = %#v, want 1 (recorded for diagnostics, decoupled from emission)", obs)
	}
	if got := obs[0].Version; got != "1.5.0" {
		t.Fatalf("observation version = %q, want 1.5.0 (raw inbound recorded even though floored on egress)", got)
	}
}

type fakeClaudeDeviceProfileKVClient struct {
	values        map[string][]byte
	getErr        error
	setErr        error
	setNXErr      error
	expireErr     error
	setNXResult   bool
	getCount      int
	setCount      int
	setNXCount    int
	expireCount   int
	lastSetTTL    time.Duration
	lastSetNXTTL  time.Duration
	lastExpireTTL time.Duration
}

func newFakeClaudeDeviceProfileKVClient() *fakeClaudeDeviceProfileKVClient {
	return &fakeClaudeDeviceProfileKVClient{
		values:      make(map[string][]byte),
		setNXResult: true,
	}
}

func (c *fakeClaudeDeviceProfileKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.getCount++
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), value...), true, nil
}

func (c *fakeClaudeDeviceProfileKVClient) KVSet(_ context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error) {
	c.setCount++
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeClaudeDeviceProfileKVClient) KVSetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	c.setNXCount++
	c.lastSetNXTTL = ttl
	if c.setNXErr != nil {
		return false, c.setNXErr
	}
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	if c.setNXResult {
		c.values[key] = append([]byte(nil), value...)
		return true, nil
	}
	return false, nil
}

func (c *fakeClaudeDeviceProfileKVClient) KVExpire(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.expireCount++
	c.lastExpireTTL = ttl
	if c.expireErr != nil {
		return false, c.expireErr
	}
	return true, nil
}

func useFakeClaudeDeviceProfileKVClient(t *testing.T, client *fakeClaudeDeviceProfileKVClient, homeMode bool, errClient error) {
	t.Helper()
	previous := currentClaudeDeviceProfileKVClient
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(func() {
		currentClaudeDeviceProfileKVClient = previous
	})
}

func mustClaudeDeviceProfileJSON(t *testing.T, value claudeDeviceProfileKVValue) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal device profile: %v", errMarshal)
	}
	return raw
}

func claudeDeviceHeaders(userAgent string) http.Header {
	return http.Header{
		"User-Agent":                  {userAgent},
		"X-Stainless-Package-Version": {defaultClaudeFingerprintPackageVersion},
		"X-Stainless-Runtime-Version": {defaultClaudeFingerprintRuntimeVersion},
		"X-Stainless-Os":              {"Windows"},
		"X-Stainless-Arch":            {"x64"},
	}
}

func TestResolveClaudeDeviceProfileLocalUsesBaselineForInvalidSignals(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	auth := &cliproxyauth.Auth{ID: "auth-invalid-signals"}
	headers := claudeDeviceHeaders("claude-cli/999.0.0 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "999.0.0")
	headers.Set("X-Stainless-Runtime-Version", "v999.0.0")

	profile := resolveClaudeDeviceProfileLocal(auth, "api-key", headers, exactClaudeDeviceProfileTestConfig(nil))
	baseline := defaultClaudeDeviceProfile(nil)
	if profile.UserAgent != baseline.UserAgent || profile.PackageVersion != baseline.PackageVersion || profile.RuntimeVersion != baseline.RuntimeVersion {
		t.Fatalf("invalid profile = %#v, want local baseline %#v", profile, baseline)
	}
}

func TestResolveClaudeDeviceProfileLocalKeepsExactMeasuredSoftwareBaseline(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	auth := &cliproxyauth.Auth{ID: "auth-newer-patch-signals"}
	headers := claudeDeviceHeaders("claude-cli/2.1.281 (external, cli)")

	profile := resolveClaudeDeviceProfileLocal(auth, "api-key", headers, exactClaudeDeviceProfileTestConfig(nil))
	baseline := defaultClaudeDeviceProfile(nil)
	if profile.UserAgent != baseline.UserAgent || profile.PackageVersion != baseline.PackageVersion || profile.RuntimeVersion != baseline.RuntimeVersion {
		t.Fatalf("unmeasured profile = %#v, want exact local baseline %#v", profile, baseline)
	}
}

func TestApplyClaudeLegacyDeviceHeadersReplacesInvalidNativeSoftwareSignals(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	incoming := claudeDeviceHeaders("claude-cli/999.0.0 (external, cli)")
	incoming.Set("X-Stainless-Package-Version", "999.0.0")
	incoming.Set("X-Stainless-Runtime-Version", "v999.0.0")

	ApplyClaudeLegacyDeviceHeaders(request, incoming, nil, true)

	baseline := defaultClaudeDeviceProfile(nil)
	if got := request.Header.Get("User-Agent"); got != baseline.UserAgent {
		t.Fatalf("User-Agent = %q, want local baseline %q", got, baseline.UserAgent)
	}
	if got := request.Header.Get("X-Stainless-Package-Version"); got != baseline.PackageVersion {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, baseline.PackageVersion)
	}
	if got := request.Header.Get("X-Stainless-Runtime-Version"); got != baseline.RuntimeVersion {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, baseline.RuntimeVersion)
	}
}

func TestApplyClaudeLegacyDeviceHeadersAcceptsConfiguredMeasuredBaseline(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		UserAgent:      "claude-cli/2.2.0 (external, cli)",
		PackageVersion: "0.95.0",
		RuntimeVersion: "v26.4.0",
		OS:             "MacOS",
		Arch:           "arm64",
	}}
	incoming := claudeDeviceHeaders("claude-cli/2.2.0 (external, cli)")
	incoming.Set("X-Stainless-Package-Version", "0.95.0")
	incoming.Set("X-Stainless-Runtime-Version", "v26.4.0")

	ApplyClaudeLegacyDeviceHeaders(request, incoming, cfg, true)

	if got := request.Header.Get("User-Agent"); got != "claude-cli/2.2.0 (external, cli)" {
		t.Fatalf("User-Agent = %q, want configured measured baseline", got)
	}
	if got := request.Header.Get("X-Stainless-Package-Version"); got != "0.95.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want 0.95.0", got)
	}
	if got := request.Header.Get("X-Stainless-Runtime-Version"); got != "v26.4.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want v26.4.0", got)
	}
}

func TestApplyClaudeLegacyDeviceHeadersKeepNewerVersionsUsesConfiguredBaseline(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		UserAgent:      "claude-cli/2.2.0 (external, cli)",
		PackageVersion: "0.95.0",
		RuntimeVersion: "v26.4.0",
		OS:             "MacOS",
		Arch:           "arm64",
	}}
	incoming := claudeDeviceHeaders("claude-cli/2.2.5 (external, cli)")
	incoming.Set("X-Stainless-Package-Version", "0.128.0")
	incoming.Set("X-Stainless-Runtime-Version", "v26.5.0")

	ApplyClaudeLegacyDeviceHeaders(request, incoming, cfg, true, true)

	if got := request.Header.Get("User-Agent"); got != "claude-cli/2.2.5 (external, cli)" {
		t.Fatalf("User-Agent = %q, want claude-cli/2.2.5 (external, cli)", got)
	}
	if got := request.Header.Get("X-Stainless-Package-Version"); got != "0.128.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want 0.128.0", got)
	}
	if got := request.Header.Get("X-Stainless-Runtime-Version"); got != "v26.5.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want v26.5.0", got)
	}
}

func TestResolveClaudeDeviceProfileRequiredHomeReadWithoutCandidate(t *testing.T) {
	client := newFakeClaudeDeviceProfileKVClient()
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	key := claudeDeviceProfileKVKey(auth, "api-key", ClaudeDeviceProfile{})
	client.values[key] = mustClaudeDeviceProfileJSON(t, claudeDeviceProfileKVValue{
		UserAgent:      "claude-cli/2.2.0 (external, cli)",
		PackageVersion: "0.80.0",
		RuntimeVersion: "v24.4.0",
		OS:             "Windows",
		Arch:           "x64",
	})
	useFakeClaudeDeviceProfileKVClient(t, client, true, nil)

	profile, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", nil, exactClaudeDeviceProfileTestConfig(nil))
	if errProfile != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() error = %v", errProfile)
	}
	if profile.UserAgent != defaultClaudeFingerprintUserAgent {
		t.Fatalf("UserAgent = %q, want local baseline %q for unmeasured cached profile", profile.UserAgent, defaultClaudeFingerprintUserAgent)
	}
	if profile.OS != defaultClaudeFingerprintOS || profile.Arch != defaultClaudeFingerprintArch {
		t.Fatalf("platform = %s/%s, want baseline pinned %s/%s", profile.OS, profile.Arch, defaultClaudeFingerprintOS, defaultClaudeFingerprintArch)
	}
	if client.expireCount != 1 || client.lastExpireTTL != claudeDeviceProfileTTL {
		t.Fatalf("KVExpire count/ttl = %d/%v, want 1/%v", client.expireCount, client.lastExpireTTL, claudeDeviceProfileTTL)
	}
}

func TestResolveClaudeDeviceProfileRequiredHomeCandidateLocksRereadsAndWrites(t *testing.T) {
	client := newFakeClaudeDeviceProfileKVClient()
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	useFakeClaudeDeviceProfileKVClient(t, client, true, nil)

	profile, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), exactClaudeDeviceProfileTestConfig(nil))
	if errProfile != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() error = %v", errProfile)
	}
	if profile.UserAgent != defaultClaudeFingerprintUserAgent {
		t.Fatalf("UserAgent = %q, want candidate %q", profile.UserAgent, defaultClaudeFingerprintUserAgent)
	}
	if client.setNXCount != 1 || client.lastSetNXTTL != claudeDeviceProfileLockTTL {
		t.Fatalf("KVSetNX count/ttl = %d/%v, want 1/%v", client.setNXCount, client.lastSetNXTTL, claudeDeviceProfileLockTTL)
	}
	if client.getCount != 1 {
		t.Fatalf("KVGet count = %d, want re-read after lock", client.getCount)
	}
	if client.setCount != 1 || client.lastSetTTL != claudeDeviceProfileTTL {
		t.Fatalf("KVSet count/ttl = %d/%v, want 1/%v", client.setCount, client.lastSetTTL, claudeDeviceProfileTTL)
	}
}

func TestResolveClaudeDeviceProfileRequiredHomeSeparatesVSCodeAgentSDKFromCLI(t *testing.T) {
	client := newFakeClaudeDeviceProfileKVClient()
	auth := &cliproxyauth.Auth{ID: "auth-home-subclient-isolation"}
	useFakeClaudeDeviceProfileKVClient(t, client, true, nil)

	cliProfile, errCLI := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", captured280ClaudeDeviceHeaders("claude-cli/2.1.280 (external, cli)"), captured280ClaudeDeviceConfig())
	if errCLI != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() CLI error = %v", errCLI)
	}
	vscodeUA := "claude-cli/2.1.280 (external, claude-vscode, agent-sdk/0.3.220)"
	vscodeProfile, errVSCode := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", captured280ClaudeDeviceHeaders(vscodeUA), captured280ClaudeDeviceConfig())
	if errVSCode != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() VSCode error = %v", errVSCode)
	}

	if cliProfile.UserAgent != "claude-cli/2.1.280 (external, cli)" {
		t.Fatalf("CLI UserAgent = %q, want CLI profile", cliProfile.UserAgent)
	}
	if vscodeProfile.UserAgent != vscodeUA {
		t.Fatalf("VSCode UserAgent = %q, want %q", vscodeProfile.UserAgent, vscodeUA)
	}
	if client.setCount != 2 {
		t.Fatalf("KVSet count = %d, want separate CLI and VSCode profiles", client.setCount)
	}
	cliKey := claudeDeviceProfileKVKey(auth, "api-key", cliProfile)
	vscodeKey := claudeDeviceProfileKVKey(auth, "api-key", vscodeProfile)
	if cliKey == vscodeKey {
		t.Fatalf("CLI and VSCode KV keys are equal: %q", cliKey)
	}
	if _, ok := client.values[cliKey]; !ok {
		t.Fatalf("CLI profile missing from KV key %q", cliKey)
	}
	if _, ok := client.values[vscodeKey]; !ok {
		t.Fatalf("VSCode profile missing from KV key %q", vscodeKey)
	}
}

func TestResolveClaudeDeviceProfileRequiredHomeNormalizesUnmeasuredCachedProfile(t *testing.T) {
	client := newFakeClaudeDeviceProfileKVClient()
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	key := claudeDeviceProfileKVKey(auth, "api-key", ClaudeDeviceProfile{})
	client.values[key] = mustClaudeDeviceProfileJSON(t, claudeDeviceProfileKVValue{
		UserAgent:      "claude-cli/2.4.0 (external, cli)",
		PackageVersion: "0.90.0",
		RuntimeVersion: "v24.5.0",
		OS:             "Windows",
		Arch:           "x64",
	})
	useFakeClaudeDeviceProfileKVClient(t, client, true, nil)

	profile, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", claudeDeviceHeaders("claude-cli/2.3.0 (external, cli)"), exactClaudeDeviceProfileTestConfig(nil))
	if errProfile != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() error = %v", errProfile)
	}
	if profile.UserAgent != defaultClaudeFingerprintUserAgent {
		t.Fatalf("UserAgent = %q, want local baseline %q", profile.UserAgent, defaultClaudeFingerprintUserAgent)
	}
	if client.setCount != 0 {
		t.Fatalf("KVSet count = %d, want no downgrade write", client.setCount)
	}
	if client.expireCount != 1 {
		t.Fatalf("KVExpire count = %d, want cached refresh", client.expireCount)
	}
}

func TestResolveClaudeDeviceProfileRequiredHomeFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		client  *fakeClaudeDeviceProfileKVClient
	}{
		{name: "read", client: &fakeClaudeDeviceProfileKVClient{values: make(map[string][]byte), getErr: errors.New("get failed")}},
		{name: "lock", headers: claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), client: &fakeClaudeDeviceProfileKVClient{values: make(map[string][]byte), setNXResult: true, setNXErr: errors.New("lock failed")}},
		{name: "lock-miss", headers: claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), client: &fakeClaudeDeviceProfileKVClient{values: make(map[string][]byte), setNXResult: false}},
		{name: "reread", headers: claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), client: &fakeClaudeDeviceProfileKVClient{values: make(map[string][]byte), setNXResult: true, getErr: errors.New("re-read failed")}},
		{name: "write", headers: claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), client: &fakeClaudeDeviceProfileKVClient{values: make(map[string][]byte), setNXResult: true, setErr: errors.New("write failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeClaudeDeviceProfileKVClient(t, tc.client, true, nil)
			if _, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "auth-1"}, "api-key", tc.headers, exactClaudeDeviceProfileTestConfig(nil)); errProfile == nil {
				t.Fatalf("ResolveClaudeDeviceProfileRequired() error = nil, want error")
			}
		})
	}
}

func TestResolveClaudeDeviceProfilePreservesConfirmedClientAtBaselineVersion(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	client := newFakeClaudeDeviceProfileKVClient()
	useFakeClaudeDeviceProfileKVClient(t, client, false, nil)
	auth := &cliproxyauth.Auth{ID: "auth-baseline-entrypoint"}
	headers := claudeDeviceHeaders("claude-cli/2.1.280 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "0.112.1")
	headers.Set("X-Stainless-Runtime-Version", "v26.3.0")

	profile, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", headers, captured280ClaudeDeviceConfig())
	if errProfile != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() error = %v", errProfile)
	}
	if profile.UserAgent != "claude-cli/2.1.280 (external, cli)" {
		t.Fatalf("UserAgent = %q, want confirmed cli entrypoint preserved", profile.UserAgent)
	}
	if profile.PackageVersion != "0.112.1" || profile.RuntimeVersion != "v26.3.0" {
		t.Fatalf("software profile = %s/%s, want 0.112.1/v26.3.0", profile.PackageVersion, profile.RuntimeVersion)
	}
}

func TestResolveClaudeDeviceProfileSeparatesVSCodeAgentSDKFromCLI(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	client := newFakeClaudeDeviceProfileKVClient()
	useFakeClaudeDeviceProfileKVClient(t, client, false, nil)
	auth := &cliproxyauth.Auth{ID: "auth-subclient-isolation"}

	cliHeaders := claudeDeviceHeaders("claude-cli/2.1.280 (external, cli)")
	cliHeaders.Set("X-Stainless-Package-Version", "0.112.1")
	cliHeaders.Set("X-Stainless-Runtime-Version", "v26.3.0")
	cliProfile, errCLI := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", cliHeaders, captured280ClaudeDeviceConfig())
	if errCLI != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() CLI error = %v", errCLI)
	}

	vscodeUA := "claude-cli/2.1.280 (external, claude-vscode, agent-sdk/0.3.220)"
	vscodeHeaders := claudeDeviceHeaders(vscodeUA)
	vscodeHeaders.Set("X-Stainless-Package-Version", "0.112.1")
	vscodeHeaders.Set("X-Stainless-Runtime-Version", "v26.3.0")
	vscodeProfile, errVSCode := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", vscodeHeaders, captured280ClaudeDeviceConfig())
	if errVSCode != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() VSCode error = %v", errVSCode)
	}

	sdkTSUA := "claude-cli/2.1.280 (external, sdk-ts, agent-sdk/0.3.220)"
	sdkTSHeaders := claudeDeviceHeaders(sdkTSUA)
	sdkTSHeaders.Set("X-Stainless-Package-Version", "0.112.1")
	sdkTSHeaders.Set("X-Stainless-Runtime-Version", "v26.3.0")
	sdkTSProfile, errSDKTS := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", sdkTSHeaders, captured280ClaudeDeviceConfig())
	if errSDKTS != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() sdk-ts error = %v", errSDKTS)
	}

	if cliProfile.UserAgent != "claude-cli/2.1.280 (external, cli)" {
		t.Fatalf("CLI UserAgent = %q, want CLI profile", cliProfile.UserAgent)
	}
	if vscodeProfile.UserAgent != vscodeUA {
		t.Fatalf("VSCode UserAgent = %q, want %q", vscodeProfile.UserAgent, vscodeUA)
	}
	if sdkTSProfile.UserAgent != sdkTSUA {
		t.Fatalf("sdk-ts UserAgent = %q, want %q", sdkTSProfile.UserAgent, sdkTSUA)
	}

	cliProfileAgain, errCLIAgain := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", cliHeaders, captured280ClaudeDeviceConfig())
	if errCLIAgain != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() second CLI error = %v", errCLIAgain)
	}
	if cliProfileAgain.UserAgent != cliProfile.UserAgent {
		t.Fatalf("second CLI UserAgent = %q, want isolated cached %q", cliProfileAgain.UserAgent, cliProfile.UserAgent)
	}
}

func TestResolveClaudeDeviceProfileRequiredNonHomeKeepsLocalCache(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	client := newFakeClaudeDeviceProfileKVClient()
	useFakeClaudeDeviceProfileKVClient(t, client, false, nil)
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	cfg := &config.Config{}

	first, errFirst := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), exactClaudeDeviceProfileTestConfig(cfg))
	if errFirst != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() first error = %v", errFirst)
	}
	second, errSecond := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "api-key", nil, exactClaudeDeviceProfileTestConfig(cfg))
	if errSecond != nil {
		t.Fatalf("ResolveClaudeDeviceProfileRequired() second error = %v", errSecond)
	}
	if second.UserAgent != first.UserAgent {
		t.Fatalf("cached UserAgent = %q, want %q", second.UserAgent, first.UserAgent)
	}
	if client.getCount != 0 || client.setCount != 0 || client.setNXCount != 0 {
		t.Fatalf("KV calls = get %d set %d setnx %d, want all zero", client.getCount, client.setCount, client.setNXCount)
	}
}

func exactClaudeDeviceProfileTestConfig(cfg *config.Config) *config.Config {
	var copy config.Config
	if cfg != nil {
		copy = *cfg
	}
	copy.ClaudeHeaderDefaults.PinSoftwareBaseline = true
	return &copy
}

// These isolation fixtures model the upstream 280 capture explicitly. Pinning
// chooses the configured baseline; it never upgrades the fork's default floor.
func captured280ClaudeDeviceConfig() *config.Config {
	return exactClaudeDeviceProfileTestConfig(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		UserAgent: "claude-cli/2.1.280 (external, cli)", PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0",
	}})
}
func captured280ClaudeDeviceHeaders(ua string) http.Header {
	headers := claudeDeviceHeaders(ua)
	headers.Set("X-Stainless-Package-Version", "0.112.1")
	headers.Set("X-Stainless-Runtime-Version", "v26.3.0")
	return headers
}
func TestPinSoftwareBaselineWithoutOverridesUsesForkFloor(t *testing.T) {
	for _, home := range []bool{false, true} {
		t.Run(fmt.Sprintf("home=%v", home), func(t *testing.T) {
			ResetClaudeDeviceProfileCache()
			t.Cleanup(ResetClaudeDeviceProfileCache)
			useFakeClaudeDeviceProfileKVClient(t, newFakeClaudeDeviceProfileKVClient(), home, nil)
			cfg := exactClaudeDeviceProfileTestConfig(nil)
			profile, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "pin-default"}, "key", captured280ClaudeDeviceHeaders("claude-cli/2.1.280 (external, cli)"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if profile.UserAgent != "claude-cli/2.1.211 (external, cli)" || profile.PackageVersion != "0.94.0" || profile.RuntimeVersion != "v26.3.0" {
				t.Fatalf("pin without overrides changed fork floor: %+v", profile)
			}
		})
	}
}
func TestPinSoftwareBaselineDoesNotOverrideFarmMeasurement(t *testing.T) {
	for _, home := range []bool{false, true} {
		t.Run(fmt.Sprintf("home=%v", home), func(t *testing.T) {
			ResetClaudeDeviceProfileCache()
			t.Cleanup(ResetClaudeDeviceProfileCache)
			useFakeClaudeDeviceProfileKVClient(t, newFakeClaudeDeviceProfileKVClient(), home, nil)
			cfg := captured280ClaudeDeviceConfig()
			cfg.ClaudeHeaderDefaults.FarmOS = "Linux"
			cfg.ClaudeHeaderDefaults.FarmArch = "x64"
			profile, err := ResolveClaudeDeviceProfileRequired(context.Background(), farmBoundClaudeAuth("pin-farm"), "key", claudeInboundHeaders("2.1.197"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if profile.VersionString() != "2.1.197" || profile.OS != "Linux" || profile.Arch != "x64" {
				t.Fatalf("pin overrode farm measurement/platform: %+v", profile)
			}
		})
	}
}
