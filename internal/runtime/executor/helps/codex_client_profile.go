package helps

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	// fork(anticorr Wave10-D): codex 出站画像从冻结的 community "Codex Desktop" 受管画像
	// 改成扮真实 codex-rs CLI（codex_cli_rs）。
	//
	// 背景：真实出站流量的 body 一直是 codex CLI 格式，C 又把 TLS 改成 codex-rs CLI 的
	// rustls（JA3 e4d448cd）；但 UA/Originator 还停在冻结 "Codex Desktop/26.x" + Chromium
	// sec-ch-ua，于是 body/TLS 是 CLI、UA 却是 Desktop，三者互相矛盾，UA 成异类。
	// 这里把代码默认 originator/UA/版本全切到 codex_cli_rs CLI 家族，让 body/UA/TLS/版本
	// 自洽；TLS（C）不动。
	//
	// 选 codex_cli_rs 而不是 codex_exec：interactive 通用入口，isFirstPartyCodexOriginator
	// 两者都接受；真实样本 codex_exec/0.140.0 是 exec 子命令，codex_cli_rs 覆盖更广。
	//
	// 版本走观测 high-water（extractCodexClientProfile→bumpCodexVersionMarkers→online ceiling），
	// 这里 0.140.0 只是离线 floor（接观测高水位起点），不是出站锁。改成 CLI 家族后，
	// isCodexDesktopLike()==false，会解除 ResolveCodexClientProfile 的 Desktop 短路，high-water
	// 链路才接得上。
	//
	// OS/arch/terminal 稳定 pin（仿 claude pinClaudeDeviceProfilePlatform）：固定
	// Mac OS 15.7.4 / arm64 / iTerm.app 3.6.8，不透传真实环境，每账号稳定一致、可 config 覆盖。
	defaultCodexManagedOriginator  = "codex_cli_rs"
	defaultCodexManagedVersion     = "0.140.0"
	defaultCodexManagedPlatform    = "Mac OS 15.7.4; arm64"
	defaultCodexManagedTerminalApp = "iTerm.app/3.6.8"
	defaultCodexManagedOS          = "Mac OS 15.7.4"
	defaultCodexManagedArch        = "arm64"
	defaultCodexManagedChromium    = "144"
	codexClientProfileCacheTTL     = 7 * 24 * time.Hour
	codexClientProfileCleanupEvery = time.Hour

	// fork(anticorr A-2): codex 版本 sanity ceiling（仿 claude claudeStaticSanityCeiling）。
	//
	// 背景：extractCodexClientProfile 旧逻辑只校验 version.valid，对版本上限不设防。
	// 持有合法下游 key 的人伪造 "Codex Desktop/999.999.99999" 这类荒谬高版本，会被
	// 当成合法观测，污染 per-account + 全局 high-water（bumpCodexVersionMarkers 的
	// only-up 单调递增），并被当成出站版本应用到其它账号。这里加一个静态上限把超界
	// 观测在录入 high-water 前直接拒掉。
	//
	// codex 有两个版本家族，第一段不可线性比较，必须分家族设上限：
	//   - Desktop：真实 "Codex Desktop/26.318.11754"（year.day-of-year.build），
	//     首段约 26。ceiling 取 28（领先真实 year-major 约两年），允许正常年度增长
	//     但挡住荒谬值。
	//   - CLI（codex_cli_rs / codex-tui / codex_vscode / codex_exec）：真实约
	//     "0.140.0"，主版本仍是 0。ceiling 取 "1.0.0"（领先一个主版本，仿 claude
	//     "领先 live family 一个 major" 的余量），允许 0.x 任意小幅增长但挡住 999.x。
	//
	// 何时上调：若 codex 客户端某家族合法跨过这里的常量，按对应家族上调（并保持领先
	// live family 一档），避免误拒真实新客户端。这是保守离线兜底，不是精确版本锁。
	// 线上 latest 抬 ceiling 见 codexObservationSanityCeiling（接 codex 已有的
	// resolveManagedHeaderOnlineVersion("codex")，仅用于抬上限，从不用于推高出站版本）。
	codexDesktopSanityCeilingVersion = "28.0.0"
	codexCLISanityCeilingVersion     = "1.0.0"
)

var (
	codexUserAgentVersionPattern = regexp.MustCompile(`^([A-Za-z0-9_.: -]+)/([^\s]+)`)
	codexNumericVersionPattern   = regexp.MustCompile(`\d+`)
	codexTailVersionPattern      = regexp.MustCompile(`\(([^;()]+);\s*([^)]+)\)`)

	codexClientProfileCache            = make(map[string]codexClientProfileCacheEntry)
	codexClientProfileCacheMu          sync.RWMutex
	codexClientProfileCacheCleanupOnce sync.Once
)

type codexVersionMarker struct {
	raw   string
	parts []int
	valid bool
}

type CodexClientProfile struct {
	UserAgent        string
	Version          string
	Originator       string
	BetaFeatures     string
	UserAgentProduct string
	UserAgentVersion string
	PlatformToken    string
	TailToken        string
	ChromiumVersion  string
	SecCHUA          string
	SecCHUAMobile    string
	SecCHUAPlatform  string
	AcceptEncoding   string
	AcceptLanguage   string
	SecFetchSite     string
	SecFetchMode     string
	SecFetchDest     string
	Source           ManagedHeaderProfileSource
	version          codexVersionMarker
}

type codexClientProfileCacheEntry struct {
	profile CodexClientProfile
	expire  time.Time
}

func DefaultCodexManagedOriginator() string {
	return defaultCodexManagedOriginator
}

func DefaultCodexManagedVersion() string {
	return defaultCodexManagedVersion
}

// defaultCodexManagedTerminalTail 构造 CLI 家族默认 UA 的尾段
// （terminal + "(originator; version)"），例如
// "iTerm.app/3.6.8 (codex_cli_rs; 0.140.0)"。originator/version 跟随当前画像，
// 后续 high-water 抬版本时由 bumpCodexTailVersionMarker 同步刷新尾段内的版本。
func defaultCodexManagedTerminalTail(originator string, version string) string {
	terminal := strings.TrimSpace(defaultCodexManagedTerminalApp)
	originator = strings.TrimSpace(firstNonEmptyString(originator, defaultCodexManagedOriginator))
	version = strings.TrimSpace(firstNonEmptyString(version, defaultCodexManagedVersion))
	if terminal == "" {
		return ""
	}
	return terminal + " (" + originator + "; " + version + ")"
}

