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

// fork(anticorr Wave10-D)：默认画像从冻结 "Codex Desktop" 改成 codex_cli_rs CLI。
// default fallback 出站应是 CLI 家族（Originator=codex_cli_rs、UA 三段式、floor 0.140.0），
// 且不带 Desktop 专属 sec-ch-ua。
func TestResolveCodexClientProfile_DefaultFallbackUsesCodexCLIProfile(t *testing.T) {
	resetCodexClientProfileCache()

	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-default-auth",
		Provider: "codex",
	}, nil, &config.Config{})

	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs CLI default", got)
	}
	if !strings.HasPrefix(profile.UserAgent, "codex_cli_rs/0.140.0 (Mac OS 15.7.4; arm64) iTerm.app/3.6.8 (codex_cli_rs; 0.140.0)") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs CLI product", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
	if got := profile.Source.Source; got != managedHeaderProfileSourceCodexCLI {
		t.Fatalf("Source = %q, want static codex-cli source", got)
	}
}

// fork(anticorr Wave10-D)：CLI 策略下接观测高水位，但 OS/arch/terminal 稳定 pin，
// 不透传客户端真实环境（Ghostty / Mac OS 15.6.0）。
func TestResolveCodexClientProfile_CLIPolicyTracksVersionHighWaterButPinsPlatform(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-profile-auth",
		Provider: "codex",
	}
	cfg := &config.Config{}

	firstProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.141.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"0.141.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := firstProfile.Originator; got != "codex_cli_rs" {
		t.Fatalf("first profile Originator = %q, want codex_cli_rs", got)
	}
	if got := firstProfile.Version; got != "0.141.0" {
		t.Fatalf("first profile Version = %q, want observed high-water 0.141.0", got)
	}
	if !strings.Contains(firstProfile.UserAgent, "Mac OS 15.7.4; arm64") {
		t.Fatalf("first profile User-Agent did not keep pinned platform: %q", firstProfile.UserAgent)
	}
	if strings.Contains(firstProfile.UserAgent, "Mac OS 15.5.0") {
		t.Fatalf("first profile leaked observed platform: %q", firstProfile.UserAgent)
	}

	upgradedProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.142.0 (Mac OS 15.6.0; arm64) Ghostty/1.0.0"},
		"Version":    []string{"0.142.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := upgradedProfile.Version; got != "0.142.0" {
		t.Fatalf("upgraded profile Version = %q, want bumped high-water 0.142.0", got)
	}
	if got := upgradedProfile.Originator; got != "codex_cli_rs" {
		t.Fatalf("upgraded profile Originator = %q, want codex_cli_rs", got)
	}
	if strings.Contains(upgradedProfile.UserAgent, "Ghostty/1.0.0") {
		t.Fatalf("upgraded User-Agent leaked observed terminal fingerprint: %q", upgradedProfile.UserAgent)
	}
	if strings.Contains(upgradedProfile.UserAgent, "Mac OS 15.6.0") {
		t.Fatalf("upgraded User-Agent leaked observed platform fingerprint: %q", upgradedProfile.UserAgent)
	}
	if !strings.Contains(upgradedProfile.UserAgent, "iTerm.app/3.6.8 (codex_cli_rs; 0.142.0)") {
		t.Fatalf("upgraded User-Agent did not keep pinned terminal with bumped version: %q", upgradedProfile.UserAgent)
	}
}

// fork(anticorr Wave10-D)：观测到 codex-tui first-party 时，CLI 策略钉死出站 Originator
// 为 codex_cli_rs（受管家族），只采纳版本，不暴露 codex-tui 终端身份给上游。
func TestResolveCodexClientProfile_ObservedCodexTuiPinsCLIOriginator(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-tui-observed-auth",
		Provider: "codex",
	}
	profile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex-tui/0.143.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.143.0)"},
		"Version":    []string{"0.143.0"},
		"Originator": []string{"codex-tui"},
	}, &config.Config{})

	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs CLI policy", got)
	}
	if got := profile.UserAgentProduct; got != "codex_cli_rs" {
		t.Fatalf("UserAgentProduct = %q, want codex_cli_rs", got)
	}
	if strings.Contains(profile.UserAgent, "codex-tui") {
		t.Fatalf("User-Agent should not expose observed codex-tui under CLI policy: %q", profile.UserAgent)
	}
	if got := profile.Version; got != "0.143.0" {
		t.Fatalf("Version = %q, want observed high-water 0.143.0", got)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
}

