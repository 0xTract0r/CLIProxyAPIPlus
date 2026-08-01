package config

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

// PluginsConfig holds dynamic plugin system settings.
type PluginsConfig struct {
	// Enabled toggles dynamic plugin loading.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir is the plugin discovery directory.
	Dir string `yaml:"dir" json:"dir"`
	// StoreSources appends third-party plugin store registries to the built-in official source.
	StoreSources []string `yaml:"store-sources,omitempty" json:"store-sources,omitempty"`
	// StoreAuth defines optional auth rules for plugin store registry, metadata, and artifact requests.
	StoreAuth []sdkpluginstore.AuthConfig `yaml:"store-auth,omitempty" json:"store-auth,omitempty"`
	// AuthRevision changes when Home-managed plugin credentials change.
	AuthRevision int64 `yaml:"auth-revision,omitempty" json:"auth-revision,omitempty"`
	// Configs stores per-plugin instance configuration by plugin ID.
	Configs map[string]PluginInstanceConfig `yaml:"configs" json:"configs"`
}

// PluginInstanceConfig stores host-owned plugin settings and the original plugin YAML subtree.
type PluginInstanceConfig struct {
	// Enabled toggles this plugin instance. Nil is normalized to false during YAML parsing.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Priority controls plugin startup and routing order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Raw preserves the full original plugin configuration YAML subtree.
	Raw yaml.Node `yaml:"-" json:"-"`
}

// UnmarshalYAML extracts host-owned fields while preserving the full original YAML node.
func (c *PluginInstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil {
		return nil
	}

	c.Priority = 0
	defaultEnabled := false
	c.Enabled = &defaultEnabled

	if value == nil || value.Kind == 0 {
		c.Raw = *defaultPluginInstanceConfigNode()
		return nil
	}

	c.Raw = *deepCopyNode(value)
	if value.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i]
		node := value.Content[i+1]
		if key == nil {
			continue
		}
		switch key.Value {
		case "enabled":
			var enabled bool
			if errDecodeEnabled := node.Decode(&enabled); errDecodeEnabled != nil {
				return fmt.Errorf("parse plugin enabled: %w", errDecodeEnabled)
			}
			c.Enabled = &enabled
		case "priority":
			var priority int
			if errDecodePriority := node.Decode(&priority); errDecodePriority != nil {
				return fmt.Errorf("parse plugin priority: %w", errDecodePriority)
			}
			c.Priority = priority
		}
	}

	return nil
}

// MarshalYAML returns the preserved raw plugin YAML subtree for lossless config output.
func (c PluginInstanceConfig) MarshalYAML() (any, error) {
	if c.Raw.Kind == 0 {
		return defaultPluginInstanceConfigNode(), nil
	}
	return deepCopyNode(&c.Raw), nil
}

func defaultPluginInstanceConfigNode() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{},
	}
}

// ClaudeHeaderDefaults configures default header values injected into Claude API requests.
// In legacy mode, UserAgent/PackageVersion/RuntimeVersion/Timeout act as fallbacks when
// the client omits them, while OS/Arch remain runtime-derived. When stabilized device
// profiles are enabled, OS/Arch become the pinned platform baseline, while
// UserAgent/PackageVersion/RuntimeVersion seed the upgradeable software fingerprint.
type ClaudeHeaderDefaults struct {
	UserAgent              string `yaml:"user-agent" json:"user-agent"`
	PackageVersion         string `yaml:"package-version" json:"package-version"`
	RuntimeVersion         string `yaml:"runtime-version" json:"runtime-version"`
	OS                     string `yaml:"os" json:"os"`
	Arch                   string `yaml:"arch" json:"arch"`
	Timeout                string `yaml:"timeout" json:"timeout"`
	StabilizeDeviceProfile *bool  `yaml:"stabilize-device-profile,omitempty" json:"stabilize-device-profile,omitempty"`

	// ReplayWireHeaderOrder, when true, makes the claude serving/quota outbound
	// transport replay the real claude-cli (undici/Stainless) HTTP/1.1 request
	// header wire order AND original header-name casing, instead of Go net/http's
	// canonical Title-Case + alphabetical order. This closes the JA4H "_hd"
	// (header-order) fingerprint gap on claude egress. It only affects the
	// claude_cli_clienthello_v1 uTLS path; codex/gemini and the OAuth
	// token-refresh path are unaffected. Defaults to disabled (nil == false):
	// opt-in until validated against a real upstream, and gate-off preserves the
	// exact current Go header behavior.
	ReplayWireHeaderOrder *bool `yaml:"replay-wire-header-order,omitempty" json:"replay-wire-header-order,omitempty"`
}

