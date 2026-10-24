package executor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fixtureAuth builds a cloaked direct-Anthropic API-key auth for header tests.
// The key value is a placeholder that never reaches a real upstream.
func fixtureAuth() *cliproxyauth.Auth {
	attrs := map[string]string{}
	attrs[cliproxyauth.AttributeAPIKey] = strings.Join([]string{"test", "fixture", "key"}, "-")
	attrs["fingerprint_profile"] = "claude-code-cli"
	return &cliproxyauth.Auth{Attributes: attrs}
}

// A caller the cloak does not recognize (a Claude Code newer than the pinned
// profile) keeps betas the proxy does not manage: dropping them fails whole
// turns whose features need the missing authorization (#5738). per-turn-control
// is now assembled for fable-5-1, so this uses a beta that is still unmanaged.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "message-threads-2026-08-12")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-fable-5-1"}`), claudeBeta280Config375(), incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(betas, "message-threads-2026-08-12") {
		t.Fatalf("Anthropic-Beta = %q, want unmanaged caller beta forwarded", betas)
	}
	if !strings.Contains(betas, "mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,mid-conversation-system-clear-at-2026-08-21,effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want fable-5-1 per-turn-control between mid-conversation-system and tool-changes", betas)
	}
}

// Managed betas stay governed by the assembled baseline on direct Anthropic:
// one whose gating excludes the request (effort on a Haiku model) is not
// reinstated just because the caller asked for it.
func TestApplyClaudeHeaders_StillGatesManagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "effort-2025-11-24")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-haiku-4-5"}`), claudeBeta280Config375(), incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if betas := req.Header.Get("Anthropic-Beta"); strings.Contains(betas, "effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want gated effort beta kept off the Haiku request", betas)
	}
}

func TestApplyClaudeHeaders_PreservesNativeGatewayHintsOnly(t *testing.T) {
	for _, tt := range []struct {
		name         string
		requestClass string
		confirmed    bool
	}{
		{name: "confirmed main", requestClass: "main", confirmed: true},
		{name: "confirmed auxiliary", requestClass: "auxiliary", confirmed: true},
		{name: "unconfirmed caller", requestClass: "main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			incoming := http.Header{
				"x-claude-code-request-class":       {tt.requestClass},
				"x-claude-code-agent-type":          {"builtin"},
				"x-claude-code-prev-tool-durations": {"5"},
				"x-claude-code-compaction":          {"false"},
				"x-claude-code-context-compacted":   {"false"},
			}
			auth := fixtureAuth()
			if err := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil,
				[]byte(`{"model":"claude-opus-5-5"}`), &config.Config{}, incoming, tt.confirmed); err != nil {
				t.Fatal(err)
			}
			for key, values := range incoming {
				got := req.Header.Get(key)
				if tt.confirmed && got != values[0] {
					t.Errorf("%s = %q, want %q", key, got, values[0])
				}
				if !tt.confirmed && got != "" {
					t.Errorf("unconfirmed caller forwarded %s = %q", key, got)
				}
			}
		})
	}
}

// TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth verifies the exact
// scenario reported in #5738: an OAuth credential with an unconfirmed client
// sending a per-turn effort directive turn.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{
		ID:       "claude-oauth-fixture",
		Metadata: map[string]any{"access_token": "sk-ant-oat-fixture"},
	}
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "message-threads-2026-08-12,mid-conversation-system-2026-04-07")
	body := []byte(`{
		"model": "claude-fable-5-1",
		"max_tokens": 16,
		"messages": [
			{"role": "user",   "content": "hi"},
			{"role": "system", "content": [], "output_config": {"effort": "low"}},
			{"role": "user",   "content": "say ok"}
		]
	}`)
	if errHeaders := applyClaudeHeaders(req, auth, "sk-ant-oat-fixture", false, nil, body, claudeBeta280Config375(), incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(betas, "message-threads-2026-08-12") {
		t.Fatalf("Anthropic-Beta = %q, want unmanaged caller beta forwarded on OAuth", betas)
	}
	if !strings.Contains(betas, "mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01") {
		t.Fatalf("Anthropic-Beta = %q, want fable-5-1 per-turn-control assembled", betas)
	}
}

// 明确选择U的280 beta模型门；无caller UA/农场身份，不把软件pin解释为强盖caller UA。
func claudeBeta280Config375() *config.Config {
	return &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		UserAgent: "claude-cli/2.1.280 (external, cli)", PackageVersion: "0.112.1", RuntimeVersion: "v26.3.0", PinSoftwareBaseline: true,
	}}
}

// F request.go:540-545 的固定floor+caller并集，和SD15的显式uncloak分开断言。
func TestClaudeBeta375ForkDefaultAndExplicitCallerOwnership(t *testing.T) {
	const floor = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07"
	const feature = "caller-feature-2099-01-01"
	for _, mode := range []string{"", "uncloak"} {
		t.Run("mode="+mode, func(t *testing.T) {
			incoming := http.Header{"Anthropic-Beta": {feature + ",effort-2025-11-24,claude-code-20250219"}}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-default-beta-375", "cloak_mode": mode}}
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyClaudeHeaders(req, auth, "key-default-beta-375", false, nil, []byte(`{"model":"claude-haiku-4-5"}`), &config.Config{}, incoming, false); err != nil {
				t.Fatal(err)
			}
			want := floor + "," + feature + ",effort-2025-11-24"
			if mode == "uncloak" {
				want = feature
			}
			if got := req.Header.Get("Anthropic-Beta"); got != want {
				t.Fatalf("mode=%q beta=%q want=%q", mode, got, want)
			}
			if req.Header.Get("x-api-key") != "key-default-beta-375" || req.Header.Get("Authorization") != "" {
				t.Fatal("API-key authority changed")
			}
		})
	}
}