func DefaultCodexManagedUserAgent() string {
	return buildCodexUserAgent(
		defaultCodexManagedOriginator,
		defaultCodexManagedVersion,
		defaultCodexManagedPlatform,
		defaultCodexManagedTerminalTail(defaultCodexManagedOriginator, defaultCodexManagedVersion),
	)
}

// ResetCodexClientProfileCacheForTests 清空 per-account 画像缓存，供跨包测试隔离。
// 仅用于测试场景下保证 high-water / 默认画像断言不被上一个用例的缓存污染。
func ResetCodexClientProfileCacheForTests() {
	codexClientProfileCacheMu.Lock()
	codexClientProfileCache = make(map[string]codexClientProfileCacheEntry)
	codexClientProfileCacheMu.Unlock()
}

func ResolveCodexClientProfile(auth *cliproxyauth.Auth, headers http.Header, cfg *config.Config) CodexClientProfile {
	codexClientProfileCacheCleanupOnce.Do(startCodexClientProfileCacheCleanup)

	now := time.Now()
	defaultProfile := defaultCodexClientProfile(cfg)
	if defaultProfile.isCodexDesktopLike() && !codexHeaderDefaultsUserAgentOverridden(cfg) {
		return defaultProfile
	}
	// fork(anticorr Wave10-D)：operator 通过 config 配了一个「非 codex 三段式 / 非 first-party」
	// 的自定义 UA（例如自研代理标识）时，该 UA 是权威原样值，不参与 CLI 家族强制/平台 pin
	// /high-water 改写。只有这种「不可解析成 codex UA」的覆盖才走短路；config 配的是正常
	// codex UA（含默认 CLI UA）时仍走完整 resolve，保留 per-account high-water。
	if codexHeaderDefaultsUserAgentOverridden(cfg) && !codexHeaderDefaultsUserAgentIsCodexLike(cfg) {
		if _, hasObserved := extractCodexClientProfile(headers, cfg); !hasObserved {
			return defaultProfile
		}
	}
	persistedProfile, hasPersisted := codexClientProfileFromAuth(auth, cfg)

	current := defaultProfile
	if hasPersisted {
		// fork(anticorr Wave10-D 要点2)：persisted bundle 可能仍是历史 "Codex Desktop"
		// 受管 bundle（测试端账号 metadata.headers / managed_header_state 残留）。当代码
		// 默认策略已是 CLI 家族时，必须把 persisted 的 Desktop 身份压回 CLI，避免
		// Desktop Originator / sec-ch-ua 经 baseline 优先（normalizeCodexClientProfile:447）
		// 漏回出站。只保留 high-water 版本，不保留 Desktop 身份。
		current = enforceCodexManagedFamily(normalizeCodexClientProfile(persistedProfile, defaultProfile), defaultProfile)
	}

	cacheKey := codexClientProfileCacheKey(auth)
	codexClientProfileCacheMu.RLock()
	entry, hasCached := codexClientProfileCache[cacheKey]
	cachedValid := hasCached && entry.expire.After(now) && strings.TrimSpace(entry.profile.UserAgent) != ""
	codexClientProfileCacheMu.RUnlock()
	if cachedValid {
		current = enforceCodexManagedFamily(normalizeCodexClientProfile(entry.profile, current), defaultProfile)
	}

	candidate, hasCandidate := extractCodexClientProfile(headers, cfg)
	if hasCandidate {
		var next CodexClientProfile
		if hasPersisted || cachedValid {
			next = bumpCodexVersionMarkers(candidate, current)
		} else {
			next = normalizeCodexClientProfile(candidate, current)
		}
		// fork(anticorr Wave10-D 要点2)：客户端可能上报 Originator=Codex Desktop（仍是
		// first-party 白名单值）。CLI 策略下出站身份钉死为 CLI 家族，只采纳观测版本，
		// 不让下游把出站 Originator/sec-ch-ua 切回 Desktop，同时 OS/arch/terminal 稳定 pin。
		next = enforceCodexManagedFamily(next, defaultProfile)
		codexClientProfileCacheMu.Lock()
		codexClientProfileCache[cacheKey] = codexClientProfileCacheEntry{
			profile: next,
			expire:  now.Add(codexClientProfileCacheTTL),
		}
		codexClientProfileCacheMu.Unlock()
		return next
	}

	if cachedValid {
		codexClientProfileCacheMu.Lock()
		entry = codexClientProfileCache[cacheKey]
		if entry.expire.After(now) && strings.TrimSpace(entry.profile.UserAgent) != "" {
			entry.profile = normalizeCodexClientProfile(entry.profile, current)
			entry.expire = now.Add(codexClientProfileCacheTTL)
			codexClientProfileCache[cacheKey] = entry
			current = entry.profile
		}
		codexClientProfileCacheMu.Unlock()
	}

	// 只有非 bundle 的 online latest（npm CLI 同家族版本）才能抬高 CLI 出站版本；codex-proxy
	// Desktop bundle 的版本与 CLI 0.x 不可线性比较，绝不让其污染 CLI 家族版本（要点1/2）。
	if online, ok := resolveManagedHeaderOnlineVersion("codex", cfg); ok && online.CodexProxyBundle == nil && !current.isCodexDesktopLike() {
		candidate := current
		candidate.Version = online.Version
		candidate.UserAgentVersion = online.Version
		candidate.Source = online.ManagedHeaderProfileSource
		candidate.UserAgent = buildCodexUserAgent(
			candidate.UserAgentProduct,
			candidate.UserAgentVersion,
			candidate.PlatformToken,
			candidate.TailToken,
		)
		candidate.version = parseCodexVersion(candidate.Version)
		current = bumpCodexVersionMarkers(candidate, current)
	}

	return enforceCodexManagedFamily(normalizeCodexClientProfile(current, defaultProfile), defaultProfile)
}

