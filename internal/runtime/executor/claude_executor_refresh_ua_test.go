package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// authWithClaudeHighWaterUA builds an *Auth whose metadata carries a claude
// device-profile high-water mark with the given outbound User-Agent, mirroring
// the persisted shape RaiseClaudeDeviceHighWater writes and Refresh reads back.
func authWithClaudeHighWaterUA(ua string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Metadata: map[string]any{
			cliproxyauth.ClaudeDeviceHighWaterMetadataKey: map[string]any{
				"user_agent":      ua,
				"version":         "2.1.215",
				"package_version": "0.1.0",
				"runtime_version": "v22.0.0",
			},
		},
	}
}

// TestClaudeRefreshHighWaterUserAgent_FoldsSdkCliKeepingVersion pins the refresh
// (OAuth token endpoint) injection path: the User-Agent injected onto the refresh
// request via svc.WithUserAgent is the account's high-water UA with its suffix
// entrypoint folded (sdk-cli -> cli) under the default config, so background token
// refresh presents the same "(external, cli)" identity this account's serving
// requests present (aligned by helps.AlignClaudeDeviceProfileUserAgentSuffix). The
// high-water version (2.1.215) is preserved.
func TestClaudeRefreshHighWaterUserAgent_FoldsSdkCliKeepingVersion(t *testing.T) {
	enabled := true
	disabled := false

	cases := []struct {
		name string
		cfg  *config.Config
		auth *cliproxyauth.Auth
		want string
	}{
		{
			name: "default config folds sdk-cli high-water to cli",
			cfg:  nil,
			auth: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			name: "explicitly enabled folds sdk-cli high-water to cli",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &enabled}},
			auth: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			name: "disabled keeps sdk-cli high-water verbatim",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}},
			auth: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)"),
			want: "claude-cli/2.1.215 (external, sdk-cli)",
		},
		{
			name: "already-cli high-water preserved verbatim",
			cfg:  nil,
			auth: authWithClaudeHighWaterUA("claude-cli/2.1.215 (external, cli)"),
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			name: "nil auth yields empty (caller keeps the OAuth floor)",
			cfg:  nil,
			auth: nil,
			want: "",
		},
		{
			name: "auth without high-water yields empty (caller keeps the OAuth floor)",
			cfg:  nil,
			auth: &cliproxyauth.Auth{Metadata: map[string]any{}},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeRefreshHighWaterUserAgent(tc.cfg, tc.auth); got != tc.want {
				t.Fatalf("claudeRefreshHighWaterUserAgent = %q, want %q", got, tc.want)
			}
		})
	}
}
