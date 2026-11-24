package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryDevinServicePublishesNativeAndExactAlias(t *testing.T) {
	const native = "devin/swe-1-6-slow"
	const alias = "ordinary-devin-registered(high)"
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{"devin": {
		{Name: native, Alias: alias, Fork: true},
	}}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.SetOAuthModelAlias(cfg.OAuthModelAlias)
	s := &Service{cfg: cfg, coreManager: m}
	a := &coreauth.Auth{ID: t.Name(), Provider: "devin", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
	if _, err := m.Register(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	r := GlobalModelRegistry()
	t.Cleanup(func() { r.UnregisterClient(a.ID) })
	s.registerModelsForAuth(t.Context(), a)
	if !r.ClientSupportsModel(a.ID, native) || !r.ClientSupportsModel(a.ID, alias) {
		t.Fatal("ordinary catalog or exact alias missing")
	}
	if got := m.ResolveExecutionModel(a, alias); got != native+"(high)" {
		t.Errorf("registered alias resolved to %q", got)
	}
}