// enforceCodexManagedFamily 把 profile 的出站身份钉死到 baseline 受管家族（Wave10-D
// 起 baseline 默认是 codex_cli_rs CLI 家族），做两件事：
//
//  1. 家族强制（要点2）：当 baseline 是 CLI 家族而 profile 仍是残留的 "Codex Desktop"
//     身份时，重写 Originator / UA product 为 CLI baseline，并清掉 Desktop 专属的
//     sec-ch-ua / sec-fetch-* / Accept-* 字段（CLI 不发这些），只保留更高的 high-water
//     版本。这样 persisted/cached/observed 的 Desktop bundle 不会把 Desktop Originator
//     或 sec-ch-ua 漏回 CLI 出站。
//  2. OS/arch/terminal 稳定 pin（仿 claude pinClaudeDeviceProfilePlatform）：CLI baseline
//     下无条件把 PlatformToken（Mac OS/arch）与 terminal 尾段钉到 baseline，不透传客户端
//     上报的真实环境（如 Ghostty / Mac OS 15.6.0），每账号稳定一致、可 config 覆盖。
//     版本部分跟随 high-water 在尾段刷新。
//
// 若 baseline 自身仍是 Desktop（例如 operator 用 config 把 UA 配回 Desktop），不改动，
// 保持向后兼容。
func enforceCodexManagedFamily(profile CodexClientProfile, baseline CodexClientProfile) CodexClientProfile {
	if baseline.isCodexDesktopLike() {
		// baseline 仍是 Desktop（兼容旧 config）：完全不强制。
		return profile
	}

	// CLI baseline 下：出站身份（Originator / UA product / platform / terminal）一律钉死
	// 到 baseline 受管画像；只采纳观测到的更高版本（high-water）。这统一覆盖三种残留：
	//   - persisted/cached/observed 的 Codex Desktop bundle；
	//   - 其它 first-party CLI 终端（如 codex-tui / codex_exec）——也回退到 baseline
	//     codex_cli_rs，不把观测终端身份暴露给上游；
	//   - 同家族但平台/终端不同（如 Ghostty / Mac OS 15.6.0）——稳定 pin 到 baseline。
	// 并显式清掉 Desktop 专属指纹字段（CLI 不发 sec-ch-ua / sec-fetch-* / Accept-*）。
	//
	// 版本取 max(观测, baseline floor)，但必须先做「家族 + ceiling 门」过滤（要点1/2）：
	//
	// 背景 bug（Wave10-D 残留）：账号 persisted/cached 的 Desktop bundle 版本是
	// year.day.build（如 26.318.11753），与 CLI 家族 0.x 不可线性比较。旧逻辑用纯数值
	// max(观测, baseline)：26.318.11753 因首段 26>0 被判为「更高」而保留，于是出站
	// Version=26.318.11753、UA=codex_cli_rs/26.318.11753——CLI 身份配 Desktop 版本号，
	// 自相矛盾，反成新破绽。
	//
	// 修法：CLI baseline 下，incoming（profile）版本只有「同属 CLI 家族且不超 CLI ceiling
	// (1.0.0)」才允许参与 high-water 抬升；否则视为 Desktop 残留或伪造超界值，整体丢弃，
	// 回落 baseline（floor 0.140.0，或 config/online 抬升后的 CLI 高水位）。
	//   - profile 仍是 Desktop 家族（isCodexDesktopLike）→ 丢弃（year.day.build 不可比）。
	//   - profile 版本超过 CLI ceiling（codexCLISanityCeilingVersion=1.0.0）→ 丢弃
	//     （Desktop 26.x 自然超界；伪造的 CLI 2.x 也在此被拒回 floor）。
	// 复用既有家族判定 isCodexDesktopLike 与 ceiling 常量 codexCLISanityCeilingVersion，
	// 不另造一套。
	cliCeiling := parseCodexVersion(codexCLISanityCeilingVersion)
	incomingVersion := parseCodexVersion(strings.TrimSpace(profile.Version))
	incomingUsable := strings.TrimSpace(profile.Version) != "" &&
		incomingVersion.valid &&
		!profile.isCodexDesktopLike() &&
		incomingVersion.Compare(cliCeiling) <= 0

	version := strings.TrimSpace(baseline.Version)
	if incomingUsable {
		// incoming 是合法 CLI 家族版本：走 only-up high-water，max(观测, baseline floor)。
		version = firstNonEmptyString(strings.TrimSpace(profile.Version), version)
		observedVersion := parseCodexVersion(version)
		if baseline.version.valid && (!observedVersion.valid || observedVersion.Compare(baseline.version) < 0) {
			version = firstNonEmptyString(strings.TrimSpace(baseline.Version), version)
		}
	}
	// incoming 不可用（Desktop 残留 / 超 ceiling）时 version 已回落到 baseline，不污染 CLI。
	if strings.TrimSpace(version) == "" {
		version = firstNonEmptyString(strings.TrimSpace(profile.Version), strings.TrimSpace(baseline.Version))
	}

	coerced := baseline
	coerced.Version = version
	coerced.UserAgentVersion = version
	coerced.TailToken = bumpCodexTailVersionMarker(baseline.TailToken, coerced.UserAgentProduct, version)
	coerced.ChromiumVersion = ""
	coerced.SecCHUA = ""
	coerced.SecCHUAMobile = ""
	coerced.SecCHUAPlatform = ""
	coerced.AcceptEncoding = ""
	coerced.AcceptLanguage = ""
	coerced.SecFetchSite = ""
	coerced.SecFetchMode = ""
	coerced.SecFetchDest = ""
	coerced.UserAgent = buildCodexUserAgent(
		coerced.UserAgentProduct,
		coerced.UserAgentVersion,
		coerced.PlatformToken,
		coerced.TailToken,
	)
	coerced.version = parseCodexVersion(version)
	// BetaFeatures 是请求能力声明（非身份指纹），保留 profile 观测值。
	if strings.TrimSpace(profile.BetaFeatures) != "" {
		coerced.BetaFeatures = profile.BetaFeatures
	}
	return coerced
}

func CodexManagedHeaders(profile CodexClientProfile) map[string]string {
	headers := map[string]string{
		"User-Agent": profile.UserAgent,
		"Originator": profile.Originator,
	}
	if strings.TrimSpace(profile.Version) != "" {
		headers["Version"] = strings.TrimSpace(profile.Version)
	}
	if strings.TrimSpace(profile.BetaFeatures) != "" {
		headers["X-Codex-Beta-Features"] = strings.TrimSpace(profile.BetaFeatures)
	}
	if strings.TrimSpace(profile.SecCHUA) != "" {
		headers["sec-ch-ua"] = strings.TrimSpace(profile.SecCHUA)
	}
	if strings.TrimSpace(profile.SecCHUAMobile) != "" {
		headers["sec-ch-ua-mobile"] = strings.TrimSpace(profile.SecCHUAMobile)
	}
	if strings.TrimSpace(profile.SecCHUAPlatform) != "" {
		headers["sec-ch-ua-platform"] = strings.TrimSpace(profile.SecCHUAPlatform)
	}
	if strings.TrimSpace(profile.AcceptEncoding) != "" {
		headers["Accept-Encoding"] = strings.TrimSpace(profile.AcceptEncoding)
	}
	if strings.TrimSpace(profile.AcceptLanguage) != "" {
		headers["Accept-Language"] = strings.TrimSpace(profile.AcceptLanguage)
	}
	if strings.TrimSpace(profile.SecFetchSite) != "" {
		headers["sec-fetch-site"] = strings.TrimSpace(profile.SecFetchSite)
	}
	if strings.TrimSpace(profile.SecFetchMode) != "" {
		headers["sec-fetch-mode"] = strings.TrimSpace(profile.SecFetchMode)
	}
	if strings.TrimSpace(profile.SecFetchDest) != "" {
		headers["sec-fetch-dest"] = strings.TrimSpace(profile.SecFetchDest)
	}
	return normalizeHeaderMap(headers)
}

