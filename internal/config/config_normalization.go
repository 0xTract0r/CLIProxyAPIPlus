package config

import (
	"fmt"
	"strings"
	"time"

	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
)

// NormalizePluginsConfig applies default plugin configuration values.
func (cfg *Config) NormalizePluginsConfig() {
	if cfg == nil {
		return
	}
	cfg.Plugins.Dir = strings.TrimSpace(cfg.Plugins.Dir)
	if cfg.Plugins.Dir == "" {
		cfg.Plugins.Dir = defaultPluginsDir
	}
	if len(cfg.Plugins.StoreSources) > 0 {
		sources := make([]string, 0, len(cfg.Plugins.StoreSources))
		for _, source := range cfg.Plugins.StoreSources {
			source = strings.TrimSpace(source)
			if source == "" {
				continue
			}
			sources = append(sources, source)
		}
		cfg.Plugins.StoreSources = sources
	}
	cfg.Plugins.StoreAuth = sdkpluginstore.NormalizeAuthConfigs(cfg.Plugins.StoreAuth)
	if cfg.Plugins.Configs == nil {
		cfg.Plugins.Configs = map[string]PluginInstanceConfig{}
	}
}

// SanitizeCodexHeaderDefaults trims surrounding whitespace from the
// configured Codex header fallback values.
func (cfg *Config) SanitizeCodexHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.CodexHeaderDefaults.UserAgent = strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	cfg.CodexHeaderDefaults.BetaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

// SanitizeClaudeHeaderDefaults trims surrounding whitespace from the
// configured Claude fingerprint baseline values.
func (cfg *Config) SanitizeClaudeHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.ClaudeHeaderDefaults.UserAgent = strings.TrimSpace(cfg.ClaudeHeaderDefaults.UserAgent)
	cfg.ClaudeHeaderDefaults.PackageVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.PackageVersion)
	cfg.ClaudeHeaderDefaults.RuntimeVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.RuntimeVersion)
	cfg.ClaudeHeaderDefaults.OS = strings.TrimSpace(cfg.ClaudeHeaderDefaults.OS)
	cfg.ClaudeHeaderDefaults.Arch = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Arch)
	cfg.ClaudeHeaderDefaults.Timeout = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout)
	cfg.ClaudeHeaderDefaults.FarmOS = strings.TrimSpace(cfg.ClaudeHeaderDefaults.FarmOS)
	cfg.ClaudeHeaderDefaults.FarmArch = strings.TrimSpace(cfg.ClaudeHeaderDefaults.FarmArch)
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	if cfg == nil {
		return
	}

	// fork(anticorr): default alias injection was dropped when config sanitization
	// was split out of config.go during the upstream merge. Restore it here so a
	// stock config still exposes the fork's Kiro / GitHub Copilot / Claude 1M
	// aliases. Injection is per-channel and only fires when the channel is entirely
	// absent, so a user-configured channel — or an explicit empty/nil channel used
	// as a "deleted" marker (#222) — is never re-populated.
	if cfg.OAuthModelAlias == nil {
		cfg.OAuthModelAlias = make(map[string][]OAuthModelAlias)
	}

	hasChannel := func(channel string) bool {
		for key := range cfg.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(key), channel) {
				return true
			}
		}
		return false
	}

	if !hasChannel("kiro") {
		cfg.OAuthModelAlias["kiro"] = defaultKiroAliases()
	}
	if !hasChannel("github-copilot") {
		cfg.OAuthModelAlias["github-copilot"] = defaultGitHubCopilotAliases()
	}
	if !hasChannel("claude") {
		cfg.OAuthModelAlias["claude"] = defaultClaudeAliases()
	}

	out := make(map[string][]OAuthModelAlias, len(cfg.OAuthModelAlias))
	for rawChannel, aliases := range cfg.OAuthModelAlias {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" {
			continue
		}
		// fork(anticorr): preserve explicit empty/nil channels as disabled markers
		// so defaults are not re-injected on later sanitization passes (#222).
		if len(aliases) == 0 {
			out[channel] = nil
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(clean, OAuthModelAlias{
				Name:         name,
				Alias:        alias,
				Fork:         entry.Fork,
				DisplayName:  strings.TrimSpace(entry.DisplayName),
				ForceMapping: entry.ForceMapping,
			})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	cfg.OAuthModelAlias = out
}

// SanitizeOpenAICompatibility removes OpenAI-compatibility provider entries that are
// not actionable, specifically those missing a BaseURL. It trims whitespace before
// evaluation and preserves the relative order of remaining entries.
func (cfg *Config) SanitizeOpenAICompatibility() {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return
	}
	out := make([]OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		e := cfg.OpenAICompatibility[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		if e.BaseURL == "" {
			// Skip providers with no base-url; treated as removed
			continue
		}
		out = append(out, e)
	}
	cfg.OpenAICompatibility = out
}

