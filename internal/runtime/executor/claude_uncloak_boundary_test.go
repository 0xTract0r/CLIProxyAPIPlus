package executor

import (
	"bytes"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"testing"
)

func TestClaudeUncloakRetainsForkDeviceIdentityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, mode       string
		native, override bool
	}{
		{name: "default auto"}, {name: "never", mode: "never"},
		{name: "native", native: true}, {name: "always", mode: "always"},
		{name: "configured always overrides uncloak", mode: "uncloak", override: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "key-uncloak-boundary"
			auth := &cliproxyauth.Auth{ID: "uncloak-boundary-" + tc.name, Attributes: map[string]string{"api_key": key}}
			if tc.mode != "" {
				auth.Attributes["cloak_mode"] = tc.mode
			}
			cfg := &config.Config{}
			if tc.override {
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: key, Cloak: &config.CloakConfig{Mode: "always"}}}
			}
			policy, _ := resolveClaudeWirePolicy(cfg, auth, key, tc.native)
			if policy.Uncloak {
				t.Fatal("fork mode unexpectedly became Uncloak")
			}
			if tc.override && !policy.Cloak {
				t.Fatal("configured always lost precedence")
			}
			raw := []byte(`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hello"}]}`)
			got, _, err := applyCloaking(context.Background(), cfg, auth, raw, key, tc.native, true)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(got, raw) || gjson.GetBytes(got, "metadata.user_id").String() == "" {
				t.Fatal("fork mode lost account device identity injection")
			}
		})
	}
}