// CodexHeaderDefaults configures fallback header values injected into Codex
// model requests for OAuth/file-backed auth when the client omits them.
// UserAgent applies to HTTP and websocket requests; BetaFeatures only applies to websockets.
type CodexHeaderDefaults struct {
	UserAgent    string `yaml:"user-agent" json:"user-agent"`
	BetaFeatures string `yaml:"beta-features" json:"beta-features"`
	// fork(anticorr Wave10-D)：codex CLI 画像可选 pin 杠杆。无配置时用代码内置的
	// codex_cli_rs CLI 默认（Originator=codex_cli_rs，OS/arch/terminal 稳定 pin）。
	// 这些字段允许 operator 覆盖默认 CLI 画像（不透传真实环境，每账号稳定一致）。
	// Originator 为空时用代码默认 codex_cli_rs；OS/Arch/Terminal 用于构造稳定 UA 平台段
	// 与 terminal 尾段，仅在未直接配置 UserAgent 时生效。
	Originator string `yaml:"originator,omitempty" json:"originator,omitempty"`
	OS         string `yaml:"os,omitempty" json:"os,omitempty"`
	Arch       string `yaml:"arch,omitempty" json:"arch,omitempty"`
	Terminal   string `yaml:"terminal,omitempty" json:"terminal,omitempty"`
}

// CodexConfig configures provider-wide Codex request behavior.
type CodexConfig struct {
	IdentityConfuse bool `yaml:"identity-confuse" json:"identity-confuse"`
	// OptimizeMultiAgentV2 optimizes official Codex multi-agent requests.
	OptimizeMultiAgentV2 bool `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`
	// LiveMediaRelay terminates and relays Codex Live WebRTC media in this process.
	LiveMediaRelay CodexLiveMediaRelayConfig `yaml:"live-media-relay" json:"live-media-relay"`
}

// CodexLiveMediaRelayConfig configures the in-process Codex Live WebRTC gateway.
type CodexLiveMediaRelayConfig struct {
	Enabled                 bool                 `yaml:"enabled" json:"enabled"`
	MaxSessions             int                  `yaml:"max-sessions" json:"max-sessions"`
	DisablePrivateRemoteIPs bool                 `yaml:"disable-private-remote-ips" json:"disable-private-remote-ips"`
	PublicIP                string               `yaml:"public-ip" json:"public-ip"`
	UDPPortMin              uint16               `yaml:"udp-port-min" json:"udp-port-min"`
	UDPPortMax              uint16               `yaml:"udp-port-max" json:"udp-port-max"`
	ICEServers              []CodexLiveICEServer `yaml:"ice-servers" json:"ice-servers"`
}

