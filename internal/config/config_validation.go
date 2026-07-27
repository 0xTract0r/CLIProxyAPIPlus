package config

import (
	"bytes"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// SanitizePayloadRules validates raw JSON payload rule params and drops invalid rules.
func (cfg *Config) SanitizePayloadRules() {
	if cfg == nil {
		return
	}
	cfg.Payload.DefaultRaw = sanitizePayloadRawRules(cfg.Payload.DefaultRaw, "default-raw")
	cfg.Payload.OverrideRaw = sanitizePayloadRawRules(cfg.Payload.OverrideRaw, "override-raw")
}

func sanitizePayloadRawRules(rules []PayloadRule, section string) []PayloadRule {
	if len(rules) == 0 {
		return rules
	}
	out := make([]PayloadRule, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if len(rule.Params) == 0 {
			continue
		}
		invalid := false
		for path, value := range rule.Params {
			raw, ok := payloadRawString(value)
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				log.WithFields(log.Fields{
					"section":    section,
					"rule_index": i + 1,
					"param":      path,
				}).Warn("payload rule dropped: invalid raw JSON")
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func payloadRawString(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}

// looksLikeBcrypt returns true if the provided string appears to be a bcrypt hash.
func looksLikeBcrypt(s string) bool {
	return len(s) > 4 && (s[:4] == "$2a$" || s[:4] == "$2b$" || s[:4] == "$2y$")
}

// hashSecret hashes the given secret using bcrypt.
func hashSecret(secret string) (string, error) {
	// Use default cost for simplicity.
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}

// SanitizeClaudeConfig normalizes Claude-specific runtime policy.
func (cfg *Config) SanitizeClaudeConfig() {
	if cfg == nil {
		return
	}
	cfg.Claude.SonnetLongContextPolicy = NormalizeClaudeSonnetLongContextPolicy(cfg.Claude.SonnetLongContextPolicy)
}

// SanitizeManagedHeaderProfile clamps managed-header online-refresh timing knobs.
func (cfg *Config) SanitizeManagedHeaderProfile() {
	if cfg == nil {
		return
	}
	// fork(anticorr) requirement ⑥ plan A: the managed-header online-update (npm)
	// flag must materialize a non-nil default of OFF when the config file omits it,
	// so the outbound claude-cli version ceiling is the account's real observed
	// high-water mark and never npm "latest". An explicit `online-update: true`
	// survives (unmarshal sets the pointer before this runs).
	if cfg.ManagedHeaderProfile.OnlineUpdate == nil {
		onlineUpdate := false
		cfg.ManagedHeaderProfile.OnlineUpdate = &onlineUpdate
	}
	if cfg.ManagedHeaderProfile.FetchTimeoutSeconds <= 0 {
		cfg.ManagedHeaderProfile.FetchTimeoutSeconds = 2
	}
	if cfg.ManagedHeaderProfile.FetchTimeoutSeconds > 10 {
		cfg.ManagedHeaderProfile.FetchTimeoutSeconds = 10
	}
	if cfg.ManagedHeaderProfile.CacheTTLSeconds <= 0 {
		cfg.ManagedHeaderProfile.CacheTTLSeconds = 6 * 60 * 60
	}
	if cfg.ManagedHeaderProfile.CacheTTLSeconds < 60 {
		cfg.ManagedHeaderProfile.CacheTTLSeconds = 60
	}
}

// SanitizeQuotaSnapshotRefresh normalizes the persisted quota snapshot refresh
// policy, applying defaults for unset pointers and canonicalizing durations.
func (cfg *Config) SanitizeQuotaSnapshotRefresh() {
	if cfg == nil {
		return
	}
	if cfg.QuotaSnapshotRefresh.Enabled == nil {
		enabled := true
		cfg.QuotaSnapshotRefresh.Enabled = &enabled
	}
	if cfg.QuotaSnapshotRefresh.StartupCatchUp == nil {
		startupCatchUp := true
		cfg.QuotaSnapshotRefresh.StartupCatchUp = &startupCatchUp
	}
	cfg.QuotaSnapshotRefresh.Interval = normalizeQuotaSnapshotDuration(
		cfg.QuotaSnapshotRefresh.Interval,
		DefaultQuotaSnapshotRefreshInterval,
		DefaultQuotaSnapshotRefreshIntervalString,
	)
	cfg.QuotaSnapshotRefresh.Jitter = normalizeQuotaSnapshotDuration(
		cfg.QuotaSnapshotRefresh.Jitter,
		DefaultQuotaSnapshotRefreshJitter,
		DefaultQuotaSnapshotRefreshJitterString,
		true,
	)
	cfg.QuotaSnapshotRefresh.StartupMaxStaleness = normalizeQuotaSnapshotDuration(
		cfg.QuotaSnapshotRefresh.StartupMaxStaleness,
		DefaultQuotaSnapshotRefreshStartupMaxStaleness,
		DefaultQuotaSnapshotRefreshStartupMaxStalenessString,
		true,
	)
}

// SanitizeKiroKeys trims whitespace from Kiro credential fields.
func (cfg *Config) SanitizeKiroKeys() {
	if cfg == nil || len(cfg.KiroKey) == 0 {
		return
	}
	for i := range cfg.KiroKey {
		entry := &cfg.KiroKey[i]
		entry.TokenFile = strings.TrimSpace(entry.TokenFile)
		entry.AccessToken = strings.TrimSpace(entry.AccessToken)
		entry.RefreshToken = strings.TrimSpace(entry.RefreshToken)
		entry.ExpiresAt = strings.TrimSpace(entry.ExpiresAt)
		entry.Email = strings.TrimSpace(entry.Email)
		entry.ProfileArn = strings.TrimSpace(entry.ProfileArn)
		entry.Region = strings.TrimSpace(entry.Region)
		entry.AgentTaskType = strings.TrimSpace(entry.AgentTaskType)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.PreferredEndpoint = strings.TrimSpace(entry.PreferredEndpoint)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	}
}
