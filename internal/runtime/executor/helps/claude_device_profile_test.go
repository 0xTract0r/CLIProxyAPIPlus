package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
