package executor

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// ginContextWithUA builds a context carrying a gin request with the given
// User-Agent so applyCloaking's ShouldCloak gate can be exercised.
func ginContextWithUA(userAgent string) context.Context {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = req
	//nolint:staticcheck // applyCloaking reads the gin context via the "gin" string key.
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func normalizeEnvTestPayload() []byte {
	return []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "You are Claude.\n<env>\nWorking directory: /Users/realdev/Project/secret\n</env>"}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)
}

// TestApplyCloaking_NormalizeAccountEnvSwitchOff covers P2.A7.4: with the global
// switch off the request body is left byte-for-byte unchanged (zero behavior
// change / safe gray rollout).
func TestApplyCloaking_NormalizeAccountEnvSwitchOff(t *testing.T) {
	resetClaudeDeviceProfileCache()
	cfg := &config.Config{} // NormalizeAccountEnv unset -> default off
	auth := &cliproxyauth.Auth{ID: "acct-off"}
	payload := normalizeEnvTestPayload()

	// Use a claude-cli UA so the broader cloak transforms are gated out and we
	// observe only the env-normalization decision. The device_id rewrite still
	// runs, so compare the system text specifically rather than the whole body.
	out := applyCloaking(ginContextWithUA("claude-cli/2.1.70 (external, cli)"), cfg, auth, payload, "claude-sonnet-4-5", "key-off", "")

	got := gjson.GetBytes(out, "system.0.text").String()
	if !strings.Contains(got, "/Users/realdev/Project/secret") {
		t.Fatalf("switch off must not normalize cwd, got %q", got)
	}
}

// TestApplyCloaking_NormalizeAccountEnvSwitchOnRealClaudeCli covers P2.A7.3 +
// P2.A7.4: with the switch on, env normalization applies even for real claude-cli
// (ShouldCloak == false), proving it runs before the ShouldCloak gate.
func TestApplyCloaking_NormalizeAccountEnvSwitchOnRealClaudeCli(t *testing.T) {
	resetClaudeDeviceProfileCache()
	on := true
	cfg := &config.Config{NormalizeAccountEnv: &on}
	auth := &cliproxyauth.Auth{ID: "acct-on"}
	payload := normalizeEnvTestPayload()

	out := applyCloaking(ginContextWithUA("claude-cli/2.1.70 (external, cli)"), cfg, auth, payload, "claude-sonnet-4-5", "key-on", "")

	got := gjson.GetBytes(out, "system.0.text").String()
	canonical := helps.AccountCanonicalCwd(auth, "key-on")
	if strings.Contains(got, "/Users/realdev") {
		t.Fatalf("switch on must normalize cwd for claude-cli, got %q", got)
	}
	if !strings.Contains(got, canonical) {
		t.Fatalf("expected canonical %q in %q", canonical, got)
	}
}
