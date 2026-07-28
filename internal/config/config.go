// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// Config represents the application's configuration, loaded from a YAML file.
type Config struct {
	SDKConfig `yaml:",inline"`
	// Host is the network host/interface on which the API server will bind.
	// Default is empty ("") to bind all interfaces (IPv4 + IPv6). Use "127.0.0.1" or "localhost" for local-only access.
	Host string `yaml:"host" json:"-"`
	// Port is the network port on which the API server will listen.
	Port int `yaml:"port" json:"-"`

	// TLS config controls HTTPS server settings.
	TLS TLSConfig `yaml:"tls" json:"tls"`

	// Home config is runtime-only and is populated from -home-jwt.
	Home HomeConfig `yaml:"-" json:"-"`

	// CredentialConcurrency contains Home-authoritative credential lifecycle settings.
	CredentialConcurrency CredentialConcurrencyConfig `yaml:"credential-concurrency" json:"credential-concurrency"`

	// CredentialInFlight configures credential observation snapshots.
	CredentialInFlight CredentialInFlightConfig `yaml:"credential-in-flight" json:"credential-in-flight"`

	// RemoteManagement nests management-related options under 'remote-management'.
	RemoteManagement RemoteManagement `yaml:"remote-management" json:"-"`

	// Plugins configures dynamic plugin discovery and per-plugin settings.
	Plugins PluginsConfig `yaml:"plugins" json:"plugins"`

	// AuthDir is the directory where authentication token files are stored.
	AuthDir string `yaml:"auth-dir" json:"-"`

	// Debug enables or disables debug-level logging and other debug features.
	Debug bool `yaml:"debug" json:"debug"`

	// Pprof config controls the optional pprof HTTP debug server.
	Pprof PprofConfig `yaml:"pprof" json:"pprof"`

	// CommercialMode disables high-overhead request logging and HTTP middleware features to minimize per-request memory usage.
	CommercialMode bool `yaml:"commercial-mode" json:"commercial-mode"`

	// LoggingToFile controls whether application logs are written to rotating files or stdout.
	LoggingToFile bool `yaml:"logging-to-file" json:"logging-to-file"`

	// LogsMaxTotalSizeMB limits the total size (in MB) of log files under the logs directory.
	// When exceeded, the oldest log files are deleted until within the limit. Set to 0 to disable.
	LogsMaxTotalSizeMB int `yaml:"logs-max-total-size-mb" json:"logs-max-total-size-mb"`

	// LogsCompressAfterDays compresses log files older than this many days using gzip.
	// Set to 0 to disable age-based compression.
	LogsCompressAfterDays int `yaml:"logs-compress-after-days" json:"logs-compress-after-days"`

	// LogsDeleteAfterDays deletes log files older than this many days.
	// Applies to both plain `.log` files and compressed `.log.gz` files. Set to 0 to disable age-based deletion.
	LogsDeleteAfterDays int `yaml:"logs-delete-after-days" json:"logs-delete-after-days"`

	// LoggingDisplayTimezoneOffsetHours 控制「人看的后端日志」时间显示时区偏移（小时）。
	// 默认 8 表示 UTC+8（东八区），方便本地运维直接读懂日志时间。它只影响日志显示
	// 与后台日志查询解析，不影响任何出站到上游的时间字段（Kiro 注入的
	// `[Context: Current time is ...]` 等指纹时间保持原口径，绝不经过本设置）。
	// 取值范围 [-12, 14]；超出范围会被钳回默认值 8。
	LoggingDisplayTimezoneOffsetHours int `yaml:"logging-display-timezone-offset-hours" json:"logging-display-timezone-offset-hours"`

	// ErrorLogsMaxFiles limits the number of error log files retained when request logging is disabled.
	// When exceeded, the oldest error log files are deleted. Default is 10. Set to 0 to disable cleanup.
	ErrorLogsMaxFiles int `yaml:"error-logs-max-files" json:"error-logs-max-files"`

	// ErrorLogAlert controls external notifications for application error logs.
	ErrorLogAlert ErrorLogAlertConfig `yaml:"error-log-alert" json:"error-log-alert"`

	// UsageStatisticsEnabled toggles in-memory usage aggregation; when false, usage data is discarded.
	UsageStatisticsEnabled bool `yaml:"usage-statistics-enabled" json:"usage-statistics-enabled"`

	// RedisUsageQueueRetentionSeconds controls how long usage queue items are retained
	// in memory for Management API consumers.
	// Default: 60. Max: 3600.
	RedisUsageQueueRetentionSeconds int `yaml:"redis-usage-queue-retention-seconds" json:"redis-usage-queue-retention-seconds"`

	// DisableCooling disables quota cooldown scheduling when true.
	DisableCooling bool `yaml:"disable-cooling" json:"disable-cooling"`

	// SaveCooldownStatus persists runtime cooldown status next to auth files when true.
	SaveCooldownStatus bool `yaml:"save-cooldown-status" json:"save-cooldown-status"`

	// TransientErrorCooldownSeconds controls cooldowns for transient upstream errors.
	// 0 keeps the legacy default cooldown. Negative values disable these cooldowns.
	TransientErrorCooldownSeconds int `yaml:"transient-error-cooldown-seconds" json:"transient-error-cooldown-seconds"`

	// QuotaSnapshotRefresh controls the background quota snapshot refresher.
	QuotaSnapshotRefresh QuotaSnapshotRefreshConfig `yaml:"quota-snapshot-refresh" json:"quota-snapshot-refresh"`

	// AuthAutoRefreshWorkers overrides the size of the core auth auto-refresh worker pool.
	// When <= 0, the default worker count is used.
	AuthAutoRefreshWorkers int `yaml:"auth-auto-refresh-workers" json:"auth-auto-refresh-workers"`

	// RequestRetry defines the retry times when the request failed.
	RequestRetry int `yaml:"request-retry" json:"request-retry"`
	// MaxRetryCredentials defines the maximum number of credentials to try for a failed request.
	// Set to 0 or a negative value to keep trying all available credentials (legacy behavior).
	MaxRetryCredentials int `yaml:"max-retry-credentials" json:"max-retry-credentials"`
	// MaxRetryInterval defines the maximum wait time in seconds before retrying a cooled-down credential.
	MaxRetryInterval int `yaml:"max-retry-interval" json:"max-retry-interval"`

	// QuotaExceeded defines the behavior when a quota is exceeded.
	QuotaExceeded QuotaExceeded `yaml:"quota-exceeded" json:"quota-exceeded"`

	// Routing controls credential selection behavior.
	Routing RoutingConfig `yaml:"routing" json:"routing"`

	// WebsocketAuth enables or disables authentication for the WebSocket API.
	WebsocketAuth bool `yaml:"ws-auth" json:"ws-auth"`

	// AntigravitySignatureCacheEnabled controls whether signature cache validation is enabled for thinking blocks.
	// When true (default), cached signatures are preferred and validated.
	// When false, client signatures are used directly after normalization (bypass mode).
	AntigravitySignatureCacheEnabled *bool `yaml:"antigravity-signature-cache-enabled,omitempty" json:"antigravity-signature-cache-enabled,omitempty"`

	AntigravitySignatureBypassStrict *bool `yaml:"antigravity-signature-bypass-strict,omitempty" json:"antigravity-signature-bypass-strict,omitempty"`

	// GeminiKey defines Gemini API key configurations with optional routing overrides.
	GeminiKey []GeminiKey `yaml:"gemini-api-key" json:"gemini-api-key"`

	// KiroKey defines a list of Kiro (AWS CodeWhisperer) configurations.
	KiroKey []KiroKey `yaml:"kiro" json:"kiro"`

	// KiroFingerprint defines a global fingerprint configuration for all Kiro requests.
	// When set, all Kiro requests will use this fixed fingerprint instead of random generation.
	KiroFingerprint *KiroFingerprintConfig `yaml:"kiro-fingerprint,omitempty" json:"kiro-fingerprint,omitempty"`

	// KiroPreferredEndpoint sets the global default preferred endpoint for all Kiro providers.
	// Values: "ide" (default, CodeWhisperer) or "cli" (Amazon Q).
	KiroPreferredEndpoint string `yaml:"kiro-preferred-endpoint" json:"kiro-preferred-endpoint"`

	// InteractionsKey defines native Google Interactions API key configurations.
	InteractionsKey []GeminiKey `yaml:"interactions-api-key" json:"interactions-api-key"`

	// Codex defines a list of Codex API key configurations as specified in the YAML configuration file.
	CodexKey []CodexKey `yaml:"codex-api-key" json:"codex-api-key"`

	// XAIKey defines xAI API key configurations using the same structure as Codex API keys.
	XAIKey []XAIKey `yaml:"xai-api-key" json:"xai-api-key"`

	// Codex configures provider-wide Codex request behavior.
	Codex CodexConfig `yaml:"codex" json:"codex"`

	// CodexHeaderDefaults configures fallback headers for Codex OAuth model requests.
	// These are used only when the client does not send its own headers.
	CodexHeaderDefaults CodexHeaderDefaults `yaml:"codex-header-defaults" json:"codex-header-defaults"`

	// ClaudeKey defines a list of Claude API key configurations as specified in the YAML configuration file.
	ClaudeKey []ClaudeKey `yaml:"claude-api-key" json:"claude-api-key"`

	// ClaudeHeaderDefaults configures default header values for Claude API requests.
	// These are used as fallbacks when the client does not send its own headers.
	ClaudeHeaderDefaults ClaudeHeaderDefaults `yaml:"claude-header-defaults" json:"claude-header-defaults"`

	// Claude configures Claude-specific request policy.
	Claude ClaudeConfig `yaml:"claude" json:"claude"`

	// ManagedHeaderProfile controls online refresh of version-sensitive managed header profiles.
	ManagedHeaderProfile ManagedHeaderProfileConfig `yaml:"managed-header-profile" json:"managed-header-profile"`

	// NormalizeAccountEnv is a global switch (requirement ⑦) that rewrites the real
	// cwd / home paths inside <env> and <system-reminder> blocks of the request body
	// to a per-account canonical path. It is independent of cloak / device-profile
	// stabilization and defaults to disabled (nil/false): when off the body is left
	// untouched, so enabling it is a safe, zero-migration gray rollout that can be
	// rolled back by turning the switch off again.
	NormalizeAccountEnv *bool `yaml:"normalize-account-env,omitempty" json:"normalize-account-env,omitempty"`

	// DisableClaudeCloakMode globally disables Claude request cloaking when true.
	// Cloaking disguises requests as the official Claude Code CLI and replaces the
	// system prompt. When true, every Claude credential defaults to no cloaking
	// ("never"); a specific credential can still re-enable or override it via its own
	// cloak settings (the per claude-api-key "cloak" block, or a "cloak_mode" value in
	// the auth/OAuth token file). Default false preserves the per-client "auto" behavior.
	DisableClaudeCloakMode bool `yaml:"disable-claude-cloak-mode" json:"disable-claude-cloak-mode"`

	// OpenAICompatibility defines OpenAI API compatibility configurations for external providers.
	OpenAICompatibility []OpenAICompatibility `yaml:"openai-compatibility" json:"openai-compatibility"`

	// VertexCompatAPIKey defines Vertex AI-compatible API key configurations for third-party providers.
	// Used for services that use Vertex AI-style paths but with simple API key authentication.
	VertexCompatAPIKey []VertexCompatKey `yaml:"vertex-api-key" json:"vertex-api-key"`

	// AmpCode contains Amp CLI upstream configuration, management restrictions, and model mappings.
	AmpCode AmpCode `yaml:"ampcode" json:"ampcode"`

	// OAuthExcludedModels defines per-provider global model exclusions applied to OAuth/file-backed auth entries.
	OAuthExcludedModels map[string][]string `yaml:"oauth-excluded-models,omitempty" json:"oauth-excluded-models,omitempty"`

	// OAuthModelAlias defines global model name aliases for OAuth/file-backed auth channels.
	// These aliases affect both model listing and model routing for supported channels:
	// vertex, aistudio, antigravity, claude, codex, kimi, xai.
	//
	// NOTE: This does not apply to existing per-credential model alias features under:
	// gemini-api-key, interactions-api-key, codex-api-key, xai-api-key, claude-api-key, openai-compatibility, and vertex-api-key.
	OAuthModelAlias map[string][]OAuthModelAlias `yaml:"oauth-model-alias,omitempty" json:"oauth-model-alias,omitempty"`

	// Payload defines default and override rules for provider payload parameters.
	Payload PayloadConfig `yaml:"payload" json:"payload"`

	// IncognitoBrowser enables opening OAuth URLs in incognito/private browsing mode.
	// This is useful when you want to login with a different account without logging out
	// from your current session. Default: false.
	IncognitoBrowser bool `yaml:"incognito-browser" json:"incognito-browser"`

	// CyberPolicyAlert configures the upstream cyber_policy flag alert side channel.
	// When WebhookURL is empty (default), no outbound POST is fired; structured logs
	// and per-account counters still update.
	CyberPolicyAlert CyberPolicyAlertConfig `yaml:"cyber-policy-alert" json:"cyber-policy-alert"`
}
