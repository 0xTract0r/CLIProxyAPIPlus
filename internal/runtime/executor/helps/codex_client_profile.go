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
	defaultCodexManagedOriginator  = "codex-tui"
	defaultCodexManagedVersion     = "0.124.0"
	defaultCodexManagedPlatform    = "Mac OS 26.3.1; arm64"
	defaultCodexManagedTerminal    = "iTerm.app/3.6.9"
	codexClientProfileCacheTTL     = 7 * 24 * time.Hour
	codexClientProfileCleanupEvery = time.Hour
)

var (
	codexUserAgentVersionPattern = regexp.MustCompile(`^([A-Za-z0-9_.:-]+)/([^\s]+)`)
	codexNumericVersionPattern   = regexp.MustCompile(`\d+`)

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
		defaultCodexManagedTerminal+" ("+defaultCodexManagedOriginator+"; "+defaultCodexManagedVersion+")",
	)
}

func ResolveCodexClientProfile(auth *cliproxyauth.Auth, headers http.Header, cfg *config.Config) CodexClientProfile {
	codexClientProfileCacheCleanupOnce.Do(startCodexClientProfileCacheCleanup)

	now := time.Now()
	defaultProfile := defaultCodexClientProfile(cfg)
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
		"Originator": profile.Originator,
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

	profile := defaultCodexClientProfile(cfg)
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
		return profile, false
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
	return normalizeCodexClientProfile(profile, defaultCodexClientProfile(cfg)), true
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
	}
	return normalizeCodexClientProfile(profile, CodexClientProfile{})
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
	if strings.TrimSpace(profile.BetaFeatures) == "" {
		profile.BetaFeatures = baseline.BetaFeatures
	}

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

func bumpCodexVersionMarkers(candidate CodexClientProfile, current CodexClientProfile) CodexClientProfile {
	candidate = normalizeCodexClientProfile(candidate, current)
	current = normalizeCodexClientProfile(current, defaultCodexClientProfile(nil))
	if !candidate.version.valid || (current.version.valid && candidate.version.Compare(current.version) <= 0) {
		return current
	}

	next := current
	next.UserAgentVersion = candidate.UserAgentVersion
	next.Version = candidate.Version
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
	tail = firstNonEmptyString(tail, defaultCodexManagedTerminal)
	return strings.TrimSpace(product + "/" + version + " (" + platform + ") " + tail)
}

func isFirstPartyCodexOriginator(originator string) bool {
	normalized := strings.TrimSpace(originator)
	if normalized == "" {
		return false
	}
	switch normalized {
	case "codex-tui", "codex_cli_rs", "codex_vscode":
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