// TestEnforceCodexManagedFamily_PersistedDesktopVersionFallsBackToCLIFloor 覆盖本次
// 修复的核心缺口：账号 persisted/cached 了历史 Desktop bundle，其 Version 是
// year.day.build 的 26.318.11753；CLI baseline 下经 enforceCodexManagedFamily 时，
// 旧逻辑用纯数值 max（26>0）把 Desktop 版本保留，导致出站 Version=26.318.11753、
// UA=codex_cli_rs/26.318.11753（CLI 身份配 Desktop 版本号，自相矛盾）。
// 修复后必须丢弃 Desktop 版本，回落 CLI floor 0.140.0，UA 不含 26.x。
func TestEnforceCodexManagedFamily_PersistedDesktopVersionFallsBackToCLIFloor(t *testing.T) {
	baseline := defaultCodexClientProfile(&config.Config{})
	if baseline.isCodexDesktopLike() {
		t.Fatalf("baseline should be CLI family, got Desktop-like: %#v", baseline)
	}

	// persisted 的历史 Desktop 受管 bundle（Originator=Codex Desktop、Version=26.318.11753）。
	persisted := normalizeCodexClientProfile(CodexClientProfile{
		UserAgent:  "Codex Desktop/26.318.11753 (darwin; arm64)",
		Version:    "26.318.11753",
		Originator: "Codex Desktop",
	}, baseline)
	if got := persisted.Version; got != "26.318.11753" {
		t.Fatalf("persisted Version setup = %q, want 26.318.11753", got)
	}

	coerced := enforceCodexManagedFamily(persisted, baseline)

	if got := coerced.Version; got != "0.140.0" {
		t.Fatalf("Version = %q, want CLI floor 0.140.0 (Desktop 26.x must not be kept)", got)
	}
	if got := coerced.UserAgentVersion; got != "0.140.0" {
		t.Fatalf("UserAgentVersion = %q, want 0.140.0", got)
	}
	if got := coerced.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if strings.Contains(coerced.UserAgent, "26") {
		t.Fatalf("User-Agent still contains Desktop 26.x version: %q", coerced.UserAgent)
	}
	if !strings.HasPrefix(coerced.UserAgent, "codex_cli_rs/0.140.0") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs/0.140.0 prefix", coerced.UserAgent)
	}
	if got := coerced.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
}

// TestEnforceCodexManagedFamily_RealCLIObservationBumps 确认家族门不误伤真实 CLI 观测：
// 同家族、ceiling 以内的 CLI 版本（0.141.0 > floor 0.140.0）必须能正常抬升 high-water。
func TestEnforceCodexManagedFamily_RealCLIObservationBumps(t *testing.T) {
	baseline := defaultCodexClientProfile(&config.Config{})

	observed := normalizeCodexClientProfile(CodexClientProfile{
		UserAgent:  "codex_cli_rs/0.141.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0 (codex_cli_rs; 0.141.0)",
		Version:    "0.141.0",
		Originator: "codex_cli_rs",
	}, baseline)

	coerced := enforceCodexManagedFamily(observed, baseline)
	if got := coerced.Version; got != "0.141.0" {
		t.Fatalf("Version = %q, want bumped CLI high-water 0.141.0", got)
	}
	if !strings.Contains(coerced.UserAgent, "codex_cli_rs/0.141.0") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs/0.141.0", coerced.UserAgent)
	}
}

// TestEnforceCodexManagedFamily_OverCeilingCLIVersionFallsBackToFloor 确认超 CLI
// ceiling(1.0.0) 的同家族版本（如伪造的 2.5.0）也被丢弃回落 floor，不污染出站。
func TestEnforceCodexManagedFamily_OverCeilingCLIVersionFallsBackToFloor(t *testing.T) {
	baseline := defaultCodexClientProfile(&config.Config{})

	overCeiling := normalizeCodexClientProfile(CodexClientProfile{
		UserAgent:  "codex_cli_rs/2.5.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0 (codex_cli_rs; 2.5.0)",
		Version:    "2.5.0",
		Originator: "codex_cli_rs",
	}, baseline)

	coerced := enforceCodexManagedFamily(overCeiling, baseline)
	if got := coerced.Version; got != "0.140.0" {
		t.Fatalf("Version = %q, want CLI floor 0.140.0 (over-ceiling 2.5.0 must be dropped)", got)
	}
	if strings.Contains(coerced.UserAgent, "2.5.0") {
		t.Fatalf("User-Agent still contains over-ceiling version: %q", coerced.UserAgent)
	}
}