// CodexLiveICEServer configures a STUN or TURN server for the media relay.
type CodexLiveICEServer struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username" json:"-"`
	Credential string   `yaml:"credential" json:"-"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
}

// PprofConfig holds pprof HTTP server settings.
type PprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// DisableAutoUpdatePanel disables automatic periodic background updates of the management panel asset from GitHub.
	// When false (the default), the background updater remains enabled; when true, the panel is only downloaded on first access if missing.
	DisableAutoUpdatePanel bool `yaml:"disable-auto-update-panel"`
	// PanelGitHubRepository overrides the GitHub repository used to fetch the management panel asset.
	// Accepts either a repository URL (https://github.com/org/repo) or an API releases endpoint.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
}

// QuotaExceeded defines the behavior when API quota limits are exceeded.
// It provides configuration options for automatic failover mechanisms.
type QuotaExceeded struct {
	// SwitchProject indicates whether to automatically switch to another project when a quota is exceeded.
	SwitchProject bool `yaml:"switch-project" json:"switch-project"`

	// SwitchPreviewModel indicates whether to automatically switch to a preview model when a quota is exceeded.
	SwitchPreviewModel bool `yaml:"switch-preview-model" json:"switch-preview-model"`

	// AntigravityCredits enables credits-based last-resort fallback for Claude models.
	// When all free-tier auths are exhausted (429/503), the conductor retries with
	// an auth that has available Google One AI credits.
	AntigravityCredits bool `yaml:"antigravity-credits" json:"antigravity-credits"`
}

// RoutingConfig configures how credentials are selected for requests.
type RoutingConfig struct {
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "fill-first".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// ClaudeCodeSessionAffinity enables session-sticky routing for Claude Code clients.
	// Deprecated: Use SessionAffinity instead for universal session support.
	ClaudeCodeSessionAffinity bool `yaml:"claude-code-session-affinity,omitempty" json:"claude-code-session-affinity,omitempty"`

	// SessionAffinity enables universal session-sticky routing for all clients.
	// Session IDs are extracted from multiple sources:
	// metadata.user_id (Claude Code session format), X-Session-ID, Session_id (Codex),
	// X-Client-Request-Id (PI), metadata.user_id, conversation_id, or message hash.
	// Automatic failover is always enabled when bound auth becomes unavailable.
	SessionAffinity bool `yaml:"session-affinity,omitempty" json:"session-affinity,omitempty"`

	// SessionAffinityTTL specifies how long session-to-auth bindings are retained.
	// Default: 1h. Accepts duration strings like "30m", "1h", "2h30m".
	SessionAffinityTTL string `yaml:"session-affinity-ttl,omitempty" json:"session-affinity-ttl,omitempty"`
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork,omitempty" json:"fork,omitempty"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "gpt-*", "*-5", "gemini-*-pro").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "gemini", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
	// Headers restricts the rule to requests whose headers match all configured wildcard patterns.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// FromProtocol restricts the rule to a specific source protocol (e.g., "gemini", "responses").
	FromProtocol string `yaml:"from-protocol" json:"from-protocol"`
	// Match requires payload JSON paths to equal the configured values.
	Match []map[string]any `yaml:"match" json:"match"`
	// NotMatch requires payload JSON paths to not equal the configured values.
	NotMatch []map[string]any `yaml:"not-match" json:"not-match"`
	// Exist requires payload JSON paths to exist and not be null.
	Exist []string `yaml:"exist" json:"exist"`
	// NotExist requires payload JSON paths to be missing or null.
	NotExist []string `yaml:"not-exist" json:"not-exist"`
}

// CloakConfig configures request cloaking for non-Claude-Code clients.
// Cloaking disguises API requests to appear as originating from the official Claude Code CLI.
type CloakConfig struct {
	// Mode controls cloaking behavior: "auto" (default), "always", or "never".
	// - "auto": cloak only when client is not Claude Code (based on User-Agent)
	// - "always": always apply cloaking regardless of client
	// - "never": never apply cloaking
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// StrictMode controls how system prompts are handled when cloaking.
	// - false (default): prepend Claude Code prompt to user system messages
	// - true: strip all user system messages, keep only Claude Code prompt
	StrictMode bool `yaml:"strict-mode,omitempty" json:"strict-mode,omitempty"`

	// SensitiveWords is a list of words to obfuscate with zero-width characters.
	// This can help bypass certain content filters.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// CacheUserID controls whether Claude user_id values are cached per API key.
	// When false, a fresh random user_id is generated for every request.
	CacheUserID *bool `yaml:"cache-user-id,omitempty" json:"cache-user-id,omitempty"`
}

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// RebuildMidSystemMessage moves Claude messages with role "system" into the top-level system field.
	RebuildMidSystemMessage bool `yaml:"rebuild-mid-system-message,omitempty" json:"rebuild-mid-system-message,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// Cloak configures request cloaking for non-Claude-Code clients.
	Cloak *CloakConfig `yaml:"cloak,omitempty" json:"cloak,omitempty"`

	// ExperimentalCCHSigning enables opt-in final-body cch signing for cloaked
	// Claude /v1/messages requests. It is disabled by default so upstream seed
	// changes do not alter the proxy's legacy behavior.
	ExperimentalCCHSigning bool `yaml:"experimental-cch-signing,omitempty" json:"experimental-cch-signing,omitempty"`
}

