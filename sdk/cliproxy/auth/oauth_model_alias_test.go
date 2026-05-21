package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestResolveOAuthUpstreamModel_SuffixPreservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		aliases map[string][]internalconfig.OAuthModelAlias
		channel string
		input   string
		want    string
	}{
		{
			name: "numeric suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(8192)",
			want:    "gemini-2.5-pro-exp-03-25(8192)",
		},
		{
			name: "level suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(high)",
		},
		{
			name: "no suffix unchanged",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro",
			want:    "gemini-2.5-pro-exp-03-25",
		},
		{
			name: "kiro alias resolves",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"kiro": {{Name: "kiro-claude-sonnet-4-5", Alias: "sonnet"}},
			},
			channel: "kiro",
			input:   "sonnet",
			want:    "kiro-claude-sonnet-4-5",
		},
		{
			name: "config suffix takes priority",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"claude": {{Name: "claude-sonnet-4-5-20250514(low)", Alias: "claude-sonnet-4-5"}},
			},
			channel: "claude",
			input:   "claude-sonnet-4-5(high)",
			want:    "claude-sonnet-4-5-20250514(low)",
		},
		{
			name: "auto suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(auto)",
			want:    "gemini-2.5-pro-exp-03-25(auto)",
		},
		{
			name: "none suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(none)",
			want:    "gemini-2.5-pro-exp-03-25(none)",
		},
		{
			name: "github-copilot suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"github-copilot": {{Name: "claude-opus-4.6", Alias: "opus"}},
			},
			channel: "github-copilot",
			input:   "opus(medium)",
			want:    "claude-opus-4.6(medium)",
		},
		{
			name: "github-copilot no suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"github-copilot": {{Name: "claude-opus-4.6", Alias: "opus"}},
			},
			channel: "github-copilot",
			input:   "opus",
			want:    "claude-opus-4.6",
		},
		{
			name: "kimi suffix preserved",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"kimi": {{Name: "kimi-k2.5", Alias: "k2.5"}},
			},
			channel: "kimi",
			input:   "k2.5(high)",
			want:    "kimi-k2.5(high)",
		},
		{
			name: "case insensitive alias lookup with suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "Gemini-2.5-Pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(high)",
			want:    "gemini-2.5-pro-exp-03-25(high)",
		},
		{
			name: "no alias returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "unknown-model(high)",
			want:    "",
		},
		{
			name: "wrong channel returns empty",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "claude",
			input:   "gemini-2.5-pro(high)",
			want:    "",
		},
		{
			name: "empty suffix filtered out",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro()",
			want:    "gemini-2.5-pro-exp-03-25",
		},
		{
			name: "incomplete suffix treated as no suffix",
			aliases: map[string][]internalconfig.OAuthModelAlias{
				"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro(high"}},
			},
			channel: "gemini-cli",
			input:   "gemini-2.5-pro(high",
			want:    "gemini-2.5-pro-exp-03-25",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := NewManager(nil, nil, nil)
			mgr.SetConfig(&internalconfig.Config{})
			mgr.SetOAuthModelAlias(tt.aliases)

			auth := createAuthForChannel(tt.channel)
			got := mgr.resolveOAuthUpstreamModel(auth, tt.input)
			if got != tt.want {
				t.Errorf("resolveOAuthUpstreamModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func createAuthForChannel(channel string) *Auth {
	switch channel {
	case "gemini-cli":
		return &Auth{Provider: "gemini-cli"}
	case "claude":
		return &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "vertex":
		return &Auth{Provider: "vertex", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "codex":
		return &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	case "aistudio":
		return &Auth{Provider: "aistudio"}
	case "antigravity":
		return &Auth{Provider: "antigravity"}
	case "kimi":
		return &Auth{Provider: "kimi"}
	case "kiro":
		return &Auth{Provider: "kiro"}
	case "github-copilot":
		return &Auth{Provider: "github-copilot"}
	default:
		return &Auth{Provider: channel}
	}
}

func TestResolveOAuthUpstreamModel_GatesClaudeOpus1MAliasByPlan(t *testing.T) {
	aliases := map[string][]internalconfig.OAuthModelAlias{
		"claude": {
			{Name: "claude-opus-4-7", Alias: "opus[1m]", Fork: true},
			{Name: "claude-opus-4-7", Alias: "claude-opus-4-7[1m]", Fork: true},
			{Name: "claude-opus-4-6", Alias: "my-opus", Fork: true},
		},
	}

	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{
			name: "pro without credits blocks alias",
			auth: &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"}},
			want: "",
		},
		{
			name: "unknown local plan blocks alias",
			auth: &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}},
			want: "",
		},
		{
			name: "pro with credits still blocks alias",
			auth: &Auth{
				Provider: "claude",
				Attributes: map[string]string{
					"auth_kind":           "oauth",
					"plan_type":           "pro",
					"extra_usage_enabled": "true",
				},
			},
			want: "",
		},
		{
			name: "max allows alias",
			auth: &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "max"}},
			want: "claude-opus-4-7",
		},
		{
			name: "nested max profile allows alias",
			auth: &Auth{
				Provider:   "claude",
				Attributes: map[string]string{"auth_kind": "oauth"},
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"subscription": map[string]any{"has_claude_max": true},
						},
					},
				},
			},
			want: "claude-opus-4-7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager(nil, nil, nil)
			mgr.SetConfig(&internalconfig.Config{})
			mgr.SetOAuthModelAlias(aliases)

			if got := mgr.resolveOAuthUpstreamModel(tt.auth, "opus[1m]"); got != tt.want {
				t.Fatalf("resolveOAuthUpstreamModel(opus[1m]) = %q, want %q", got, tt.want)
			}
			if got := mgr.resolveOAuthUpstreamModel(tt.auth, "claude-opus-4-7[1m]"); got != tt.want {
				t.Fatalf("resolveOAuthUpstreamModel(claude-opus-4-7[1m]) = %q, want %q", got, tt.want)
			}
			wantCustomAlias := ""
			if tt.want != "" {
				wantCustomAlias = "claude-opus-4-6"
			}
			if got := mgr.resolveOAuthUpstreamModel(tt.auth, "my-opus"); got != wantCustomAlias {
				t.Fatalf("resolveOAuthUpstreamModel(my-opus) = %q, want %q", got, wantCustomAlias)
			}
			if got := mgr.resolveOAuthUpstreamModel(tt.auth, "claude-opus-4-7"); got != "" {
				t.Fatalf("base claude-opus-4-7 should not need alias resolution, got %q", got)
			}
		})
	}
}

