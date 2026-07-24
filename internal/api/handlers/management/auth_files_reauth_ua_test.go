package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// authWithClaudeHighWaterUA builds an *Auth whose metadata carries a claude
// device-profile high-water mark with the given outbound User-Agent, mirroring
// the persisted shape reauth reads back before injecting it via WithUserAgent.
func authWithClaudeHighWaterUA(ua string) *coreauth.Auth {
	return &coreauth.Auth{
		Metadata: map[string]any{
			coreauth.ClaudeDeviceHighWaterMetadataKey: map[string]any{
				"user_agent":      ua,
				"version":         "2.1.215",
				"package_version": "0.1.0",
				"runtime_version": "v22.0.0",
			},
		},
	}
}

// TestClaudeReauthHighWaterUserAgent_FoldsSdkCliKeepingVersion pins the reauth
// (OAuth token endpoint) injection path used by newClaudeOAuthAuth /
// newClaudeOAuthAccountProxyFallbackAuth: the User-Agent handed to
// ClaudeAuth.WithUserAgent is the account's high-water UA with its suffix
// entrypoint folded (sdk-cli -> cli) under the default config, so the reauth
// exchange presents the same "(external, cli)" identity this account's serving
// requests present (aligned by helps.AlignClaudeDeviceProfileUserAgentSuffix). The
// high-water version (2.1.215) is preserved.
func TestClaudeReauthHighWaterUserAgent_FoldsSdkCliKeepingVersion(t *testing.T) {
	enabled := true
	disabled := false

	cases := []struct {
		name   string
		cfg    *config.Config
		target *coreauth.Auth
		want   string
	}{
		{
			name:   "default config folds sdk-cli high-water to cli",
			cfg:    nil,
			target: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want:   "claude-cli/2.1.215 (external, cli)",
		},
		{
			name:   "explicitly enabled folds sdk-cli high-water to cli",
			cfg:    &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &enabled}},
			target: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want:   "claude-cli/2.1.215 (external, cli)",
		},
		{
			name:   "disabled keeps sdk-cli high-water verbatim",
			cfg:    &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}},
			target: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want:   "claude-cli/2.1.215 (external, sdk-cli)",
		},
		{
			name:   "already-cli high-water preserved verbatim",
			cfg:    nil,
			target: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, cli)"),
			want:   "claude-cli/2.1.215 (external, cli)",
		},
		{
			name:   "nil target yields empty (caller keeps the OAuth floor)",
			cfg:    nil,
			target: nil,
			want:   "",
		},
		{
			name:   "target without high-water yields empty (caller keeps the OAuth floor)",
			cfg:    nil,
			target: &coreauth.Auth{Metadata: map[string]any{}},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeReauthHighWaterUserAgent(tc.cfg, tc.target); got != tc.want {
				t.Fatalf("claudeReauthHighWaterUserAgent = %q, want %q", got, tc.want)
			}
		})
	}
}