// SanitizeCodexKeys removes Codex API key entries missing a BaseURL.
// It trims whitespace and preserves order for remaining entries.
func (cfg *Config) SanitizeCodexKeys() {
	if cfg == nil {
		return
	}
	cfg.CodexKey = sanitizeCodexKeyEntries(cfg.CodexKey)
}

// SanitizeXAIKeys removes xAI API key entries missing a BaseURL.
// It applies the same normalization rules as codex-api-key.
func (cfg *Config) SanitizeXAIKeys() {
	if cfg == nil {
		return
	}
	cfg.XAIKey = sanitizeCodexKeyEntries(cfg.XAIKey)
}

func sanitizeCodexKeyEntries(entries []CodexKey) []CodexKey {
	if len(entries) == 0 {
		return entries
	}
	out := make([]CodexKey, 0, len(entries))
	for i := range entries {
		e := entries[i]
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SanitizeClaudeKeys normalizes headers for Claude credentials.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
	}
}

func sanitizeGeminiKeyEntries(entries []GeminiKey) []GeminiKey {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for i := range entries {
		entry := entries[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// SanitizeGeminiKeys deduplicates and normalizes Gemini credentials.
// It uses API key + base URL as the uniqueness key.
func (cfg *Config) SanitizeGeminiKeys() {
	if cfg == nil {
		return
	}
	cfg.GeminiKey = sanitizeGeminiKeyEntries(cfg.GeminiKey)
}

// SanitizeInteractionsKeys deduplicates and normalizes native Interactions credentials.
// It uses API key + base URL as the uniqueness key.
func (cfg *Config) SanitizeInteractionsKeys() {
	if cfg == nil {
		return
	}
	cfg.InteractionsKey = sanitizeGeminiKeyEntries(cfg.InteractionsKey)
}

func normalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeCodexFastModels trims, lowercases, and deduplicates the Codex fast/priority
// model allowlist. It preserves the order of first occurrences and drops empty entries.
// Reused by both the auth synthesizer (to write the fast_models attribute) and the
// runtime gate (to compare a request's model against the allowlist) so both sides agree
// on normalization.
func NormalizeCodexFastModels(models []string) []string {
	return NormalizeExcludedModels(models)
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeClaudeSonnetLongContextPolicy returns a supported policy value.
func NormalizeClaudeSonnetLongContextPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", ClaudeSonnetLongContextPolicyFailWithHint:
		return ClaudeSonnetLongContextPolicyFailWithHint
	case ClaudeSonnetLongContextPolicyRouteToOpus1M:
		return ClaudeSonnetLongContextPolicyRouteToOpus1M
	case ClaudeSonnetLongContextPolicyCompact:
		return ClaudeSonnetLongContextPolicyCompact
	default:
		return ClaudeSonnetLongContextPolicyFailWithHint
	}
}

// NormalizeSdkCliEntrypointEnabled reports whether the sdk-cli→cli
// cc_entrypoint normalization (see ClaudeConfig.NormalizeSdkCliEntrypoint) is
// active. It defaults to true (enabled) when the pointer is unset, so a stock
// config normalizes Agent SDK / `claude -p` traffic without an explicit
// opt-in. Set claude.normalize-sdk-cli-entrypoint: false to opt out and
// restore the previous "mirror inbound entrypoint verbatim" behavior.
func NormalizeSdkCliEntrypointEnabled(cfg *Config) bool {
	if cfg == nil || cfg.Claude.NormalizeSdkCliEntrypoint == nil {
		return true
	}
	return *cfg.Claude.NormalizeSdkCliEntrypoint
}

// AlignRealPathBillingVersionEnabled reports whether the REAL serving path
// (genuine claude-cli, helps.ShouldCloak == false) rewrites the body billing
// header cc_version <version> segment to the account high-water version V (see
// ClaudeConfig.AlignRealPathBillingVersion). Unlike NormalizeSdkCliEntrypoint,
// this defaults to false (disabled) when the pointer is unset, so the real
// serving path stays byte-for-byte unchanged until an operator explicitly opts
// in after real-machine validation. Set claude.align-real-path-billing-version:
// true to enable.
func AlignRealPathBillingVersionEnabled(cfg *Config) bool {
	return cfg != nil &&
		cfg.Claude.AlignRealPathBillingVersion != nil &&
		*cfg.Claude.AlignRealPathBillingVersion
}

// ManagedHeaderOnlineUpdateEnabled reports whether core may consult public
// online registries to refresh provider-managed version markers.
func ManagedHeaderOnlineUpdateEnabled(cfg *Config) bool {
	return cfg != nil &&
		cfg.ManagedHeaderProfile.OnlineUpdate != nil &&
		*cfg.ManagedHeaderProfile.OnlineUpdate
}

// ManagedHeaderProfileFetchTimeout returns the clamped online-refresh fetch
// timeout in seconds (default 2, clamped to [1, 10]).
func ManagedHeaderProfileFetchTimeout(cfg *Config) int {
	if cfg == nil || cfg.ManagedHeaderProfile.FetchTimeoutSeconds <= 0 {
		return 2
	}
	if cfg.ManagedHeaderProfile.FetchTimeoutSeconds > 10 {
		return 10
	}
	return cfg.ManagedHeaderProfile.FetchTimeoutSeconds
}

// ManagedHeaderProfileCacheTTL returns the clamped online-refresh cache TTL in
// seconds (default 6h, floored at 60s).
func ManagedHeaderProfileCacheTTL(cfg *Config) int {
	if cfg == nil || cfg.ManagedHeaderProfile.CacheTTLSeconds <= 0 {
		return 6 * 60 * 60
	}
	if cfg.ManagedHeaderProfile.CacheTTLSeconds < 60 {
		return 60
	}
	return cfg.ManagedHeaderProfile.CacheTTLSeconds
}

// QuotaSnapshotRefreshEnabled reports whether the background quota snapshot
// refresher is on (default enabled when unset).
func QuotaSnapshotRefreshEnabled(cfg *Config) bool {
	return cfg == nil ||
		cfg.QuotaSnapshotRefresh.Enabled == nil ||
		*cfg.QuotaSnapshotRefresh.Enabled
}

// QuotaSnapshotRefreshInterval returns the configured refresh interval.
func QuotaSnapshotRefreshInterval(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultQuotaSnapshotRefreshInterval
	}
	return parseQuotaSnapshotDuration(cfg.QuotaSnapshotRefresh.Interval, DefaultQuotaSnapshotRefreshInterval)
}

// QuotaSnapshotRefreshJitter returns the configured refresh jitter.
func QuotaSnapshotRefreshJitter(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultQuotaSnapshotRefreshJitter
	}
	return parseQuotaSnapshotDuration(cfg.QuotaSnapshotRefresh.Jitter, DefaultQuotaSnapshotRefreshJitter, true)
}

