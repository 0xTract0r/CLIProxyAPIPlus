package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func resetCodexClientProfileCache() {
	codexClientProfileCacheMu.Lock()
	codexClientProfileCache = make(map[string]codexClientProfileCacheEntry)
	codexClientProfileCacheMu.Unlock()
}

func TestResolveCodexClientProfile_DefaultFallbackUsesCodexProxyDesktopProfile(t *testing.T) {
	resetCodexClientProfileCache()

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{
		ID:       "codex-default-auth",
		Provider: "codex",
	}, nil, &config.Config{})

	if got := profile.Originator; got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop community fallback", got)
	}
	if !strings.HasPrefix(profile.UserAgent, "Codex Desktop/26.318.11754 (darwin; arm64)") {
		t.Fatalf("User-Agent = %q, want Codex Desktop product", profile.UserAgent)
	}
	if got := profile.SecCHUA; !strings.Contains(got, `"Chromium";v="144"`) {
		t.Fatalf("sec-ch-ua = %q, want Codex-Proxy Chromium 144 marker", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy community source", got)
	}
}

func TestResolveCodexClientProfile_DefaultPolicyIgnoresObservedCLIHeaders(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{
		ID:       "codex-profile-auth",
		Provider: "codex",
	}
	cfg := &config.Config{}

	firstProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"0.124.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := firstProfile.UserAgent; !strings.HasPrefix(got, "Codex Desktop/26.318.11754") {
		t.Fatalf("first profile User-Agent = %q, want Codex Desktop policy", got)
	}
	if got := firstProfile.Originator; got != "Codex Desktop" {
		t.Fatalf("first profile Originator = %q, want Codex Desktop", got)
	}
	if got := firstProfile.Version; got != "26.318.11754" {
		t.Fatalf("first profile Version = %q, want Codex Desktop app version", got)
	}
	if got := firstProfile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("first profile Source = %q, want codex-proxy", got)
	}

	upgradedProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.125.0 (Mac OS 15.6.0; arm64) Ghostty/1.0.0"},
		"Version":    []string{"0.125.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := upgradedProfile.Version; got != "26.318.11754" {
		t.Fatalf("upgraded profile Version = %q, want Codex Desktop app version", got)
	}
	if got := upgradedProfile.Originator; got != "Codex Desktop" {
		t.Fatalf("upgraded profile Originator = %q, want Codex Desktop", got)
	}
	if strings.Contains(upgradedProfile.UserAgent, "Ghostty/1.0.0") {
		t.Fatalf("upgraded User-Agent unexpectedly changed terminal fingerprint: %q", upgradedProfile.UserAgent)
	}
	if strings.Contains(upgradedProfile.UserAgent, "Mac OS 15.6.0") {
		t.Fatalf("upgraded User-Agent unexpectedly changed platform fingerprint: %q", upgradedProfile.UserAgent)
	}
	if !strings.Contains(upgradedProfile.UserAgent, "Codex Desktop/26.318.11754") {
		t.Fatalf("upgraded User-Agent did not keep Codex Desktop marker: %q", upgradedProfile.UserAgent)
	}
}

func TestResolveCodexClientProfile_ObservedCodexTuiPinsConsistentOriginatorAndUserAgent(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{
		ID:       "codex-tui-observed-auth",
		Provider: "codex",
	}
	profile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex-tui/0.126.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.126.0)"},
		"Version":    []string{"0.126.0"},
		"Originator": []string{"codex-tui"},
	}, &config.Config{})

	if got := profile.Originator; got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop policy", got)
	}
	if got := profile.UserAgentProduct; got != "Codex Desktop" {
		t.Fatalf("UserAgentProduct = %q, want Codex Desktop", got)
	}
	if strings.Contains(profile.UserAgent, "codex-tui") {
		t.Fatalf("User-Agent should not expose observed codex-tui under default policy: %q", profile.UserAgent)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy", got)
	}
}

