package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	managedHeaderProfileSourceDefault    = "default"
	managedHeaderProfileSourceCodexProxy = "community:codex-proxy"
	managedHeaderProfileSourceNPM        = "online:npm"
	managedHeaderProfileSourceRequest    = "observed:first_party"

	claudeCodeNPMURL = "https://registry.npmjs.org/@anthropic-ai%2fclaude-code/latest"
	codexNPMURL      = "https://registry.npmjs.org/@openai%2fcodex/latest"

	codexProxyDefaultConfigURL     = "https://raw.githubusercontent.com/icebear0828/codex-proxy/master/config/default.yaml"
	codexProxyFingerprintConfigURL = "https://raw.githubusercontent.com/icebear0828/codex-proxy/master/config/fingerprint.yaml"
)

type ManagedHeaderProfileSource struct {
	Source       string
	SourceURL    string
	CheckedAt    string
	Completeness string
}

type ManagedHeaderOnlineVersion struct {
	Version string
	ManagedHeaderProfileSource
	CodexProxyBundle *CodexProxyManagedHeaderBundle
}

type managedHeaderOnlineVersion = ManagedHeaderOnlineVersion

type CodexProxyManagedHeaderBundle struct {
	Originator        string
	AppVersion        string
	BuildNumber       string
	Platform          string
	Arch              string
	ChromiumVersion   string
	UserAgentTemplate string
	DefaultHeaders    map[string]string
	SourceURLs        []string
}

type managedHeaderOnlineCacheEntry struct {
	result ManagedHeaderOnlineVersion
	expire time.Time
}

var (
	managedHeaderOnlineHTTPClient = http.DefaultClient
	managedHeaderOnlineNow        = time.Now
	managedHeaderOnlineCache      = make(map[string]managedHeaderOnlineCacheEntry)
	managedHeaderOnlineCacheMu    sync.Mutex

	ManagedHeaderOnlineFetchOverride func(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool)
)

func resetManagedHeaderOnlineProfileCacheForTests() {
	managedHeaderOnlineCacheMu.Lock()
	managedHeaderOnlineCache = make(map[string]managedHeaderOnlineCacheEntry)
	managedHeaderOnlineCacheMu.Unlock()
}

func defaultManagedHeaderProfileSource() ManagedHeaderProfileSource {
	return ManagedHeaderProfileSource{Source: managedHeaderProfileSourceDefault, Completeness: "static-fallback"}
}

func codexProxyManagedHeaderProfileSource() ManagedHeaderProfileSource {
	return ManagedHeaderProfileSource{
		Source:       managedHeaderProfileSourceCodexProxy,
		SourceURL:    "https://github.com/icebear0828/codex-proxy",
		Completeness: "static-coherent-bundle",
	}
}

func observedManagedHeaderProfileSource() ManagedHeaderProfileSource {
	return ManagedHeaderProfileSource{
		Source:       managedHeaderProfileSourceRequest,
		CheckedAt:    managedHeaderOnlineNow().UTC().Format(time.RFC3339),
		Completeness: "observed-complete-request",
	}
}

func resolveManagedHeaderOnlineVersion(provider string, cfg *config.Config) (ManagedHeaderOnlineVersion, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || !config.ManagedHeaderOnlineUpdateEnabled(cfg) {
		return ManagedHeaderOnlineVersion{}, false
	}
	if ManagedHeaderOnlineFetchOverride != nil {
		return ManagedHeaderOnlineFetchOverride(provider, cfg)
	}

	now := managedHeaderOnlineNow()
	managedHeaderOnlineCacheMu.Lock()
	if entry, ok := managedHeaderOnlineCache[provider]; ok && entry.expire.After(now) && strings.TrimSpace(entry.result.Version) != "" {
		managedHeaderOnlineCacheMu.Unlock()
		return entry.result, true
	}
	managedHeaderOnlineCacheMu.Unlock()

	result, ok := fetchManagedHeaderOnlineVersion(provider, cfg, now)
	if !ok {
		return managedHeaderOnlineVersion{}, false
	}

	ttl := time.Duration(config.ManagedHeaderProfileCacheTTL(cfg)) * time.Second
	managedHeaderOnlineCacheMu.Lock()
	managedHeaderOnlineCache[provider] = managedHeaderOnlineCacheEntry{
		result: result,
		expire: now.Add(ttl),
	}
	managedHeaderOnlineCacheMu.Unlock()
	return result, true
}