// QuotaSnapshotRefreshStartupCatchUp reports whether the startup catch-up pass
// runs (default enabled when unset).
func QuotaSnapshotRefreshStartupCatchUp(cfg *Config) bool {
	return cfg == nil ||
		cfg.QuotaSnapshotRefresh.StartupCatchUp == nil ||
		*cfg.QuotaSnapshotRefresh.StartupCatchUp
}

// QuotaSnapshotRefreshStartupMaxStaleness returns the max staleness window that
// still triggers a startup catch-up refresh.
func QuotaSnapshotRefreshStartupMaxStaleness(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultQuotaSnapshotRefreshStartupMaxStaleness
	}
	return parseQuotaSnapshotDuration(cfg.QuotaSnapshotRefresh.StartupMaxStaleness, DefaultQuotaSnapshotRefreshStartupMaxStaleness, true)
}

func normalizeQuotaSnapshotDuration(raw string, fallback time.Duration, fallbackString string, allowZero ...bool) string {
	duration := parseQuotaSnapshotDuration(raw, fallback, allowZero...)
	if duration < 0 || (duration == 0 && !quotaSnapshotDurationAllowsZero(allowZero...)) {
		return fallbackString
	}
	return formatQuotaSnapshotDuration(duration)
}

func parseQuotaSnapshotDuration(raw string, fallback time.Duration, allowZero ...bool) time.Duration {
	value := normalizeQuotaSnapshotDurationInput(raw)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 || (duration == 0 && !quotaSnapshotDurationAllowsZero(allowZero...)) {
		return fallback
	}
	return duration
}

func quotaSnapshotDurationAllowsZero(allowZero ...bool) bool {
	return len(allowZero) > 0 && allowZero[0]
}

func normalizeQuotaSnapshotDurationInput(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}
	replacements := []struct {
		old string
		new string
	}{
		{"minutes", "m"},
		{"minute", "m"},
		{"mins", "m"},
		{"min", "m"},
		{"hours", "h"},
		{"hour", "h"},
		{"hrs", "h"},
		{"hr", "h"},
	}
	for _, replacement := range replacements {
		if strings.HasSuffix(value, replacement.old) {
			return strings.TrimSpace(strings.TrimSuffix(value, replacement.old)) + replacement.new
		}
	}
	return value
}

func formatQuotaSnapshotDuration(duration time.Duration) string {
	if duration == 0 {
		return "0m"
	}
	switch {
	case duration%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(duration/time.Hour))
	case duration%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(duration/time.Minute))
	default:
		return duration.String()
	}
}
