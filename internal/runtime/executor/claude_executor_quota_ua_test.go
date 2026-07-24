package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// quotaAuthWithClaudeHighWaterUA seeds an *Auth whose persisted
// claude_device_high_water triple carries the given outbound User-Agent, so
// claudeFallbackBaseline surfaces it as the resolved device-profile UA that
// PrepareRequest applies to the outgoing quota/oauth request. The version encoded
// in the UA (2.1.215) is above the frozen floor (2.1.63), so it is picked as the
// ceiling; the executor is built without an auth manager so PrepareRequest's
// persistClaudeDeviceHighWater is a no-op and the seeded high-water is stable.
func quotaAuthWithClaudeHighWaterUA(ua string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "sk-ant-oat-quota-test",
		},
		Metadata: map[string]any{
			"type": "claude",
			cliproxyauth.ClaudeDeviceHighWaterMetadataKey: map[string]any{
				"user_agent":      ua,
				"version":         "2.1.215",
				"package_version": "0.1.0",
				"runtime_version": "v22.0.0",
			},
		},
	}
}

// TestClaudeExecutorPrepareRequest_FoldsQuotaHighWaterUserAgentSuffix pins the
// third account-level OAuth token-endpoint egress path: the background quota/oauth
// snapshot lookups (GET /api/oauth/profile and /api/oauth/usage, reaching here via
// quota_snapshots.go fetchQuotaJSON -> exec.HttpRequest -> ClaudeExecutor.PrepareRequest).
//
// Before this fix PrepareRequest applied the account's device-profile high-water
// User-Agent verbatim (ApplyClaudeDeviceProfileHeaders) without the suffix fold
// serving does via AlignClaudeDeviceProfileUserAgentSuffix. An account whose
// frozen high-water suffix was seeded to "(external, sdk-cli)" (a single
// `claude --print` can do this) therefore egressed "(external, sdk-cli)" on quota
// probes while its serving/refresh/reauth paths all present "(external, cli)" — a
// UA/entrypoint divergence real claude-code never emits and Anthropic can detect.
//
// This drives the real PrepareRequest path on-wire (stabilize on, sdk-cli
// high-water seeded into auth.Metadata) and asserts the resolved outbound
// User-Agent header suffix is folded to "(external, cli)" under the default and
// explicitly-enabled gate while the high-water version (2.1.215) is preserved, and
// is left "(external, sdk-cli)" verbatim when the normalize gate is disabled. The
// fold reuses helps.NormalizeClaudeUserAgentEntrypoint — the same in-place fold the
// refresh/reauth token-endpoint paths use — gated by the same
// config.NormalizeSdkCliEntrypointEnabled switch as serving's alignment.
func TestClaudeExecutorPrepareRequest_FoldsQuotaHighWaterUserAgentSuffix(t *testing.T) {
	stabilize := true
	enabled := true
	disabled := false

	cases := []struct {
		name          string
		normalizeGate *bool // nil => documented default (enabled)
		wantUA        string
	}{
		{
			name:          "default gate folds sdk-cli high-water to cli",
			normalizeGate: nil,
			wantUA:        "claude-cli/2.1.215 (external, cli)",
		},
		{
			name:          "explicitly enabled gate folds sdk-cli high-water to cli",
			normalizeGate: &enabled,
			wantUA:        "claude-cli/2.1.215 (external, cli)",
		},
		{
			name:          "disabled gate keeps sdk-cli high-water verbatim",
			normalizeGate: &disabled,
			wantUA:        "claude-cli/2.1.215 (external, sdk-cli)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the shared observation/high-water cache so a prior test's
			// global observation cannot outrank the seeded persisted high-water.
			resetClaudeDeviceProfileCache()

			cfg := &config.Config{
				ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
					StabilizeDeviceProfile: &stabilize,
				},
				Claude: config.ClaudeConfig{
					NormalizeSdkCliEntrypoint: tc.normalizeGate,
				},
			}
			executor := NewClaudeExecutor(cfg)
			auth := quotaAuthWithClaudeHighWaterUA("claude-cli/2.1.215 (external, sdk-cli)")

			// Mirrors quota_snapshots.go fetchQuotaJSON: a bare GET to the
			// first-party oauth profile endpoint with no inbound client UA.
			req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}

			if err := executor.PrepareRequest(req, auth); err != nil {
				t.Fatalf("PrepareRequest() error = %v", err)
			}

			if got := req.Header.Get("User-Agent"); got != tc.wantUA {
				t.Fatalf("quota-path outbound User-Agent = %q, want %q", got, tc.wantUA)
			}
		})
	}
}
