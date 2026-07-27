package config

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ParseConfigBytes parses a YAML configuration payload into Config and applies the same
// in-memory normalizations as LoadConfigOptional, without persisting any changes to disk.
func ParseConfigBytes(data []byte) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config payload is empty")
	}

	var cfg Config
	// Keep defaults aligned with LoadConfigOptional.
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.LoggingDisplayTimezoneOffsetHours = DefaultLoggingDisplayTimezoneOffsetHours
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.TransientErrorCooldownSeconds = 0
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.WebsocketAuth = true
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	// fork(anticorr): restore the AmpCode localhost-restriction default that upstream's
	// defaults-block rewrite silently dropped during the merge. Default false: API key
	// auth is sufficient, so the Amp management surface is not locked to localhost.
	cfg.AmpCode.RestrictManagementToLocalhost = false
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config payload: %w", err)
	}

	cfg.CredentialConcurrency = cfg.CredentialConcurrency.WithDefaults()
	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, errValidate
	}

	// Hash remote management key if plaintext is detected (nested), but do NOT persist.
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		hashed, errHash := bcrypt.GenerateFromPassword([]byte(cfg.RemoteManagement.SecretKey), bcrypt.DefaultCost)
		if errHash != nil {
			return nil, fmt.Errorf("hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = string(hashed)
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	// 显示时区偏移仅用于日志显示/解析；超出 [-12, 14] 钳回默认 UTC+8。
	if cfg.LoggingDisplayTimezoneOffsetHours < -12 || cfg.LoggingDisplayTimezoneOffsetHours > 14 {
		cfg.LoggingDisplayTimezoneOffsetHours = DefaultLoggingDisplayTimezoneOffsetHours
	}

	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 3600")
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	// fork(anticorr): DORMANT — mirror the LoadConfigOptional neutralization here so
	// account env/cwd normalization (requirement ⑦) stays off on EVERY runtime config
	// load path, not just the on-disk file. ParseConfigBytes is the parser used for the
	// home remote config overlay (sdk/cliproxy/service.go StartConfigSubscriber ->
	// applyHomeOverlay), so without this a remotely-pushed `normalize-account-env: true`
	// could re-enable the retired cwd-normalization chain even though the file path is
	// already severed. Whatever the payload says, the effective runtime value is nil
	// (off); NormalizeAccountEnvEnabled therefore always returns false. Kept here (right
	// after unmarshal) rather than in the gate function for the same reason as
	// LoadConfigOptional: unit tests can still build a Config with the pointer set and
	// exercise the dormant normalize implementations directly.
	cfg.NormalizeAccountEnv = nil

	cfg.NormalizePluginsConfig()
	if errResolvePluginsDir := cfg.ResolvePluginsDir(); errResolvePluginsDir != nil && cfg.Plugins.Enabled {
		return nil, errResolvePluginsDir
	}

	// Apply the same sanitization pipeline.
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeInteractionsKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeXAIKeys()
	cfg.SanitizeCodexHeaderDefaults()
	cfg.SanitizeClaudeHeaderDefaults()
	// fork(anticorr): mirror LoadConfigOptional's Claude/managed-header/quota
	// snapshot/Kiro sanitizers so the home remote-config overlay path applies the
	// same normalization as the on-disk load path.
	cfg.SanitizeClaudeConfig()
	cfg.SanitizeManagedHeaderProfile()
	cfg.SanitizeQuotaSnapshotRefresh()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeKiroKeys()
	cfg.SanitizeOpenAICompatibility()
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)
	cfg.SanitizeOAuthModelAlias()
	cfg.SanitizePayloadRules()

	return &cfg, nil
}