func TestResolveCodexClientProfile_ReconcilesMismatchedFirstPartyOriginatorAndUserAgent(t *testing.T) {
	resetCodexClientProfileCache()

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{
		ID:       "codex-mismatched-observed-auth",
		Provider: "codex",
	}, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.126.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex_cli_rs; 0.126.0)"},
		"Version":    []string{"0.126.0"},
		"Originator": []string{"codex-tui"},
	}, &config.Config{})

	if got := profile.Originator; got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop policy", got)
	}
	if got := profile.UserAgentProduct; got != "Codex Desktop" {
		t.Fatalf("UserAgentProduct = %q, want Codex Desktop", got)
	}
	if strings.Contains(profile.UserAgent, "codex_cli_rs/") || strings.Contains(profile.UserAgent, "codex-tui") {
		t.Fatalf("User-Agent should not use observed CLI identity under default policy: %q", profile.UserAgent)
	}
}

func TestCodexManagedHeaders_IncludeStructuredVersionAndOriginator(t *testing.T) {
	headers := CodexManagedHeaders(CodexClientProfile{
		UserAgent:    "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
		Version:      "0.124.0",
		Originator:   "codex-tui",
		BetaFeatures: "feature-a",
	})

	if got := headers["Version"]; got != "0.124.0" {
		t.Fatalf("Version = %q, want %q", got, "0.124.0")
	}
	if got := headers["Originator"]; got != "codex-tui" {
		t.Fatalf("Originator = %q, want %q", got, "codex-tui")
	}
	if got := headers["User-Agent"]; got == "" {
		t.Fatal("User-Agent should not be empty")
	}
}

func TestResolveCodexClientProfile_DesktopDefaultDoesNotUseNPMCliVersion(t *testing.T) {
	resetCodexClientProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "0.130.0",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:    managedHeaderProfileSourceNPM,
				SourceURL: codexNPMURL,
				CheckedAt: "2026-04-29T12:00:00Z",
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{
		ID:       "codex-online-auth",
		Provider: "codex",
	}, nil, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if got := profile.Version; got != "26.318.11754" {
		t.Fatalf("Version = %q, want Desktop app version to remain pinned", got)
	}
	if !strings.Contains(profile.UserAgent, "Codex Desktop/26.318.11754") {
		t.Fatalf("User-Agent did not keep Codex Desktop app marker: %q", profile.UserAgent)
	}
	if strings.Contains(profile.UserAgent, "codex_cli_rs/0.130.0") {
		t.Fatalf("Desktop profile must not mix npm CLI package marker into UA: %q", profile.UserAgent)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy community source", got)
	}
	if got := profile.Source.SourceURL; got != "https://github.com/icebear0828/codex-proxy" {
		t.Fatalf("SourceURL = %q, want codex-proxy URL", got)
	}
}

