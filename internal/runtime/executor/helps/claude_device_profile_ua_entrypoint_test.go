package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNormalizeClaudeUserAgentEntrypoint_FoldsSdkCliKeepingVersion pins the
// token-endpoint (OAuth refresh / reauth) UA fold: a high-water User-Agent whose
// suffix entrypoint is "sdk-cli" (which a single `claude --print` can seed into an
// account's frozen device profile) is folded to "(external, cli)" so the
// token-endpoint egress matches the serving outbound UA suffix aligned by
// AlignClaudeDeviceProfileUserAgentSuffix. The "claude-cli/<version>" high-water
// prefix (e.g. 2.1.215) must be preserved — only the entrypoint field is rewritten.
func TestNormalizeClaudeUserAgentEntrypoint_FoldsSdkCliKeepingVersion(t *testing.T) {
	enabled := true
	disabled := false

	cases := []struct {
		name string
		cfg  *config.Config
		ua   string
		want string
	}{
		{
			// nil cfg exercises the documented default:
			// config.NormalizeSdkCliEntrypointEnabled(nil) == true.
			name: "default config folds sdk-cli to cli and keeps high-water version",
			cfg:  nil,
			ua:   "claude-cli/2.1.215 (external, sdk-cli)",
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			name: "explicitly enabled folds sdk-cli to cli",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &enabled}},
			ua:   "claude-cli/2.1.215 (external, sdk-cli)",
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			// Escape hatch: with normalization disabled the sdk-cli suffix is
			// left verbatim (the pre-fold behavior), so token-endpoint UA still
			// matches serving, which also stops folding under the same switch.
			name: "disabled keeps sdk-cli verbatim",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}},
			ua:   "claude-cli/2.1.215 (external, sdk-cli)",
			want: "claude-cli/2.1.215 (external, sdk-cli)",
		},
		{
			name: "already-cli suffix untouched",
			cfg:  nil,
			ua:   "claude-cli/2.1.215 (external, cli)",
			want: "claude-cli/2.1.215 (external, cli)",
		},
		{
			name: "non-sdk-cli entrypoint (vscode) untouched",
			cfg:  nil,
			ua:   "claude-cli/2.1.215 (external, vscode)",
			want: "claude-cli/2.1.215 (external, vscode)",
		},
		{
			name: "no parenthetical block untouched",
			cfg:  nil,
			ua:   "claude-cli/2.1.215",
			want: "claude-cli/2.1.215",
		},
		{
			name: "empty string untouched",
			cfg:  nil,
			ua:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeClaudeUserAgentEntrypoint(tc.cfg, tc.ua); got != tc.want {
				t.Fatalf("NormalizeClaudeUserAgentEntrypoint(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}