func CodexManagedVersionedCapabilities(profile CodexClientProfile) map[string]string {
	return normalizeHeaderMap(map[string]string{
		"User-Agent":            profile.UserAgent,
		"Version":               profile.Version,
		"X-Codex-Beta-Features": profile.BetaFeatures,
	})
}

func CodexManagedStableIdentity(profile CodexClientProfile) map[string]string {
	return normalizeHeaderMap(map[string]string{
		"Originator":         profile.Originator,
		"sec-ch-ua":          profile.SecCHUA,
		"sec-ch-ua-mobile":   profile.SecCHUAMobile,
		"sec-ch-ua-platform": profile.SecCHUAPlatform,
		"sec-fetch-site":     profile.SecFetchSite,
		"sec-fetch-mode":     profile.SecFetchMode,
		"sec-fetch-dest":     profile.SecFetchDest,
	})
}

func CodexManagedRuntimeFingerprint(profile CodexClientProfile) map[string]string {
	return normalizeHeaderMap(map[string]string{
		"platform": profile.PlatformToken,
		"terminal": profile.TailToken,
	})
}

func codexClientProfileFromAuth(auth *cliproxyauth.Auth, cfg *config.Config) (CodexClientProfile, bool) {
	if auth == nil {
		return CodexClientProfile{}, false
	}

	defaultProfile := defaultCodexClientProfile(cfg)
	profile := CodexClientProfile{
		BetaFeatures: cfgCodexBetaFeatures(cfg),
	}
	headers := normalizeHeaderMap(cliproxyauth.ExtractCustomHeadersFromMetadata(auth.Metadata))
	if len(headers) == 0 && auth.Attributes != nil && !cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		headers = normalizeHeaderMap(map[string]string{
			"User-Agent":            auth.Attributes["header:User-Agent"],
			"Version":               auth.Attributes["header:Version"],
			"Originator":            auth.Attributes["header:Originator"],
			"X-Codex-Beta-Features": auth.Attributes["header:X-Codex-Beta-Features"],
		})
	}
	if len(headers) == 0 {
		return defaultProfile, false
	}
	if userAgent := strings.TrimSpace(headers["User-Agent"]); userAgent != "" {
		profile.UserAgent = userAgent
	}
	if version := strings.TrimSpace(headers["Version"]); version != "" {
		profile.Version = version
	}
	if originator := strings.TrimSpace(headers["Originator"]); originator != "" {
		profile.Originator = originator
	}
	if betaFeatures := strings.TrimSpace(headers["X-Codex-Beta-Features"]); betaFeatures != "" {
		profile.BetaFeatures = betaFeatures
	}
	return normalizeCodexClientProfile(profile, defaultProfile), true
}

func defaultCodexClientProfile(cfg *config.Config) CodexClientProfile {
	// fork(anticorr Wave10-D)：CLI 画像默认值，允许 config 的 originator/os/arch/terminal
	// 字段覆盖（不透传真实环境，每账号稳定一致）。未直接配置完整 UserAgent 时，用这些
	// pin 字段构造稳定 CLI UA。
	originator := defaultCodexManagedOriginator
	osToken := defaultCodexManagedOS
	arch := defaultCodexManagedArch
	terminal := defaultCodexManagedTerminalApp
	betaFeatures := ""
	if cfg != nil {
		if v := strings.TrimSpace(cfg.CodexHeaderDefaults.Originator); v != "" && isFirstPartyCodexOriginator(v) {
			originator = v
		}
		if v := strings.TrimSpace(cfg.CodexHeaderDefaults.OS); v != "" {
			osToken = v
		}
		if v := strings.TrimSpace(cfg.CodexHeaderDefaults.Arch); v != "" {
			arch = v
		}
		if v := strings.TrimSpace(cfg.CodexHeaderDefaults.Terminal); v != "" {
			terminal = v
		}
		betaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
	}
	platform := strings.TrimSpace(osToken + "; " + arch)
	tail := ""
	if strings.TrimSpace(terminal) != "" {
		tail = strings.TrimSpace(terminal) + " (" + originator + "; " + defaultCodexManagedVersion + ")"
	}
	userAgent := buildCodexUserAgent(originator, defaultCodexManagedVersion, platform, tail)
	if cfg != nil {
		if trimmed := strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent); trimmed != "" {
			// 完整 UserAgent 覆盖优先级最高（兼容旧 config，也允许配回 Desktop）。
			userAgent = trimmed
		}
	}
	// fork(anticorr Wave10-D)：默认家族切到 CLI 后，default profile 用专门的 CLI 静态
	// 来源标记，而不是 community codex-proxy（那是 Desktop bundle 来源）。
	defaultSource := codexCLIManagedHeaderProfileSource()
	if codexHeaderDefaultsUserAgentOverridden(cfg) && isCodexDesktopUserAgent(userAgent) {
		// operator 用 config 显式配回 Desktop UA：沿用 community 来源，保持旧行为。
		defaultSource = codexProxyManagedHeaderProfileSource()
	}
	profile := CodexClientProfile{
		UserAgent:    userAgent,
		BetaFeatures: betaFeatures,
		Originator:   codexOriginatorForUserAgent(userAgent, cfg),
		Source:       defaultSource,
	}
	profile = normalizeCodexClientProfile(profile, CodexClientProfile{})
	if online, ok := resolveManagedHeaderOnlineVersion("codex", cfg); ok {
		// fork(anticorr Wave10-D)：online codex-proxy bundle 是 Desktop 身份（Originator/
		// sec-ch-ua）。仅当 default 仍是 Desktop 家族时才整体采纳该 bundle；CLI 家族下
		// 不让 Desktop bundle 把身份切回 Desktop，CLI 的版本高水位走下面非 bundle 分支。
		if online.CodexProxyBundle != nil && profile.isCodexDesktopLike() {
			candidate := codexProfileFromProxyBundle(profile, *online.CodexProxyBundle, online.ManagedHeaderProfileSource)
			if candidate.version.valid && (!profile.version.valid || candidate.version.Compare(profile.version) >= 0) {
				profile = candidate
			}
		} else if online.CodexProxyBundle == nil && !profile.isCodexDesktopLike() {
			// 只有非 bundle 的 online latest（npm CLI 版本，与 CLI 同家族）才能抬高 CLI 出站
			// 版本；codex-proxy Desktop bundle 的版本（year.day.build）与 CLI 0.x 不可线性比较，
			// 绝不让它污染 CLI 家族版本。
			candidate := profile
			candidate.Version = online.Version
			candidate.UserAgentVersion = online.Version
			candidate.Source = online.ManagedHeaderProfileSource
			candidate.UserAgent = buildCodexUserAgent(
				candidate.UserAgentProduct,
				candidate.UserAgentVersion,
				candidate.PlatformToken,
				candidate.TailToken,
			)
			candidate.version = parseCodexVersion(candidate.Version)
			if candidate.version.valid && (!profile.version.valid || candidate.version.Compare(profile.version) > 0) {
				profile = candidate
			}
		}
	}
	return profile
}