// TestResolveCodexClientProfile_PersistedDesktopBundleVersionDoesNotContaminateCLI
// 端到端覆盖：账号 metadata.headers 残留 Desktop bundle（Version=26.318.11753），
// 经完整 ResolveCodexClientProfile 后出站 Version 必须是 CLI floor 0.140.0、
// UA=codex_cli_rs/0.140.0（不出现 26.x），Originator 钉死 codex_cli_rs、无 sec-ch-ua。
func TestResolveCodexClientProfile_PersistedDesktopBundleVersionDoesNotContaminateCLI(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-persisted-desktop-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent": "Codex Desktop/26.318.11753 (darwin; arm64)",
				"Version":    "26.318.11753",
				"Originator": "Codex Desktop",
			},
		},
	}

	profile := ResolveCodexClientProfile(auth, nil, &config.Config{})

	if got := profile.Version; got != "0.140.0" {
		t.Fatalf("Version = %q, want CLI floor 0.140.0 (persisted Desktop 26.x must not leak)", got)
	}
	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if strings.Contains(profile.UserAgent, "26") {
		t.Fatalf("User-Agent still contains Desktop 26.x version: %q", profile.UserAgent)
	}
	if strings.Contains(profile.UserAgent, "Codex Desktop") {
		t.Fatalf("User-Agent must not be Desktop: %q", profile.UserAgent)
	}
	if !strings.HasPrefix(profile.UserAgent, "codex_cli_rs/0.140.0") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs/0.140.0 prefix", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
}