func TestOAuthModelAliasChannel_Kimi(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("kimi", "oauth"); got != "kimi" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "kimi")
	}
}

func TestOAuthModelAliasChannel_GitHubCopilot(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("github-copilot", ""); got != "github-copilot" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "github-copilot")
	}
}

func TestOAuthModelAliasChannel_Kiro(t *testing.T) {
	t.Parallel()

	if got := OAuthModelAliasChannel("kiro", ""); got != "kiro" {
		t.Fatalf("OAuthModelAliasChannel() = %q, want %q", got, "kiro")
	}
}

func TestApplyOAuthModelAlias_SuffixPreservation(t *testing.T) {
	t.Parallel()

	aliases := map[string][]internalconfig.OAuthModelAlias{
		"gemini-cli": {{Name: "gemini-2.5-pro-exp-03-25", Alias: "gemini-2.5-pro"}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(aliases)

	auth := &Auth{ID: "test-auth-id", Provider: "gemini-cli"}

	resolvedModel := mgr.applyOAuthModelAlias(auth, "gemini-2.5-pro(8192)")
	if resolvedModel != "gemini-2.5-pro-exp-03-25(8192)" {
		t.Errorf("applyOAuthModelAlias() model = %q, want %q", resolvedModel, "gemini-2.5-pro-exp-03-25(8192)")
	}
}

func TestResolveOAuthUpstreamModel_DefaultClaude1MAliases(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{}
	cfg.SanitizeOAuthModelAlias()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.SetOAuthModelAlias(cfg.OAuthModelAlias)

	auth := &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "max"}}
	tests := map[string]string{
		"sonnet[1m]":            "claude-sonnet-4-6",
		"opus[1m]":              "claude-opus-4-7",
		"claude-sonnet-4-6[1m]": "claude-sonnet-4-6",
		"claude-opus-4-7[1m]":   "claude-opus-4-7",
		"claude-opus-4-6[1m]":   "claude-opus-4-6",
	}
	for input, want := range tests {
		if got := mgr.resolveOAuthUpstreamModel(auth, input); got != want {
			t.Fatalf("resolveOAuthUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}