func codexProfileFromProxyBundle(current CodexClientProfile, bundle CodexProxyManagedHeaderBundle, source ManagedHeaderProfileSource) CodexClientProfile {
	profile := current
	originator := firstNonEmptyString(bundle.Originator, profile.Originator, defaultCodexManagedOriginator)
	version := firstNonEmptyString(bundle.AppVersion, profile.Version, defaultCodexManagedVersion)
	platform := codexProxyPlatformToken(bundle, profile.PlatformToken)
	chromium := firstNonEmptyString(bundle.ChromiumVersion, profile.ChromiumVersion, defaultCodexManagedChromium)

	profile.Originator = originator
	profile.UserAgentProduct = originator
	profile.Version = version
	profile.UserAgentVersion = version
	profile.PlatformToken = platform
	profile.TailToken = ""
	profile.ChromiumVersion = chromium
	profile.SecCHUA = firstNonEmptyString(bundle.DefaultHeaders["sec-ch-ua"], buildCodexSecCHUA(chromium))
	profile.SecCHUAMobile = firstNonEmptyString(bundle.DefaultHeaders["sec-ch-ua-mobile"], "?0")
	profile.SecCHUAPlatform = firstNonEmptyString(bundle.DefaultHeaders["sec-ch-ua-platform"], `"macOS"`)
	profile.AcceptEncoding = firstNonEmptyString(bundle.DefaultHeaders["Accept-Encoding"], "gzip, deflate, br, zstd")
	profile.AcceptLanguage = firstNonEmptyString(bundle.DefaultHeaders["Accept-Language"], "en-US,en;q=0.9")
	profile.SecFetchSite = firstNonEmptyString(bundle.DefaultHeaders["sec-fetch-site"], "same-origin")
	profile.SecFetchMode = firstNonEmptyString(bundle.DefaultHeaders["sec-fetch-mode"], "cors")
	profile.SecFetchDest = firstNonEmptyString(bundle.DefaultHeaders["sec-fetch-dest"], "empty")
	profile.Source = withManagedHeaderProfileSource(source, codexProxyManagedHeaderProfileSource())
	profile.UserAgent = buildCodexProxyUserAgent(bundle, originator, version, platform)
	profile.version = parseCodexVersion(version)
	return normalizeCodexClientProfile(profile, current)
}

func codexProxyPlatformToken(bundle CodexProxyManagedHeaderBundle, fallback string) string {
	platform := strings.TrimSpace(bundle.Platform)
	arch := strings.TrimSpace(bundle.Arch)
	switch {
	case platform != "" && arch != "":
		return platform + "; " + arch
	case platform != "":
		return platform
	case fallback != "":
		return fallback
	default:
		return defaultCodexManagedPlatform
	}
}

func buildCodexProxyUserAgent(bundle CodexProxyManagedHeaderBundle, originator string, version string, platform string) string {
	template := strings.TrimSpace(bundle.UserAgentTemplate)
	if template == "" {
		return buildCodexUserAgent(originator, version, platform, "")
	}
	replacements := map[string]string{
		"{originator}":         originator,
		"{app_version}":        version,
		"{version}":            version,
		"{platform}":           platform,
		"{arch}":               strings.TrimSpace(bundle.Arch),
		"{chromium_version}":   strings.TrimSpace(bundle.ChromiumVersion),
		"{{originator}}":       originator,
		"{{app_version}}":      version,
		"{{version}}":          version,
		"{{platform}}":         platform,
		"{{arch}}":             strings.TrimSpace(bundle.Arch),
		"{{chromium_version}}": strings.TrimSpace(bundle.ChromiumVersion),
	}
	userAgent := template
	for key, value := range replacements {
		userAgent = strings.ReplaceAll(userAgent, key, value)
	}
	if strings.Contains(userAgent, "{") || !strings.Contains(userAgent, "/") {
		return buildCodexUserAgent(originator, version, platform, "")
	}
	return userAgent
}