func TestResolveCodexClientProfile_OnlineCodexProxyBundleUpdatesDesktopMarkers(t *testing.T) {
	resetCodexClientProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return ManagedHeaderOnlineVersion{}, false
		}
		return ManagedHeaderOnlineVersion{
			Version: "26.400.1",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:       managedHeaderProfileSourceCodexProxy,
				SourceURL:    codexProxyDefaultConfigURL + " " + codexProxyFingerprintConfigURL,
				CheckedAt:    "2026-05-09T02:00:00Z",
				Completeness: "online-coherent-bundle",
			},
			CodexProxyBundle: &CodexProxyManagedHeaderBundle{
				Originator:      "Codex Desktop",
				AppVersion:      "26.400.1",
				Platform:        "darwin",
				Arch:            "arm64",
				ChromiumVersion: "145",
				DefaultHeaders: map[string]string{
					"sec-ch-ua":          `"Chromium";v="145", "Not A(Brand";v="24"`,
					"sec-ch-ua-mobile":   "?0",
					"sec-ch-ua-platform": `"macOS"`,
					"Accept-Encoding":    "gzip, deflate, br, zstd",
					"Accept-Language":    "en-US,en;q=0.9",
					"sec-fetch-site":     "same-origin",
					"sec-fetch-mode":     "cors",
					"sec-fetch-dest":     "empty",
				},
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{
		ID:       "codex-online-proxy-auth",
		Provider: "codex",
	}, nil, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if got := profile.Version; got != "26.400.1" {
		t.Fatalf("Version = %q, want online codex-proxy app version", got)
	}
	if !strings.Contains(profile.UserAgent, "Codex Desktop/26.400.1") {
		t.Fatalf("User-Agent = %q, want online Codex Desktop app marker", profile.UserAgent)
	}
	if got := profile.PlatformToken; got != "darwin; arm64" {
		t.Fatalf("PlatformToken = %q, want darwin; arm64", got)
	}
	if got := profile.SecCHUA; !strings.Contains(got, `"Chromium";v="145"`) {
		t.Fatalf("sec-ch-ua = %q, want online chromium marker", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy", got)
	}
	if got := profile.Source.CheckedAt; got != "2026-05-09T02:00:00Z" {
		t.Fatalf("CheckedAt = %q", got)
	}
	if got := profile.Source.Completeness; got != "online-coherent-bundle" {
		t.Fatalf("Completeness = %q", got)
	}
}

func TestResolveCodexClientProfile_OnlineVersionDoesNotOverrideObservedSourceOrOriginator(t *testing.T) {
	resetCodexClientProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return ManagedHeaderOnlineVersion{}, false
		}
		return ManagedHeaderOnlineVersion{
			Version: "0.130.0",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:    managedHeaderProfileSourceNPM,
				SourceURL: codexNPMURL,
				CheckedAt: "2026-04-29T12:00:00Z",
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	auth := &cliproxyauth.Auth{
		ID:       "codex-observed-online-auth",
		Provider: "codex",
	}
	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	}
	observed := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)"},
		"Version":    []string{"0.124.0"},
		"Originator": []string{"codex-tui"},
	}, cfg)
	if got := observed.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("observed Source = %q, want codex-proxy", got)
	}

	profile := ResolveCodexClientProfile(auth, nil, cfg)
	if got := profile.Version; got != "26.318.11754" {
		t.Fatalf("Version = %q, want Codex Desktop app version", got)
	}
	if got := profile.Originator; got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop", got)
	}
	if got := profile.UserAgentProduct; got != "Codex Desktop" {
		t.Fatalf("UserAgentProduct = %q, want Codex Desktop", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy", got)
	}
	if !strings.Contains(profile.UserAgent, "Codex Desktop/26.318.11754") {
		t.Fatalf("User-Agent did not keep Codex Desktop identity: %q", profile.UserAgent)
	}
}

func TestResolveCodexClientProfile_OnlineVersionBumpsPersistedHeaders(t *testing.T) {
	resetCodexClientProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return ManagedHeaderOnlineVersion{}, false
		}
		return ManagedHeaderOnlineVersion{
			Version: "0.130.0",
			ManagedHeaderProfileSource: ManagedHeaderProfileSource{
				Source:    managedHeaderProfileSourceNPM,
				SourceURL: codexNPMURL,
				CheckedAt: "2026-04-29T12:00:00Z",
			},
		}, true
	}
	t.Cleanup(func() {
		ManagedHeaderOnlineFetchOverride = oldOverride
		resetManagedHeaderOnlineProfileCacheForTests()
	})

	auth := &cliproxyauth.Auth{
		ID:       "codex-online-persisted-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent": "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
				"Version":    "0.124.0",
				"Originator": "codex-tui",
			},
		},
	}

	profile := ResolveCodexClientProfile(auth, nil, &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{
			OnlineUpdate: &online,
		},
	})

	if got := profile.Version; got != "26.318.11754" {
		t.Fatalf("Version = %q, want Codex Desktop app version", got)
	}
	if !strings.Contains(profile.UserAgent, "Codex Desktop/26.318.11754") {
		t.Fatalf("User-Agent did not keep Codex Desktop marker: %q", profile.UserAgent)
	}
	if strings.Contains(profile.UserAgent, "iTerm.app/3.6.9") {
		t.Fatalf("User-Agent should not keep legacy terminal fingerprint under default policy: %q", profile.UserAgent)
	}
	if got := profile.Originator; got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexProxy {
		t.Fatalf("Source = %q, want codex-proxy", got)
	}
}
