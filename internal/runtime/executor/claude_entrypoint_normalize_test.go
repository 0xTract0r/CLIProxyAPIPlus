package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestParseEntrypointFromUA_SdkCliNormalization pins telemetry-farm-ux-hardening
// T4 scope A: parseEntrypointFromUA feeds the billing cc_entrypoint for the
// count_tokens and cloak (system-block regeneration) paths (see call sites in
// claude_executor.go). The verbatim /v1/messages path — real claude-cli clients
// that skip cloak regeneration — instead folds the inbound billing header via
// normalizeClaudeBillingEntrypoint; both use the same fold token and switch.
// By default (config.NormalizeSdkCliEntrypointEnabled == true when unset), an
// inbound "sdk-cli" entrypoint — the self-reported tag emitted by Claude Agent
// SDK / `claude -p` non-interactive invocations, which Anthropic policy
// disallows against subscription OAuth — is folded to "cli", the same
// entrypoint real interactive claude-cli always emits. Every other entrypoint
// (including a real interactive "cli" — a no-op fold) is passed through
// unchanged, and the fold can be disabled via claude.normalize-sdk-cli-entrypoint
// for rollback.
func TestParseEntrypointFromUA_SdkCliNormalization(t *testing.T) {
	disabled := false
	enabled := true

	cases := []struct {
		name string
		cfg  *config.Config
		ua   string
		want string
	}{
		{
			name: "nil cfg (default enabled) folds sdk-cli to cli",
			cfg:  nil,
			ua:   "claude-cli/2.1.63 (external, sdk-cli)",
			want: "cli",
		},
		{
			name: "zero-value cfg (default enabled) folds sdk-cli to cli",
			cfg:  &config.Config{},
			ua:   "claude-cli/2.1.63 (external, sdk-cli)",
			want: "cli",
		},
		{
			name: "real interactive cli UA is a no-op under normalization",
			cfg:  nil,
			ua:   "claude-cli/2.1.63 (external, cli)",
			want: "cli",
		},
		{
			name: "non-sdk-cli entrypoint (vscode) untouched",
			cfg:  nil,
			ua:   "claude-cli/2.1.63 (external, vscode)",
			want: "vscode",
		},
		{
			name: "explicitly enabled folds sdk-cli to cli",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &enabled}},
			ua:   "claude-cli/2.1.63 (external, sdk-cli)",
			want: "cli",
		},
		{
			name: "explicitly disabled mirrors sdk-cli verbatim (rollback path)",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}},
			ua:   "claude-cli/2.1.63 (external, sdk-cli)",
			want: "sdk-cli",
		},
		{
			name: "disabled + non-sdk-cli entrypoint still untouched",
			cfg:  &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}},
			ua:   "claude-cli/2.1.63 (external, vscode)",
			want: "vscode",
		},
		{
			name: "non-claude-code UA falls back to cli regardless of the switch",
			cfg:  nil,
			ua:   "curl/8.7.1",
			want: "cli",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEntrypointFromUA(tc.cfg, tc.ua)
			if got != tc.want {
				t.Fatalf("parseEntrypointFromUA(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}
