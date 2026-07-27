package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const dangerousDirectHeaderName = "Anthropic-Dangerous-Direct-Browser-Access"

// TestApplyClaudeHeaders_DangerousDirect_OAuthGatedByReplayFlag pins the Phase 7.2
// header-set fix: real claude-cli 2.1.220 sends
// Anthropic-Dangerous-Direct-Browser-Access on /v1/messages to the first-party
// api.anthropic.com endpoint in BOTH x-api-key and OAuth/Bearer mode (first-party
// ground truth: header-order-probe/COMPARISON-firstparty.md, 5/5 captures). CPA
// historically sent it only in x-api-key mode. It is now also emitted in OAuth
// mode, but gated behind replay-wire-header-order so gate-off preserves the exact
// historical header set. x-api-key mode is unchanged (always sends it).
func TestApplyClaudeHeaders_DangerousDirect_OAuthGatedByReplayFlag(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := false
	tru := true
	fal := false

	newCfg := func(replay *bool) *config.Config {
		return &config.Config{
			ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
				StabilizeDeviceProfile: &stabilize,
				ReplayWireHeaderOrder:  replay,
			},
		}
	}
	// OAuth/Bearer mode: no api_key attribute => useAPIKey=false.
	oauthAuth := func() *cliproxyauth.Auth {
		return &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-oauth"}
	}
	// x-api-key mode: api_key attribute present => useAPIKey=true.
	apiKeyAuth := func() *cliproxyauth.Auth {
		return &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-apikey",
			Attributes: map[string]string{"api_key": "key-x"}}
	}

	cases := []struct {
		name string
		auth *cliproxyauth.Auth
		key  string
		cfg  *config.Config
		want bool
	}{
		{"oauth + flag nil (default off) => absent", oauthAuth(), "tok", newCfg(nil), false},
		{"oauth + flag false => absent", oauthAuth(), "tok", newCfg(&fal), false},
		{"oauth + flag on => present", oauthAuth(), "tok", newCfg(&tru), true},
		{"apikey + flag nil => present (unchanged)", apiKeyAuth(), "key-x", newCfg(nil), true},
		{"apikey + flag on => present", apiKeyAuth(), "key-x", newCfg(&tru), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newClaudeHeaderTestRequest(t, http.Header{})
			applyClaudeHeaders(req, tc.auth, tc.key, false, nil, tc.cfg)
			got := req.Header.Get(dangerousDirectHeaderName) == "true"
			if got != tc.want {
				t.Fatalf("dangerous-direct present = %v, want %v (headers: %v)",
					got, tc.want, req.Header.Get(dangerousDirectHeaderName))
			}
		})
	}
}