func normalizeCodexClientProfile(profile CodexClientProfile, baseline CodexClientProfile) CodexClientProfile {
	originalUserAgent := strings.TrimSpace(profile.UserAgent)
	if strings.TrimSpace(profile.UserAgent) == "" {
		profile.UserAgent = baseline.UserAgent
	}
	if product, version, platform, tail, ok := parseCodexUserAgent(profile.UserAgent); ok {
		if strings.TrimSpace(profile.UserAgentProduct) == "" {
			profile.UserAgentProduct = product
		}
		if strings.TrimSpace(profile.UserAgentVersion) == "" {
			profile.UserAgentVersion = version
		}
		if strings.TrimSpace(profile.PlatformToken) == "" {
			profile.PlatformToken = platform
		}
		if strings.TrimSpace(profile.TailToken) == "" {
			profile.TailToken = tail
		}
	}
	if strings.TrimSpace(profile.Originator) == "" {
		profile.Originator = firstNonEmptyString(baseline.Originator, profile.UserAgentProduct, defaultCodexManagedOriginator)
	}
	if strings.TrimSpace(profile.UserAgentProduct) == "" {
		profile.UserAgentProduct = firstNonEmptyString(baseline.UserAgentProduct, profile.Originator, defaultCodexManagedOriginator)
	}
	if strings.TrimSpace(profile.UserAgentVersion) == "" {
		profile.UserAgentVersion = firstNonEmptyString(profile.Version, baseline.UserAgentVersion, DefaultCodexManagedVersion())
	}
	if strings.TrimSpace(profile.Version) == "" {
		profile.Version = firstNonEmptyString(profile.UserAgentVersion, baseline.Version, DefaultCodexManagedVersion())
	}
	if strings.TrimSpace(profile.PlatformToken) == "" && originalUserAgent == "" {
		profile.PlatformToken = firstNonEmptyString(baseline.PlatformToken, defaultCodexManagedPlatform)
	}
	if strings.TrimSpace(profile.TailToken) == "" && originalUserAgent == "" {
		profile.TailToken = firstNonEmptyString(baseline.TailToken, defaultCodexManagedTerminalTail(profile.Originator, profile.Version))
	}
	profile = alignCodexFirstPartyIdentity(profile)
	if profile.isCodexDesktopLike() {
		if strings.TrimSpace(profile.ChromiumVersion) == "" {
			profile.ChromiumVersion = firstNonEmptyString(baseline.ChromiumVersion, defaultCodexManagedChromium)
		}
		if strings.TrimSpace(profile.SecCHUA) == "" {
			profile.SecCHUA = firstNonEmptyString(baseline.SecCHUA, buildCodexSecCHUA(profile.ChromiumVersion))
		}
		if strings.TrimSpace(profile.SecCHUAMobile) == "" {
			profile.SecCHUAMobile = firstNonEmptyString(baseline.SecCHUAMobile, "?0")
		}
		if strings.TrimSpace(profile.SecCHUAPlatform) == "" {
			profile.SecCHUAPlatform = firstNonEmptyString(baseline.SecCHUAPlatform, `"macOS"`)
		}
		if strings.TrimSpace(profile.AcceptEncoding) == "" {
			profile.AcceptEncoding = firstNonEmptyString(baseline.AcceptEncoding, "gzip, deflate, br, zstd")
		}
		if strings.TrimSpace(profile.AcceptLanguage) == "" {
			profile.AcceptLanguage = firstNonEmptyString(baseline.AcceptLanguage, "en-US,en;q=0.9")
		}
		if strings.TrimSpace(profile.SecFetchSite) == "" {
			profile.SecFetchSite = firstNonEmptyString(baseline.SecFetchSite, "same-origin")
		}
		if strings.TrimSpace(profile.SecFetchMode) == "" {
			profile.SecFetchMode = firstNonEmptyString(baseline.SecFetchMode, "cors")
		}
		if strings.TrimSpace(profile.SecFetchDest) == "" {
			profile.SecFetchDest = firstNonEmptyString(baseline.SecFetchDest, "empty")
		}
	}
	profile.TailToken = bumpCodexTailVersionMarker(profile.TailToken, profile.UserAgentProduct, profile.Version)
	if strings.TrimSpace(profile.BetaFeatures) == "" {
		profile.BetaFeatures = baseline.BetaFeatures
	}
	profile.Source = withManagedHeaderProfileSource(profile.Source, baseline.Source)

	if originalUserAgent != "" && strings.TrimSpace(profile.PlatformToken) == "" && strings.TrimSpace(profile.TailToken) == "" {
		profile.UserAgent = originalUserAgent
	} else {
		profile.UserAgent = buildCodexUserAgent(
			profile.UserAgentProduct,
			profile.UserAgentVersion,
			profile.PlatformToken,
			profile.TailToken,
		)
	}
	profile.version = parseCodexVersion(profile.Version)
	if !profile.version.valid {
		profile.version = parseCodexVersion(profile.UserAgentVersion)
	}
	return profile
}

func (profile CodexClientProfile) isCodexDesktopLike() bool {
	return strings.EqualFold(strings.TrimSpace(profile.Originator), "Codex Desktop") ||
		strings.EqualFold(strings.TrimSpace(profile.UserAgentProduct), "Codex Desktop") ||
		strings.HasPrefix(strings.TrimSpace(profile.UserAgent), "Codex Desktop/")
}

// IsCodexDesktopProfile 是 isCodexDesktopLike 的导出包装，供 executor 侧（Wave10-D
// sec-ch-ua 剥离）判断当前出站画像是否仍是 Desktop 家族。
func IsCodexDesktopProfile(profile CodexClientProfile) bool {
	return profile.isCodexDesktopLike()
}

// isCodexDesktopUserAgent 判定一个原始 UA 字符串是否是 Desktop 家族（product=Codex Desktop）。
// 用于 operator 通过 config 覆盖 UA 时回推默认家族与来源标记。
func isCodexDesktopUserAgent(userAgent string) bool {
	product, _, _, _, ok := parseCodexUserAgent(userAgent)
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(product), "Codex Desktop")
}

// codexOriginatorForUserAgent 从默认（或 config 覆盖的）UA 推导出 default profile 的
// Originator。无覆盖时用代码默认 codex_cli_rs；有覆盖且 UA product 是合法 first-party
// 值时跟随 UA product，否则回退到代码默认。
func codexOriginatorForUserAgent(userAgent string, cfg *config.Config) string {
	if !codexHeaderDefaultsUserAgentOverridden(cfg) {
		return defaultCodexManagedOriginator
	}
	product, _, _, _, ok := parseCodexUserAgent(userAgent)
	if !ok {
		return defaultCodexManagedOriginator
	}
	product = strings.TrimSpace(product)
	if isFirstPartyCodexOriginator(product) {
		return product
	}
	return defaultCodexManagedOriginator
}

func alignCodexFirstPartyIdentity(profile CodexClientProfile) CodexClientProfile {
	originator := strings.TrimSpace(profile.Originator)
	product := strings.TrimSpace(profile.UserAgentProduct)
	if !isFirstPartyCodexOriginator(originator) {
		return profile
	}
	if product == "" || (isFirstPartyCodexOriginator(product) && product != originator) {
		profile.UserAgentProduct = originator
		profile.TailToken = alignCodexTailIdentity(profile.TailToken, originator)
	}
	return profile
}

func alignCodexTailIdentity(tail string, originator string) string {
	tail = strings.TrimSpace(tail)
	originator = strings.TrimSpace(originator)
	if tail == "" || originator == "" {
		return tail
	}
	return codexTailVersionPattern.ReplaceAllStringFunc(tail, func(match string) string {
		parts := codexTailVersionPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		identity := strings.TrimSpace(parts[1])
		if !isFirstPartyCodexOriginator(identity) || identity == originator {
			return match
		}
		return "(" + originator + "; " + strings.TrimSpace(parts[2]) + ")"
	})
}

