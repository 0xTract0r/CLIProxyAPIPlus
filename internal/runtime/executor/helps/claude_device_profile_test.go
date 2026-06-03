package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestResolveClaudeDeviceProfile_UsesOnlineRegistryVersionWithoutChangingPinnedRuntimeFingerprint(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "claude" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "2.3.4",
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

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{
		ID:       "claude-online-auth",
		Provider: "claude",
	}, "", nil, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if !strings.Contains(profile.UserAgent, "claude-cli/2.3.4") {
		t.Fatalf("UserAgent = %q, want online claude-cli version", profile.UserAgent)
	}
	if got := profile.PackageVersion; got != defaultClaudeFingerprintPackageVersion {
		t.Fatalf("PackageVersion = %q, want unchanged verified baseline", got)
	}
	if got := profile.RuntimeVersion; got != defaultClaudeFingerprintRuntimeVersion {
		t.Fatalf("RuntimeVersion = %q, want unchanged verified baseline", got)
	}
	if got := profile.OS; got != defaultClaudeFingerprintOS {
		t.Fatalf("OS = %q, want pinned baseline", got)
	}
	if got := profile.Arch; got != defaultClaudeFingerprintArch {
		t.Fatalf("Arch = %q, want pinned baseline", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceNPM {
		t.Fatalf("Source = %q, want npm", got)
	}
	if got := profile.Source.SourceURL; got != claudeCodeNPMURL {
		t.Fatalf("SourceURL = %q, want %q", got, claudeCodeNPMURL)
	}
	if got := profile.Source.Completeness; got != "partial-cli-version-only" {
		t.Fatalf("Completeness = %q, want partial-cli-version-only", got)
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

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{
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

	auth := &cliproxyauth.Auth{
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
	auth := &cliproxyauth.Auth{ID: "claude-observation-auth", Provider: "claude"}

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
	auth := &cliproxyauth.Auth{ID: "runtime-id-from-loader", FileName: "claude-file-auth.json", Provider: "claude"}

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

	sameFileDifferentRuntimeID := &cliproxyauth.Auth{ID: "management-id-from-loader", FileName: "claude-file-auth.json", Provider: "claude"}
	observations = ClaudeDeviceProfileObservations(sameFileDifferentRuntimeID, "")
	if len(observations) != 1 {
		t.Fatalf("same file observations length = %d, want 1: %#v", len(observations), observations)
	}
}

func TestClaudeDeviceProfileObservations_FileNameAuthIDAliases(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	requestAuth := &cliproxyauth.Auth{ID: "claude-file-auth.json", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.144 (external, cli)"},
		"X-Stainless-Package-Version": {"0.96.0"},
		"X-Stainless-Runtime-Version": {"v24.5.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{FileName: "claude-file-auth.json", Provider: "claude"}
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
	requestAuth := &cliproxyauth.Auth{Label: "bcd898@example.com", Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "runtime-api-key", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": {"0.94.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{Label: "bcd898@example.com", Provider: "claude"}
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
	requestAuth := &cliproxyauth.Auth{Provider: "claude"}

	_ = ResolveClaudeDeviceProfile(requestAuth, "provider-token", map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.141 (external, cli)"},
		"X-Stainless-Package-Version": {"0.93.0"},
		"X-Stainless-Runtime-Version": {"v24.3.0"},
	}, cfg)

	managementAuth := &cliproxyauth.Auth{FileName: "claude-file-auth.json", Provider: "claude"}
	observations := ClaudeDeviceProfileObservations(managementAuth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.141" {
		t.Fatalf("version = %q, want 2.1.141", got)
	}
}
