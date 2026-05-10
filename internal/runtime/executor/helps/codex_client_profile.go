package helps

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	defaultCodexManagedOriginator  = "Codex Desktop"
	defaultCodexManagedVersion     = "26.318.11754"
	defaultCodexManagedPlatform    = "darwin; arm64"
	defaultCodexManagedTerminal    = ""
	defaultCodexManagedChromium    = "144"
	codexClientProfileCacheTTL     = 7 * 24 * time.Hour
	codexClientProfileCleanupEvery = time.Hour
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

func DefaultCodexManagedUserAgent() string {
	return buildCodexUserAgent(
		defaultCodexManagedOriginator,
		defaultCodexManagedVersion,
		defaultCodexManagedPlatform,
		defaultCodexManagedTerminal,
	)
}

func ResolveCodexClientProfile(auth *cliproxyauth.Auth, headers http.Header, cfg *config.Config) CodexClientProfile {
	codexClientProfileCacheCleanupOnce.Do(startCodexClientProfileCacheCleanup)

	now := time.Now()
	defaultProfile := defaultCodexClientProfile(cfg)
	if defaultProfile.isCodexDesktopLike() && !codexHeaderDefaultsUserAgentOverridden(cfg) {
		return defaultProfile
	}
	persistedProfile, hasPersisted := codexClientProfileFromAuth(auth, cfg)

	current := defaultProfile
	if hasPersisted {
		current = normalizeCodexClientProfile(persistedProfile, defaultProfile)
	}

	cacheKey := codexClientProfileCacheKey(auth)
	codexClientProfileCacheMu.RLock()
	entry, hasCached := codexClientProfileCache[cacheKey]
	cachedValid := hasCached && entry.expire.After(now) && strings.TrimSpace(entry.profile.UserAgent) != ""
	codexClientProfileCacheMu.RUnlock()
	if cachedValid {
		current = normalizeCodexClientProfile(entry.profile, current)
	}

	candidate, hasCandidate := extractCodexClientProfile(headers, cfg)
	if hasCandidate {
		var next CodexClientProfile
		if hasPersisted || cachedValid {
			next = bumpCodexVersionMarkers(candidate, current)
		} else {
			next = normalizeCodexClientProfile(candidate, current)
		}
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

	if online, ok := resolveManagedHeaderOnlineVersion("codex", cfg); ok && !current.isCodexDesktopLike() {
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

	return normalizeCodexClientProfile(current, defaultProfile)
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
	userAgent := DefaultCodexManagedUserAgent()
	betaFeatures := ""
	if cfg != nil {
		if trimmed := strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent); trimmed != "" {
			userAgent = trimmed
		}
		betaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
	}
	profile := CodexClientProfile{
		UserAgent:    userAgent,
		BetaFeatures: betaFeatures,
		Originator:   defaultCodexManagedOriginator,
		Source:       codexProxyManagedHeaderProfileSource(),
	}
	profile = normalizeCodexClientProfile(profile, CodexClientProfile{})
	if online, ok := resolveManagedHeaderOnlineVersion("codex", cfg); ok {
		if online.CodexProxyBundle != nil {
			candidate := codexProfileFromProxyBundle(profile, *online.CodexProxyBundle, online.ManagedHeaderProfileSource)
			if candidate.version.valid && (!profile.version.valid || candidate.version.Compare(profile.version) >= 0) {
				profile = candidate
			}
		} else if !profile.isCodexDesktopLike() {
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
		profile.TailToken = firstNonEmptyString(baseline.TailToken, defaultCodexManagedTerminal)
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