func bumpCodexVersionMarkers(candidate CodexClientProfile, current CodexClientProfile) CodexClientProfile {
	candidate = normalizeCodexClientProfile(candidate, current)
	current = normalizeCodexClientProfile(current, defaultCodexClientProfile(nil))
	if !candidate.version.valid {
		return current
	}
	// fork(anticorr 要点2)：跨家族 bump 门。current 是 CLI 家族时，绝不让 Desktop 家族
	// 版本（year.day.build，如 26.318.11753）或超 CLI ceiling(1.0.0) 的伪造版本把 CLI
	// high-water 抬高。Desktop 版本首段约 26，纯数值 Compare 会误判为「更高」并 bump，
	// 于是 CLI 出站 Version 被污染成 26.x（CLI 身份配 Desktop 版本号，自相矛盾）。
	// observed Desktop 26.x 虽能过 extract 的 Desktop ceiling(28)，但跨家族不可线性比较，
	// 在此拒绝 bump CLI 家族。复用 isCodexDesktopLike 与 codexCLISanityCeilingVersion。
	if !current.isCodexDesktopLike() {
		cliCeiling := parseCodexVersion(codexCLISanityCeilingVersion)
		if candidate.isCodexDesktopLike() || candidate.version.Compare(cliCeiling) > 0 {
			return current
		}
	}
	if current.version.valid {
		switch candidate.version.Compare(current.version) {
		case -1:
			return current
		case 0:
			next := current
			next.Source = preferredManagedHeaderProfileSource(candidate.Source, current.Source)
			return next
		}
	}

	next := current
	next.UserAgentVersion = candidate.UserAgentVersion
	next.Version = candidate.Version
	next.Source = preferredManagedHeaderProfileSource(candidate.Source, current.Source)
	next.TailToken = bumpCodexTailVersionMarker(next.TailToken, next.UserAgentProduct, next.Version)
	if strings.TrimSpace(next.BetaFeatures) == "" {
		next.BetaFeatures = candidate.BetaFeatures
	}
	next.UserAgent = buildCodexUserAgent(
		next.UserAgentProduct,
		next.UserAgentVersion,
		next.PlatformToken,
		next.TailToken,
	)
	next.version = candidate.version
	return next
}

func bumpCodexTailVersionMarker(tail string, product string, version string) string {
	tail = strings.TrimSpace(tail)
	version = strings.TrimSpace(version)
	if tail == "" || version == "" {
		return tail
	}
	return codexTailVersionPattern.ReplaceAllStringFunc(tail, func(match string) string {
		parts := codexTailVersionPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		identity := strings.TrimSpace(parts[1])
		if !isFirstPartyCodexOriginator(identity) && identity != strings.TrimSpace(product) {
			return match
		}
		return "(" + identity + "; " + version + ")"
	})
}

// codexStaticSanityCeiling 返回对应家族的硬编码离线版本上限。Desktop 与 CLI 两个
// 家族第一段不可线性比较，必须按家族取不同上限（见常量处说明）。
func codexStaticSanityCeiling(profile CodexClientProfile) codexVersionMarker {
	if profile.isCodexDesktopLike() {
		return parseCodexVersion(codexDesktopSanityCeilingVersion)
	}
	return parseCodexVersion(codexCLISanityCeilingVersion)
}

// codexObservationSanityCeiling 返回用于拒绝伪造高版本的有效上限：
// max(静态家族常量, 已缓存线上 latest)。静态常量保证离线时的确定性下界；线上 latest
// 仅在 codex online-update 已开启且 latest 已缓存时被读取，且只用于「抬高」校验上限，
// 让真正的前沿真实客户端不被误拒——从不用于推高出站版本（出站版本另有 only-up high-water
// 约束）。线上 latest 仅对同家族（与 candidate 同 Desktop/CLI 归属）才有意义；codex 线上源
// 默认就是 codex-proxy desktop bundle / codex npm，归属可能与 candidate 不同，
// 因此只有线上 latest 解析为合法版本且高于静态上限时才抬升，否则维持静态上限。
func codexObservationSanityCeiling(profile CodexClientProfile, cfg *config.Config) codexVersionMarker {
	ceiling := codexStaticSanityCeiling(profile)
	if online, ok := resolveManagedHeaderOnlineVersion("codex", cfg); ok {
		if onlineVersion := parseCodexVersion(strings.TrimSpace(online.Version)); onlineVersion.valid && onlineVersion.Compare(ceiling) > 0 {
			ceiling = onlineVersion
		}
	}
	return ceiling
}

// codexObservationWithinSanityCeiling 判定 candidate 观测版本是否在有效上限以内。
// 超界的 candidate 视为伪造，必须在录入 per-account/全局 high-water 之前拒掉，
// 既不进观测，也不会成为应用到其它账号的出站版本。
func codexObservationWithinSanityCeiling(candidate CodexClientProfile, cfg *config.Config) bool {
	if !candidate.version.valid {
		return true
	}
	return candidate.version.Compare(codexObservationSanityCeiling(candidate, cfg)) <= 0
}

func extractCodexClientProfile(headers http.Header, cfg *config.Config) (CodexClientProfile, bool) {
	if headers == nil {
		return CodexClientProfile{}, false
	}

	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	version := strings.TrimSpace(headers.Get("Version"))
	originator := strings.TrimSpace(headers.Get("Originator"))
	betaFeatures := strings.TrimSpace(headers.Get("X-Codex-Beta-Features"))

	product, userAgentVersion, platform, tail, ok := parseCodexUserAgent(userAgent)
	if !ok {
		return CodexClientProfile{}, false
	}
	if strings.TrimSpace(originator) == "" {
		originator = product
	}
	if !isFirstPartyCodexOriginator(originator) && !isFirstPartyCodexOriginator(product) {
		return CodexClientProfile{}, false
	}

	profile := CodexClientProfile{
		UserAgent:        userAgent,
		Version:          firstNonEmptyString(version, userAgentVersion),
		Originator:       originator,
		BetaFeatures:     firstNonEmptyString(betaFeatures, strings.TrimSpace(cfgCodexBetaFeatures(cfg))),
		UserAgentProduct: product,
		UserAgentVersion: userAgentVersion,
		PlatformToken:    platform,
		TailToken:        tail,
		Source:           observedManagedHeaderProfileSource(),
	}
	profile = normalizeCodexClientProfile(profile, defaultCodexClientProfile(cfg))
	if !profile.version.valid {
		return CodexClientProfile{}, false
	}
	// fork(anticorr A-2): sanity-ceiling 源级拒绝。版本超过有效上限的 candidate 视为
	// 伪造的入站观测，在任何 high-water 录入前丢弃，伪造高版本无法污染 per-account/
	// 全局观测，也无法成为应用到其它账号的出站版本。
	if !codexObservationWithinSanityCeiling(profile, cfg) {
		return CodexClientProfile{}, false
	}
	return profile, true
}

