package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func resetCodexClientProfileCache() {
	codexClientProfileCacheMu.Lock()
	codexClientProfileCache = make(map[string]codexClientProfileCacheEntry)
	codexClientProfileCacheMu.Unlock()
}

func TestResolveCodexClientProfile_DefaultFallbackUsesCodexProxyDesktopProfile(t *testing.T) {
	resetCodexClientProfileCache()

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct",
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

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
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

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
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

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct",
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

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct",
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

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct",
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

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
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

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
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

// TestExtractCodexClientProfile_SanityCeilingRejectsFabricatedHighVersion 覆盖 A-2：
// 持有合法下游 key 的人伪造荒谬高版本（CLI 家族 999.0.0 / Desktop 家族 999.999.99999）
// 必须在源级被拒，不进入观测，避免污染 high-water 并被当成出站版本。
func TestExtractCodexClientProfile_SanityCeilingRejectsFabricatedHighVersion(t *testing.T) {
	cfg := &config.Config{}

	cliForged := http.Header{
		"User-Agent": []string{"codex_cli_rs/999.0.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"999.0.0"},
		"Originator": []string{"codex_cli_rs"},
	}
	if profile, ok := extractCodexClientProfile(cliForged, cfg); ok {
		t.Fatalf("forged codex_cli_rs/999.0.0 accepted as observation %#v, want rejected", profile)
	}

	desktopForged := http.Header{
		"User-Agent": []string{"Codex Desktop/999.999.99999 (darwin; arm64)"},
		"Version":    []string{"999.999.99999"},
		"Originator": []string{"Codex Desktop"},
	}
	if profile, ok := extractCodexClientProfile(desktopForged, cfg); ok {
		t.Fatalf("forged Codex Desktop/999.999.99999 accepted as observation %#v, want rejected", profile)
	}
}

// TestExtractCodexClientProfile_SanityCeilingAcceptsRealRecentVersion 覆盖 A-2：
// 真实近期版本（仍在静态上限以内）必须被接受，确保 ceiling 不误拒正常客户端。
func TestExtractCodexClientProfile_SanityCeilingAcceptsRealRecentVersion(t *testing.T) {
	cfg := &config.Config{}

	cliReal := http.Header{
		"User-Agent": []string{"codex_cli_rs/0.140.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"0.140.0"},
		"Originator": []string{"codex_cli_rs"},
	}
	profile, ok := extractCodexClientProfile(cliReal, cfg)
	if !ok {
		t.Fatalf("real codex_cli_rs/0.140.0 rejected, want accepted")
	}
	if got := profile.Version; got != "0.140.0" {
		t.Fatalf("accepted CLI Version = %q, want 0.140.0", got)
	}

	desktopReal := http.Header{
		"User-Agent": []string{"Codex Desktop/26.318.11754 (darwin; arm64)"},
		"Version":    []string{"26.318.11754"},
		"Originator": []string{"Codex Desktop"},
	}
	if _, ok := extractCodexClientProfile(desktopReal, cfg); !ok {
		t.Fatalf("real Codex Desktop/26.318.11754 rejected, want accepted")
	}
}

// TestCodexObservationSanityCeiling_LiftedByOnlineLatest 覆盖 A-2 线上抬 ceiling 分支：
// 当 codex online latest 高于静态上限时，有效 ceiling 应被抬到线上 latest，
// 让真正的前沿真实客户端不被误拒；但只抬上限，不改出站版本。
func TestCodexObservationSanityCeiling_LiftedByOnlineLatest(t *testing.T) {
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return ManagedHeaderOnlineVersion{}, false
		}
		// 线上 latest 高于 CLI 静态上限 1.0.0。
		return ManagedHeaderOnlineVersion{
			Version: "2.5.0",
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

	cfg := &config.Config{
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &online},
	}
	cliProfile := CodexClientProfile{Originator: "codex_cli_rs", UserAgentProduct: "codex_cli_rs"}
	ceiling := codexObservationSanityCeiling(cliProfile, cfg)
	if ceiling.Compare(codexStaticSanityCeiling(cliProfile)) <= 0 {
		t.Fatalf("online-raised CLI ceiling %+v must exceed static ceiling %+v", ceiling, codexStaticSanityCeiling(cliProfile))
	}

	// candidate 2.4.0 在线上抬升后的上限内，应被接受。
	withinHeader := http.Header{
		"User-Agent": []string{"codex_cli_rs/2.4.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"2.4.0"},
		"Originator": []string{"codex_cli_rs"},
	}
	if _, ok := extractCodexClientProfile(withinHeader, cfg); !ok {
		t.Fatalf("codex_cli_rs/2.4.0 rejected under online-raised ceiling, want accepted")
	}
}

// TestCodexObservationSanityCeiling_OfflineConstantApplies 覆盖 A-2 离线分支：
// online-update 未开启时，有效 ceiling 等于硬编码静态上限，超界值离线即被拒。
func TestCodexObservationSanityCeiling_OfflineConstantApplies(t *testing.T) {
	cfg := &config.Config{}
	cliProfile := CodexClientProfile{Originator: "codex_cli_rs", UserAgentProduct: "codex_cli_rs"}
	if ceiling := codexObservationSanityCeiling(cliProfile, cfg); ceiling.Compare(codexStaticSanityCeiling(cliProfile)) != 0 {
		t.Fatalf("offline CLI ceiling = %+v, want static ceiling %+v", ceiling, codexStaticSanityCeiling(cliProfile))
	}
	desktopProfile := CodexClientProfile{Originator: "Codex Desktop", UserAgentProduct: "Codex Desktop"}
	if ceiling := codexObservationSanityCeiling(desktopProfile, cfg); ceiling.Compare(codexStaticSanityCeiling(desktopProfile)) != 0 {
		t.Fatalf("offline Desktop ceiling = %+v, want static ceiling %+v", ceiling, codexStaticSanityCeiling(desktopProfile))
	}
}

// TestIsFirstPartyCodexOriginator_Whitelist 覆盖 A-1 白名单的导出包装：合法 first-party
// Originator 通过，伪造/路人值被拒。executor 侧据此决定是否保留客户端 Originator。
func TestIsFirstPartyCodexOriginator_Whitelist(t *testing.T) {
	for _, ok := range []string{"codex-tui", "codex_cli_rs", "codex_vscode", "codex_exec", "Codex Desktop"} {
		if !IsFirstPartyCodexOriginator(ok) {
			t.Fatalf("IsFirstPartyCodexOriginator(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "evil-client", "curl/8.1", "Codex", "codex"} {
		if IsFirstPartyCodexOriginator(bad) {
			t.Fatalf("IsFirstPartyCodexOriginator(%q) = true, want false", bad)
		}
	}
}
