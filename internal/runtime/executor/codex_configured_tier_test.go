package executor

import (
	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestOrdinaryConfiguredCodexTier(t *testing.T) {
	a := &coreauth.Auth{Metadata: map[string]any{"fast_models": []any{" authorized "}}}
	for _, tc := range []struct{ model, body, tier string }{
		{"authorized", "{}", "priority"},
		{"other", "{}", ""},
		{"authorized", `{"service_tier":"default"}`, "default"},
		{"other", `{"service_tier":"priority"}`, "priority"},
	} {
		doc := ojson.NewDocString(tc.body)
		applyConfiguredCodexTier(doc, a, tc.model)
		if got := doc.Get("service_tier").String(); got != tc.tier {
			t.Errorf("model %s, body %s: want %q, got %q", tc.model, tc.body, tc.tier, got)
		}
	}
}