func cfgCodexBetaFeatures(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.CodexHeaderDefaults.BetaFeatures
}

func codexHeaderDefaultsUserAgentOverridden(cfg *config.Config) bool {
	return cfg != nil && strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent) != ""
}

// codexHeaderDefaultsUserAgentIsCodexLike 判定 config 覆盖的 UA 是否是「可解析的 codex
// 家族 UA」（product 落在 first-party 白名单或 Desktop）。是的话仍按受管画像走完整
// resolve（含 high-water）；否则视为 operator 想要的自定义原样 UA，走短路返回。
func codexHeaderDefaultsUserAgentIsCodexLike(cfg *config.Config) bool {
	if !codexHeaderDefaultsUserAgentOverridden(cfg) {
		return false
	}
	product, _, _, _, ok := parseCodexUserAgent(strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent))
	if !ok {
		return false
	}
	product = strings.TrimSpace(product)
	return isFirstPartyCodexOriginator(product) || strings.EqualFold(product, "Codex Desktop")
}

func parseCodexUserAgent(userAgent string) (product string, version string, platform string, tail string, ok bool) {
	trimmed := strings.TrimSpace(userAgent)
	if trimmed == "" {
		return "", "", "", "", false
	}
	match := codexUserAgentVersionPattern.FindStringSubmatch(trimmed)
	if len(match) != 3 {
		return "", "", "", "", false
	}
	product = strings.TrimSpace(match[1])
	version = strings.TrimSpace(match[2])
	remainder := strings.TrimSpace(strings.TrimPrefix(trimmed, match[0]))
	if !strings.HasPrefix(remainder, "(") {
		return product, version, "", strings.TrimSpace(remainder), true
	}
	endIdx := strings.Index(remainder, ")")
	if endIdx < 0 {
		return product, version, "", strings.TrimSpace(remainder), true
	}
	platform = strings.TrimSpace(remainder[1:endIdx])
	tail = strings.TrimSpace(remainder[endIdx+1:])
	return product, version, platform, tail, true
}

func buildCodexUserAgent(product string, version string, platform string, tail string) string {
	product = firstNonEmptyString(product, defaultCodexManagedOriginator)
	version = firstNonEmptyString(version, defaultCodexManagedVersion)
	if strings.TrimSpace(platform) == "" && strings.TrimSpace(tail) == "" {
		return strings.TrimSpace(product + "/" + version)
	}
	platform = firstNonEmptyString(platform, defaultCodexManagedPlatform)
	if strings.TrimSpace(tail) == "" {
		return strings.TrimSpace(product + "/" + version + " (" + platform + ")")
	}
	return strings.TrimSpace(product + "/" + version + " (" + platform + ") " + tail)
}

func buildCodexSecCHUA(chromiumVersion string) string {
	chromiumVersion = firstNonEmptyString(chromiumVersion, defaultCodexManagedChromium)
	return `"Chromium";v="` + chromiumVersion + `", "Not:A-Brand";v="24"`
}

// IsFirstPartyCodexOriginator 是 isFirstPartyCodexOriginator 的导出包装，供 executor
// 侧（A-1 Originator 钉死）判断客户端传入的 Originator 是否落在合法 first-party 白名单内。
func IsFirstPartyCodexOriginator(originator string) bool {
	return isFirstPartyCodexOriginator(originator)
}

func isFirstPartyCodexOriginator(originator string) bool {
	normalized := strings.TrimSpace(originator)
	if normalized == "" {
		return false
	}
	switch normalized {
	case "codex-tui", "codex_cli_rs", "codex_vscode", "codex_exec":
		return true
	default:
		return strings.HasPrefix(normalized, "Codex ")
	}
}

func parseCodexVersion(raw string) codexVersionMarker {
	version := strings.TrimSpace(raw)
	if version == "" {
		return codexVersionMarker{}
	}
	matches := codexNumericVersionPattern.FindAllString(version, -1)
	if len(matches) == 0 {
		return codexVersionMarker{raw: version}
	}
	parts := make([]int, 0, len(matches))
	for _, match := range matches {
		part := 0
		for _, digit := range match {
			part = part*10 + int(digit-'0')
		}
		parts = append(parts, part)
	}
	return codexVersionMarker{
		raw:   version,
		parts: parts,
		valid: true,
	}
}

func (v codexVersionMarker) Compare(other codexVersionMarker) int {
	if !v.valid && !other.valid {
		return 0
	}
	if v.valid && !other.valid {
		return 1
	}
	if !v.valid && other.valid {
		return -1
	}
	limit := len(v.parts)
	if len(other.parts) > limit {
		limit = len(other.parts)
	}
	for idx := 0; idx < limit; idx++ {
		left := 0
		right := 0
		if idx < len(v.parts) {
			left = v.parts[idx]
		}
		if idx < len(other.parts) {
			right = other.parts[idx]
		}
		switch {
		case left > right:
			return 1
		case left < right:
			return -1
		}
	}
	return 0
}

func codexClientProfileCacheKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return "global"
	}
	switch {
	case strings.TrimSpace(auth.ID) != "":
		return "auth:" + strings.TrimSpace(auth.ID)
	case strings.TrimSpace(auth.FileName) != "":
		return "file:" + strings.TrimSpace(auth.FileName)
	case strings.TrimSpace(auth.Label) != "":
		return "label:" + strings.TrimSpace(auth.Label)
	default:
		return "global"
	}
}

func startCodexClientProfileCacheCleanup() {
	go func() {
		ticker := time.NewTicker(codexClientProfileCleanupEvery)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			codexClientProfileCacheMu.Lock()
			for key, entry := range codexClientProfileCache {
				if !entry.expire.After(now) {
					delete(codexClientProfileCache, key)
				}
			}
			codexClientProfileCacheMu.Unlock()
		}
	}()
}

func normalizeHeaderMap(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	normalized := make(map[string]string)
	for rawKey, rawValue := range headers {
		key := strings.TrimSpace(rawKey)
		value := strings.TrimSpace(rawValue)
		if key == "" || value == "" {
			continue
		}
		normalized[key] = value
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