func (k ClaudeKey) GetAPIKey() string { return k.APIKey }

func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

func (m ClaudeModel) GetName() string { return m.Name }

func (m ClaudeModel) GetAlias() string { return m.Alias }

func (m ClaudeModel) GetDisplayName() string { return m.DisplayName }

func (m ClaudeModel) GetForceMapping() bool { return m.ForceMapping }

// CodexKey represents the configuration for a Codex API key,
// including the API key itself and an optional base URL for the API endpoint.
type CodexKey struct {
	// APIKey is the authentication key for accessing Codex API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gpt-5-codex").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Codex API endpoint.
	// If empty, the default Codex API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// Websockets enables the Responses API websocket transport for this credential.
	Websockets bool `yaml:"websockets,omitempty" json:"websockets,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []CodexModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

func (k CodexKey) GetAPIKey() string { return k.APIKey }

func (k CodexKey) GetBaseURL() string { return k.BaseURL }

// CodexModel describes a mapping between an alias and the actual upstream model name.
type CodexModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

func (m CodexModel) GetName() string { return m.Name }

func (m CodexModel) GetAlias() string { return m.Alias }

func (m CodexModel) GetDisplayName() string { return m.DisplayName }

func (m CodexModel) GetForceMapping() bool { return m.ForceMapping }

// XAIKey uses the Codex API key structure for native xAI execution.
type XAIKey = CodexKey

// XAIModel uses the Codex model mapping structure for xAI models.
type XAIModel = CodexModel

// GeminiKey represents the configuration for a Gemini API key,
// including optional overrides for upstream base URL, proxy routing, and headers.
type GeminiKey struct {
	// APIKey is the authentication key for accessing Gemini API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Gemini API endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []GeminiModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

func (k GeminiKey) GetAPIKey() string { return k.APIKey }

func (k GeminiKey) GetBaseURL() string { return k.BaseURL }

// GeminiModel describes a mapping between an alias and the actual upstream model name.
type GeminiModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

func (m GeminiModel) GetName() string { return m.Name }

func (m GeminiModel) GetAlias() string { return m.Alias }

func (m GeminiModel) GetDisplayName() string { return m.DisplayName }

func (m GeminiModel) GetForceMapping() bool { return m.ForceMapping }

// OpenAICompatibility represents the configuration for OpenAI API compatibility
// with external providers, allowing model aliases to be routed through OpenAI API format.
type OpenAICompatibility struct {
	// Name is the identifier for this OpenAI compatibility configuration.
	Name string `yaml:"name" json:"name"`

	// Priority controls selection preference when multiple providers or credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Disabled prevents this provider from being used for routing.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Prefix optionally namespaces model aliases for this provider (e.g., "teamA/kimi-k2").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the external OpenAI-compatible API endpoint.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// APIKeyEntries defines API keys with optional per-key proxy configuration.
	APIKeyEntries []OpenAICompatibilityAPIKey `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []OpenAICompatibilityModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent to this provider.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this provider when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

// OpenAICompatibilityAPIKey represents an API key configuration with optional proxy setting.
type OpenAICompatibilityAPIKey struct {
	// APIKey is the authentication key for accessing the external API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
}

// OpenAICompatibilityModel represents a model configuration for OpenAI compatibility,
// including the actual model name and its alias for API routing.
type OpenAICompatibilityModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// Image marks this model as callable through /v1/images/generations and /v1/images/edits.
	Image bool `yaml:"image,omitempty" json:"image,omitempty"`

	// InputModalities declares chat/responses input capabilities (e.g. text, image) for Codex and other clients.
	// This is separate from Image, which only enables /v1/images/* endpoints.
	InputModalities []string `yaml:"input-modalities,omitempty" json:"input-modalities,omitempty"`

	// OutputModalities declares supported output modalities when known (e.g. text, image).
	OutputModalities []string `yaml:"output-modalities,omitempty" json:"output-modalities,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	// If nil, the model defaults to level-based reasoning with levels ["low", "medium", "high"].
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m OpenAICompatibilityModel) GetName() string { return m.Name }

func (m OpenAICompatibilityModel) GetAlias() string { return m.Alias }

func (m OpenAICompatibilityModel) GetDisplayName() string { return m.DisplayName }

func (m OpenAICompatibilityModel) GetForceMapping() bool { return m.ForceMapping }

// ErrorLogAlertConfig controls external notifications for application error logs.
type ErrorLogAlertConfig struct {
	// FeishuWebhookURL is a Feishu custom bot webhook URL. Empty disables alerting.
	FeishuWebhookURL string `yaml:"feishu-webhook-url" json:"feishu-webhook-url"`
}

// CyberPolicyAlertConfig groups optional side-channel configuration for the
// Codex /v1/responses upstream cyber_policy event. When WebhookURL is empty the
// alert subsystem keeps logging and counting hits without firing HTTP callouts.
type CyberPolicyAlertConfig struct {
	// WebhookURL receives an asynchronous JSON POST when a cyber_policy hit is
	// recorded. Default is empty (disabled).
	WebhookURL string `yaml:"webhook-url" json:"webhook-url"`
}

// ClaudeConfig contains Claude-specific runtime policy.
type ClaudeConfig struct {
	// SonnetLongContextPolicy controls how Sonnet requests above the normal
	// 200K window are handled. Recognized values:
	// fail_with_hint, route_to_opus_1m, compact_required.
	SonnetLongContextPolicy string `yaml:"sonnet_long_context_policy" json:"sonnet_long_context_policy"`

	// NormalizeSdkCliEntrypoint controls whether outbound Claude requests fold an
	// inbound "sdk-cli" cc_entrypoint into "cli" before it reaches Anthropic. Real
	// interactive claude-cli always emits cc_entrypoint=cli; "sdk-cli" is the
	// self-reported entrypoint tag emitted by Claude Agent SDK / `claude -p`
	// non-interactive invocations, and Anthropic policy disallows Agent SDK usage
	// against subscription OAuth. The fold is applied identically to the outbound
	// User-Agent parenthetical suffix (helps.AlignClaudeDeviceProfileUserAgentSuffix)
	// and the billing-header cc_entrypoint (parseEntrypointFromUA in
	// claude_executor.go) so the two never diverge (a UA/entrypoint mismatch is
	// itself a detectable signal real claude-code never produces). Only the exact
	// "sdk-cli" token is folded; every other entrypoint (cli, vscode, ide, ...) is
	// left untouched.
	//
	// Defaults to enabled (nil == true) so `claude -p` / Agent SDK traffic is
	// normalized out of the box without requiring config changes. Set to false to
	// restore the previous "mirror inbound entrypoint verbatim" behavior, e.g. for
	// rollback.
	NormalizeSdkCliEntrypoint *bool `yaml:"normalize-sdk-cli-entrypoint,omitempty" json:"normalize-sdk-cli-entrypoint,omitempty"`

	// AlignRealPathBillingVersion controls whether the REAL serving path (genuine
	// claude-cli traffic, helps.ShouldCloak == false) rewrites the body
	// x-anthropic-billing-header cc_version=<version>.<build> token's <version>
	// segment to the account high-water billing version V — the same V the outbound
	// User-Agent is floored up to (resolveClaudeBillingVersion). On the real path
	// applyCloaking early-returns before the cloaked-path cc_version floor
	// (checkSystemInstructionsWithSigningMode), so a below-high-water client would
	// otherwise emit an outbound UA floored to V while its body cc_version stays at
	// the lower client version — a "one account, two versions" mismatch real
	// claude-code never produces. Aligning the body version closes that gap; the
	// billing-header cch is re-signed so it still covers the rewritten body.
	//
	// Only the <version> segment is rewritten; the <build> fingerprint segment is
	// always passed through byte-for-byte (never recomputed — the build fingerprint
	// algorithm is not yet real-machine validated, so forging a build no real
	// client emits would itself be a detection signal).
	//
	// Defaults to DISABLED (nil == false). This real-path body mutation stays INERT
	// until an operator explicitly opts in after real-machine (`claude -p` MITM)
	// validation, so a stock config leaves the real serving path byte-for-byte
	// unchanged.
	AlignRealPathBillingVersion *bool `yaml:"align-real-path-billing-version,omitempty" json:"align-real-path-billing-version,omitempty"`
}

// ManagedHeaderProfileConfig controls whether core can consult public online
// registries to refresh provider-managed version markers. The pointer bool lets
// config-loaded runtimes default to enabled while hand-built test configs stay
// offline unless explicitly opted in.
type ManagedHeaderProfileConfig struct {
	OnlineUpdate        *bool `yaml:"online-update,omitempty" json:"online-update,omitempty"`
	FetchTimeoutSeconds int   `yaml:"fetch-timeout-seconds,omitempty" json:"fetch-timeout-seconds,omitempty"`
	CacheTTLSeconds     int   `yaml:"cache-ttl-seconds,omitempty" json:"cache-ttl-seconds,omitempty"`
}

// QuotaSnapshotRefreshConfig controls persisted quota snapshot refresh policy.
type QuotaSnapshotRefreshConfig struct {
	Enabled             *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Interval            string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Jitter              string `yaml:"jitter,omitempty" json:"jitter,omitempty"`
	StartupCatchUp      *bool  `yaml:"startup-catch-up,omitempty" json:"startup-catch-up,omitempty"`
	StartupMaxStaleness string `yaml:"startup-max-staleness,omitempty" json:"startup-max-staleness,omitempty"`
}

// AmpModelMapping defines a single Amp CLI model routing override.
// When Amp requests a model that isn't available locally, this mapping
// allows routing to an alternative model that IS available.
type AmpModelMapping struct {
	// From is the model name that Amp CLI requests (e.g., "claude-opus-4.5").
	From string `yaml:"from" json:"from"`

	// To is the target model name to route to (e.g., "claude-sonnet-4").
	// The target model must have available providers in the registry.
	To string `yaml:"to" json:"to"`

	// Regex indicates whether the 'from' field should be interpreted as a regular
	// expression for matching model names. When true, this mapping is evaluated
	// after exact matches and in the order provided. Defaults to false (exact match).
	Regex bool `yaml:"regex,omitempty" json:"regex,omitempty"`
}

// AmpCode groups Amp CLI integration settings including upstream routing,
// optional overrides, management route restrictions, and model fallback mappings.
type AmpCode struct {
	// UpstreamURL defines the upstream Amp control plane used for non-provider calls.
	UpstreamURL string `yaml:"upstream-url" json:"upstream-url"`

	// UpstreamAPIKey optionally overrides the Authorization header when proxying Amp upstream calls.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// UpstreamAPIKeys maps client API keys (from top-level api-keys) to upstream API keys.
	// When a request is authenticated with one of the APIKeys, the corresponding UpstreamAPIKey
	// is used for the upstream Amp request.
	UpstreamAPIKeys []AmpUpstreamAPIKeyEntry `yaml:"upstream-api-keys,omitempty" json:"upstream-api-keys,omitempty"`

	// RestrictManagementToLocalhost restricts Amp management routes (/api/user, /api/threads, etc.)
	// to only accept connections from localhost (127.0.0.1, ::1). When true, prevents drive-by
	// browser attacks and remote access to management endpoints. Default: false (API key auth is sufficient).
	RestrictManagementToLocalhost bool `yaml:"restrict-management-to-localhost" json:"restrict-management-to-localhost"`

	// ModelMappings defines model name mappings for Amp CLI requests.
	// When Amp requests a model that isn't available locally, these mappings
	// allow routing to an alternative model that IS available.
	ModelMappings []AmpModelMapping `yaml:"model-mappings" json:"model-mappings"`

	// ForceModelMappings when true, model mappings take precedence over local API keys.
	// When false (default), local API keys are used first if available.
	ForceModelMappings bool `yaml:"force-model-mappings" json:"force-model-mappings"`
}

// AmpUpstreamAPIKeyEntry maps a set of client API keys to a specific upstream API key.
// When a request is authenticated with one of the APIKeys, the corresponding UpstreamAPIKey
// is used for the upstream Amp request.
type AmpUpstreamAPIKeyEntry struct {
	// UpstreamAPIKey is the API key to use when proxying to the Amp upstream.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// APIKeys are the client API keys (from top-level api-keys) that map to this upstream key.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// KiroKey represents the configuration for Kiro (AWS CodeWhisperer) authentication.
type KiroKey struct {
	TokenFile         string `yaml:"token-file,omitempty" json:"token-file,omitempty"`
	AccessToken       string `yaml:"access-token,omitempty" json:"access-token,omitempty"`
	RefreshToken      string `yaml:"refresh-token,omitempty" json:"refresh-token,omitempty"`
	ExpiresAt         string `yaml:"expires-at,omitempty" json:"expires-at,omitempty"`
	Email             string `yaml:"email,omitempty" json:"email,omitempty"`
	ProfileArn        string `yaml:"profile-arn,omitempty" json:"profile-arn,omitempty"`
	Region            string `yaml:"region,omitempty" json:"region,omitempty"`
	AgentTaskType     string `yaml:"agent-task-type,omitempty" json:"agent-task-type,omitempty"`
	Priority          int    `yaml:"priority,omitempty" json:"priority,omitempty"`
	Prefix            string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	PreferredEndpoint string `yaml:"preferred-endpoint,omitempty" json:"preferred-endpoint,omitempty"`
	ProxyURL          string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
}

// KiroFingerprintConfig defines a global fingerprint configuration for Kiro requests.
type KiroFingerprintConfig struct {
	OIDCSDKVersion      string `yaml:"oidc-sdk-version,omitempty" json:"oidc-sdk-version,omitempty"`
	RuntimeSDKVersion   string `yaml:"runtime-sdk-version,omitempty" json:"runtime-sdk-version,omitempty"`
	StreamingSDKVersion string `yaml:"streaming-sdk-version,omitempty" json:"streaming-sdk-version,omitempty"`
	OSType              string `yaml:"os-type,omitempty" json:"os-type,omitempty"`
	OSVersion           string `yaml:"os-version,omitempty" json:"os-version,omitempty"`
	NodeVersion         string `yaml:"node-version,omitempty" json:"node-version,omitempty"`
	KiroVersion         string `yaml:"kiro-version,omitempty" json:"kiro-version,omitempty"`
	KiroHash            string `yaml:"kiro-hash,omitempty" json:"kiro-hash,omitempty"`
	BuildVersion        string `yaml:"build-version,omitempty" json:"build-version,omitempty"`
	ClientID            string `yaml:"client-id,omitempty" json:"client-id,omitempty"`
	Platform            string `yaml:"platform,omitempty" json:"platform,omitempty"`
}
