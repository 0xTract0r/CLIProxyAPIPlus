package executor

import (
	"bytes"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestClaudeAPIKeyExplicitUncloakPreservesCallerIdentityBytes(t *testing.T) {
	for _, raw := range []string{
		`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"claude-3-5-haiku","metadata":{"user_id":"caller_identity_literal"},"messages":[{"role":"user","content":"hello"}]}`,
	} {
		auth := &cliproxyauth.Auth{ID: "apikey-explicit-uncloak", Attributes: map[string]string{"api_key": "sk-ant-api03-private-test", "cloak_mode": "uncloak"}}
		got, cloaked, err := applyCloaking(context.Background(), &config.Config{}, auth, []byte(raw), auth.Attributes["api_key"], false, true)
		if err != nil {
			t.Fatal(err)
		}
		if cloaked || !bytes.Equal(got, []byte(raw)) {
			t.Fatalf("API-key explicit Uncloak changed caller identity/body: got %s want %s cloaked=%v", got, raw, cloaked)
		}
	}
}