// TestBumpCodexVersionMarkers_DesktopCandidateDoesNotBumpCLI 覆盖要点2：current 是 CLI
// 家族时，Desktop 家族 candidate（26.318.11753）不得把 CLI high-water 抬高。
func TestBumpCodexVersionMarkers_DesktopCandidateDoesNotBumpCLI(t *testing.T) {
	current := defaultCodexClientProfile(&config.Config{}) // CLI floor 0.140.0

	desktopCandidate := normalizeCodexClientProfile(CodexClientProfile{
		UserAgent:  "Codex Desktop/26.318.11753 (darwin; arm64)",
		Version:    "26.318.11753",
		Originator: "Codex Desktop",
	}, current)

	next := bumpCodexVersionMarkers(desktopCandidate, current)
	if got := next.Version; got != "0.140.0" {
		t.Fatalf("Version = %q, want unchanged CLI floor 0.140.0 (Desktop candidate must not bump CLI)", got)
	}
	if got := next.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if strings.Contains(next.UserAgent, "26") {
		t.Fatalf("User-Agent contaminated by Desktop version: %q", next.UserAgent)
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

// fork(anticorr Wave10-D)：未观测到任何客户端时，CLI 出站版本是 floor 0.140.0，不被
// 低于 floor 的观测降级。
func TestResolveCodexClientProfile_CLIFloorVersion(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "codex-floor-auth", Provider: "codex"}
	cfg := &config.Config{}

	floor := ResolveCodexClientProfile(auth, nil, cfg)
	if got := floor.Version; got != "0.140.0" {
		t.Fatalf("default Version = %q, want floor 0.140.0", got)
	}

	// 观测到低于 floor 的版本（0.120.0），出站不应被降级到 0.120.0。
	lower := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0 (codex_cli_rs; 0.120.0)"},
		"Version":    []string{"0.120.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)
	if got := lower.Version; got != "0.140.0" {
		t.Fatalf("Version after lower observation = %q, want floor 0.140.0 (only-up high-water)", got)
	}
}

// fork(anticorr Wave10-D)：CLI 画像可 config pin OS/arch/terminal/originator。
func TestResolveCodexClientProfile_ConfigPinsOSArchTerminal(t *testing.T) {
	resetCodexClientProfileCache()

	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			Originator: "codex_cli_rs",
			OS:         "Mac OS 15.6.1",
			Arch:       "arm64",
			Terminal:   "WezTerm/20240203",
		},
	}
	profile := ResolveCodexClientProfile(&cliproxyauth.Auth{ProxyURL: "direct", ID: "codex-cfg-pin", Provider: "codex"}, nil, cfg)

	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if !strings.Contains(profile.UserAgent, "Mac OS 15.6.1; arm64") {
		t.Fatalf("User-Agent = %q, want config-pinned OS/arch", profile.UserAgent)
	}
	if !strings.Contains(profile.UserAgent, "WezTerm/20240203 (codex_cli_rs; 0.140.0)") {
		t.Fatalf("User-Agent = %q, want config-pinned terminal", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
}

// fork(anticorr Wave10-D)：CLI 默认下，npm CLI online latest 高于 floor 时抬高出站
// CLI 版本（high-water），且保持 codex_cli_rs 身份与 CLI UA 三段式。
func TestResolveCodexClientProfile_CLIDefaultBumpsToNPMCliVersion(t *testing.T) {
	resetCodexClientProfileCache()
	resetManagedHeaderOnlineProfileCacheForTests()
	online := true
	oldOverride := ManagedHeaderOnlineFetchOverride
	ManagedHeaderOnlineFetchOverride = func(provider string, cfg *config.Config) (managedHeaderOnlineVersion, bool) {
		if provider != "codex" {
			return managedHeaderOnlineVersion{}, false
		}
		return managedHeaderOnlineVersion{
			Version: "0.150.0",
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

	if got := profile.Version; got != "0.150.0" {
		t.Fatalf("Version = %q, want bumped npm CLI version 0.150.0", got)
	}
	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if !strings.Contains(profile.UserAgent, "codex_cli_rs/0.150.0") {
		t.Fatalf("User-Agent did not pick up bumped CLI version: %q", profile.UserAgent)
	}
	if strings.Contains(profile.UserAgent, "Codex Desktop") {
		t.Fatalf("CLI profile must not mix Codex Desktop into UA: %q", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
	}
}

// fork(anticorr Wave10-D 要点1/2)：online codex-proxy Desktop bundle 不得把 CLI 默认画像
// 切回 Desktop。出站必须保持 codex_cli_rs / 无 sec-ch-ua。
func TestResolveCodexClientProfile_OnlineCodexProxyBundleDoesNotSwitchCLIToDesktop(t *testing.T) {
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

	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs (Desktop bundle must not switch family)", got)
	}
	if strings.Contains(profile.UserAgent, "Codex Desktop") {
		t.Fatalf("User-Agent must not be Desktop: %q", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty (Desktop bundle sec-ch-ua must not leak)", got)
	}
	if got := profile.Version; got == "26.400.1" {
		t.Fatalf("Version = %q, Desktop bundle version must not contaminate CLI family", got)
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
		"User-Agent": []string{"codex-tui/0.141.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.141.0)"},
		"Version":    []string{"0.141.0"},
		"Originator": []string{"codex-tui"},
	}, cfg)
	// fork(anticorr Wave10-D)：CLI 策略下出站 Originator 钉死 codex_cli_rs，版本采纳观测。
	if got := observed.Originator; got != "codex_cli_rs" {
		t.Fatalf("observed Originator = %q, want codex_cli_rs", got)
	}
	if got := observed.Version; got != "0.141.0" {
		t.Fatalf("observed Version = %q, want 0.141.0 high-water", got)
	}

	profile := ResolveCodexClientProfile(auth, nil, cfg)
	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if got := profile.UserAgentProduct; got != "codex_cli_rs" {
		t.Fatalf("UserAgentProduct = %q, want codex_cli_rs", got)
	}
	if strings.Contains(profile.UserAgent, "Codex Desktop") {
		t.Fatalf("User-Agent must not be Desktop: %q", profile.UserAgent)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
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
			Version: "0.151.0",
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

	// persisted CLI bundle（codex-tui）+ npm online 抬版本：出站身份钉死 codex_cli_rs，
	// 版本走 high-water（online 0.151.0 高于 floor 0.140.0 与 persisted 0.124.0）。
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

	if got := profile.Version; got != "0.151.0" {
		t.Fatalf("Version = %q, want online npm CLI high-water 0.151.0", got)
	}
	if !strings.Contains(profile.UserAgent, "codex_cli_rs/0.151.0") {
		t.Fatalf("User-Agent did not pick up bumped CLI version: %q", profile.UserAgent)
	}
	if strings.Contains(profile.UserAgent, "iTerm.app/3.6.9") {
		t.Fatalf("User-Agent should not keep persisted terminal fingerprint: %q", profile.UserAgent)
	}
	if got := profile.Originator; got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	if got := profile.SecCHUA; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile", got)
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