func fetchManagedHeaderOnlineVersion(provider string, cfg *config.Config, now time.Time) (ManagedHeaderOnlineVersion, bool) {
	if provider == "codex" {
		if result, ok := fetchCodexProxyManagedHeaderOnlineVersion(cfg, now); ok {
			return result, true
		}
	}

	sourceURL := ""
	switch provider {
	case "claude":
		sourceURL = claudeCodeNPMURL
	case "codex":
		sourceURL = codexNPMURL
	default:
		return ManagedHeaderOnlineVersion{}, false
	}

	timeout := time.Duration(config.ManagedHeaderProfileFetchTimeout(cfg)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if errReq != nil {
		return ManagedHeaderOnlineVersion{}, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CLIProxyAPIPlus-managed-header-profile/1.0")

	resp, errDo := managedHeaderOnlineHTTPClient.Do(req)
	if errDo != nil {
		return ManagedHeaderOnlineVersion{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ManagedHeaderOnlineVersion{}, false
	}

	var payload struct {
		Version string `json:"version"`
	}
	if errDecode := json.NewDecoder(resp.Body).Decode(&payload); errDecode != nil {
		return ManagedHeaderOnlineVersion{}, false
	}
	version := strings.TrimSpace(payload.Version)
	if version == "" {
		return ManagedHeaderOnlineVersion{}, false
	}
	return ManagedHeaderOnlineVersion{
		Version: version,
		ManagedHeaderProfileSource: ManagedHeaderProfileSource{
			Source:       managedHeaderProfileSourceNPM,
			SourceURL:    sourceURL,
			CheckedAt:    now.UTC().Format(time.RFC3339),
			Completeness: npmCompletenessForProvider(provider),
		},
	}, true
}

func withManagedHeaderProfileSource(source ManagedHeaderProfileSource, fallback ManagedHeaderProfileSource) ManagedHeaderProfileSource {
	if strings.TrimSpace(source.Source) != "" {
		if strings.TrimSpace(source.Completeness) == "" {
			source.Completeness = fallback.Completeness
		}
		return source
	}
	return fallback
}

func preferredManagedHeaderProfileSource(candidate ManagedHeaderProfileSource, current ManagedHeaderProfileSource) ManagedHeaderProfileSource {
	candidateSource := strings.TrimSpace(candidate.Source)
	currentSource := strings.TrimSpace(current.Source)
	if currentSource == managedHeaderProfileSourceRequest && candidateSource == managedHeaderProfileSourceNPM {
		return current
	}
	return withManagedHeaderProfileSource(candidate, current)
}

func npmCompletenessForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "partial-cli-version-only"
	case "codex":
		return "partial-cli-version-only"
	default:
		return "partial-version-only"
	}
}

func fetchCodexProxyManagedHeaderOnlineVersion(cfg *config.Config, now time.Time) (ManagedHeaderOnlineVersion, bool) {
	timeout := time.Duration(config.ManagedHeaderProfileFetchTimeout(cfg)) * time.Second
	defaultConfig, okDefault := fetchManagedHeaderOnlineText(codexProxyDefaultConfigURL, timeout)
	if !okDefault {
		return ManagedHeaderOnlineVersion{}, false
	}
	fingerprintConfig, okFingerprint := fetchManagedHeaderOnlineText(codexProxyFingerprintConfigURL, timeout)
	if !okFingerprint {
		return ManagedHeaderOnlineVersion{}, false
	}

	bundle := CodexProxyManagedHeaderBundle{
		Originator:        yamlScalar(defaultConfig, "originator"),
		AppVersion:        yamlScalar(defaultConfig, "app_version"),
		BuildNumber:       yamlScalar(defaultConfig, "build_number"),
		Platform:          yamlScalar(defaultConfig, "platform"),
		Arch:              yamlScalar(defaultConfig, "arch"),
		ChromiumVersion:   yamlScalar(defaultConfig, "chromium_version"),
		UserAgentTemplate: yamlScalar(fingerprintConfig, "user_agent_template"),
		DefaultHeaders: map[string]string{
			"sec-ch-ua":          yamlScalar(fingerprintConfig, "sec-ch-ua"),
			"sec-ch-ua-mobile":   yamlScalar(fingerprintConfig, "sec-ch-ua-mobile"),
			"sec-ch-ua-platform": yamlScalar(fingerprintConfig, "sec-ch-ua-platform"),
			"Accept-Encoding":    yamlScalar(fingerprintConfig, "Accept-Encoding"),
			"Accept-Language":    yamlScalar(fingerprintConfig, "Accept-Language"),
			"sec-fetch-site":     yamlScalar(fingerprintConfig, "sec-fetch-site"),
			"sec-fetch-mode":     yamlScalar(fingerprintConfig, "sec-fetch-mode"),
			"sec-fetch-dest":     yamlScalar(fingerprintConfig, "sec-fetch-dest"),
		},
		SourceURLs: []string{codexProxyDefaultConfigURL, codexProxyFingerprintConfigURL},
	}
	bundle.DefaultHeaders = normalizeHeaderMap(bundle.DefaultHeaders)
	if strings.TrimSpace(bundle.Originator) == "" || strings.TrimSpace(bundle.AppVersion) == "" {
		return ManagedHeaderOnlineVersion{}, false
	}
	return ManagedHeaderOnlineVersion{
		Version: strings.TrimSpace(bundle.AppVersion),
		ManagedHeaderProfileSource: ManagedHeaderProfileSource{
			Source:       managedHeaderProfileSourceCodexProxy,
			SourceURL:    codexProxyDefaultConfigURL + " " + codexProxyFingerprintConfigURL,
			CheckedAt:    now.UTC().Format(time.RFC3339),
			Completeness: "online-coherent-bundle",
		},
		CodexProxyBundle: &bundle,
	}, true
}

func fetchManagedHeaderOnlineText(sourceURL string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if errReq != nil {
		return "", false
	}
	req.Header.Set("Accept", "text/plain,*/*")
	req.Header.Set("User-Agent", "CLIProxyAPIPlus-managed-header-profile/1.0")

	resp, errDo := managedHeaderOnlineHTTPClient.Do(req)
	if errDo != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", false
	}
	return string(body), true
}

func yamlScalar(text string, key string) string {
	key = regexp.QuoteMeta(strings.TrimSpace(key))
	if key == "" {
		return ""
	}
	pattern := regexp.MustCompile(`(?m)^\s*["']?` + key + `["']?\s*:\s*(.+?)\s*$`)
	match := pattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	value = strings.Trim(value, `"'`)
	return strings.TrimSpace(value)
}
