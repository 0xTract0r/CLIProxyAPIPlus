package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestOrdinaryModelRegistrationFencesModelAliasAndSubscription(t *testing.T) {
	const id = "synthetic-codex-catalog"
	const spark = "gpt-5.3-codex-spark"
	auth := &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{"plan_type": "plus"}}
	sourceModel := ""
	for _, m := range registry.GetCodexPlusModels() {
		if m != nil && m.ID != "" && m.ID != spark {
			sourceModel = m.ID
			break
		}
	}
	if sourceModel == "" {
		t.Fatal("fixture lacks ordinary eligible model")
	}
	r := GlobalModelRegistry()
	t.Cleanup(func() { r.UnregisterClient(id) })
	cfg := &config.Config{}
	cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{"codex": {{Name: sourceModel, Alias: spark, Fork: true}}}
	s := &Service{cfg: cfg}
	if !s.registerModelsForAuth(context.Background(), auth) {
		t.Fatal("first registration failed")
	}
	if r.ClientRegistrationEpoch(id) == 0 || !r.ClientSupportsModel(id, sourceModel) || r.ClientSupportsModel(id, spark) {
		t.Fatalf("alias evaded documented qualification: epoch=%d", r.ClientRegistrationEpoch(id))
	}
	before := r.ClientRegistrationEpoch(id)
	next := cfg.CloneForRuntime()
	next.OAuthModelAlias = map[string][]config.OAuthModelAlias{"codex": {{Name: sourceModel, Alias: "fixture-public-model", Fork: false}}}
	s.cfg = next
	if !s.registerModelsForAuth(context.Background(), auth) {
		t.Fatal("second registration failed")
	}
	if r.ClientRegistrationEpoch(id) <= before || !r.ClientSupportsModel(id, "fixture-public-model") || r.ClientSupportsModel(id, sourceModel) {
		t.Fatalf("alias update did not replace prior snapshot: before=%d after=%d", before, r.ClientRegistrationEpoch(id))
	}
}

func TestOrdinaryStaticAndCachedProviderCatalogs(t *testing.T) {
	for _, catalog := range []struct {
		provider, model string
		metadata        map[string]any
	}{
		{"gemini-cli", "gemini-2.5-pro", nil},
		{"kilo", "kilo/auto", nil},
		{"github-copilot", "gpt-4o", nil},
		{"gitlab", "fixture-gitlab-model", map[string]any{"model_name": "fixture-gitlab-model"}},
	} {
		t.Run(catalog.provider, func(t *testing.T) {
			id := "synthetic-catalog-" + catalog.provider
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
			s := &Service{cfg: &config.Config{}}
			a := &coreauth.Auth{ID: id, Provider: catalog.provider, Metadata: catalog.metadata}
			if !s.registerModelsForAuth(context.Background(), a) || !GlobalModelRegistry().ClientSupportsModel(id, catalog.model) {
				t.Fatalf("missing %q in local catalog", catalog.model)
			}
		})
	}
}

func TestOrdinaryPluginCatalogPreservesSubscriptionAndHasNoImplicitExecutor(t *testing.T) {
	const spark = "gpt-5.3-codex-spark"
	const id = "synthetic-plugin-catalog"
	const standard = "fixture-standard"
	r := GlobalModelRegistry()
	t.Cleanup(func() { r.UnregisterClient(id) })
	p := issue7512Plugin{provider: "codex", models: []pluginapi.ModelInfo{{ID: standard}, {ID: spark}}}
	h := pluginhost.New()
	h.RegisterPluginForTest("ordinary-catalog-test", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: p, ModelProvider: p}})
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: standard, Alias: spark, Fork: true}}}}
	m := coreauth.NewManager(nil, nil, nil)
	s := &Service{cfg: cfg, pluginHost: h, coreManager: m}
	a, err := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}, Metadata: map[string]any{"plan_type": "plus"}})
	if err != nil {
		t.Fatal(err)
	}
	handled, published := s.tryRegisterPluginModelsForAuthWithConfig(context.Background(), cfg, a, "codex", coreauth.AuthKindOAuth, nil)
	if !handled || !published || !r.ClientSupportsModel(id, standard) || r.ClientSupportsModel(id, spark) {
		t.Fatalf("unexpected model publication: handled=%v published=%v", handled, published)
	}
	for _, provider := range []string{"gemini-cli", "github-copilot", "kilo", "gitlab", "kiro"} {
		s.registerExecutorForAuth(&coreauth.Auth{ID: "fixture", Provider: provider}, false)
		if _, ok := m.Executor(provider); ok {
			t.Fatalf("provider %q gained an executor without a declared endpoint", provider)
		}
	}
}
