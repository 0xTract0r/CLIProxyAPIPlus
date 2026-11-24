package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

func TestOrdinaryBuiltInProviderCatalogs(t *testing.T) {
	for _, catalog := range []struct {
		provider, model string
		metadata        map[string]any
	}{
		{"kilo", "kilo-auto/efficient", nil},
		{"gitlab", "gitlab-code-suggestions", map[string]any{"model_name": "fixture-gitlab-model"}},
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

func TestOrdinaryPluginCatalogPreservesSubscription(t *testing.T) {
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
}

func TestOrdinaryExecutorRegistrationDistinguishesRetiredProviders(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	s := &Service{cfg: &config.Config{}, coreManager: m}
	for _, provider := range []string{"gemini-cli", "github-copilot", "kilo", "gitlab"} {
		s.registerExecutorForAuth(&coreauth.Auth{ID: "fixture-" + provider, Provider: provider}, false)
		if got, ok := m.Executor(provider); !ok || got.Identifier() != provider {
			t.Fatalf("ordinary built-in %q was not registered", provider)
		}
	}
	// These registrations are local refusal handlers, not inference transports.
	for _, provider := range []string{"gemini-cli", "github-copilot"} {
		executor, _ := m.Executor(provider)
		_, err := executor.Execute(t.Context(), &coreauth.Auth{Provider: provider}, coreexec.Request{}, coreexec.Options{})
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusNotImplemented {
			t.Fatalf("retired provider %q should terminate locally with 501: %v", provider, err)
		}
	}
	s.registerExecutorForAuth(&coreauth.Auth{ID: "fixture-kiro", Provider: "kiro"}, false)
	if _, ok := m.Executor("kiro"); ok {
		t.Fatal("local Kiro catalog must not register an executor without a declared endpoint")
	}
}

func TestOrdinaryRetiredFallbackNotInPublicCatalog(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{}
	s := &Service{cfg: cfg, coreManager: m}
	reg := registry.GetGlobalRegistry()
	for provider, model := range map[string]string{"gemini-cli": "gemini-2.5-pro", "github-copilot": "gpt-4o"} {
		auth, err := m.Register(t.Context(), &coreauth.Auth{ID: "fixture-retired-" + provider, Provider: provider, Status: coreauth.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		s.ensureExecutorsForAuth(auth)
		reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		if !s.registerModelsForAuth(t.Context(), auth) || len(reg.GetModelsForClient(auth.ID)) != 0 {
			t.Fatalf("stale fallback published: %s", provider)
		}
	}
	router := gin.New()
	router.GET("/v1/models", openai.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m)).OpenAIModels)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if resp.Code != 200 || strings.Contains(resp.Body.String(), "gemini-2.5-pro") || strings.Contains(resp.Body.String(), "gpt-4o") {
		t.Fatal("retired-only model visible through /v1/models")
	}
}

type ordinaryExternalLegacyProviderExecutor struct{ coreauth.ProviderExecutor }

func (e *ordinaryExternalLegacyProviderExecutor) Unwrap() coreauth.ProviderExecutor {
	return e.ProviderExecutor
}

func (*ordinaryExternalLegacyProviderExecutor) Execute(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{Payload: []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture SDK result"}}]}`)}, nil
}

func TestOrdinaryExternallyImplementedLegacyProviderKeepsExecutableCatalog(t *testing.T) {
	const provider = "gemini-cli"
	const id = "fixture-external-sdk"
	const builtinModel = "gemini-2.5-pro"
	const pluginModel = "fixture-explicit-static"
	const independentClient = "fixture-independent-catalog-client"
	const independentModel = "fixture-independent-catalog-model"
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	host := pluginhost.New()
	s := &Service{
		cfg: cfg, coreManager: manager, pluginHost: host, externalCoreManager: true,
		executorWrapper: func(name string, executor coreauth.ProviderExecutor) coreauth.ProviderExecutor {
			if name != provider {
				return executor
			}
			return &ordinaryExternalLegacyProviderExecutor{ProviderExecutor: executor}
		},
	}
	if _, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	catalog := issue7512Plugin{provider: provider, models: []pluginapi.ModelInfo{{ID: pluginModel}}}
	host.RegisterPluginForTest("fixture-static-catalog", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		ModelProvider: catalog, Executor: catalog, ExecutorModelScope: pluginapi.ExecutorModelScopeStatic,
		ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"},
	}})
	reg := GlobalModelRegistry()
	t.Cleanup(func() {
		reg.UnregisterClient(id)
		reg.UnregisterClient(independentClient)
		reg.UnregisterClient("plugin:fixture-static-catalog:gemini-cli:executor")
	})
	reg.RegisterClient(id, provider, []*ModelInfo{{ID: builtinModel}, {ID: pluginModel}})
	reg.RegisterClient(independentClient, provider, []*ModelInfo{{ID: independentModel}})
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	api := openai.NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", api.OpenAIModels)

	s.syncPluginModelRuntime(t.Context())
	executor, ok := manager.Executor(provider)
	if _, owns := executor.(*ordinaryExternalLegacyProviderExecutor); !ok || !owns {
		t.Fatal("the explicitly installed SDK executor did not retain ownership")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, independentModel) {
		t.Fatalf("unrelated registered client unexpectedly changed: status=%d", response.Code)
	}
	for _, model := range []string{builtinModel, pluginModel} {
		if !strings.Contains(body, model) || !reg.ClientSupportsModel(id, model) {
			t.Errorf("existing catalog lost SDK-executable model %s", model)
			continue
		}
		out, _, failure := base.ExecuteWithAuthManager(t.Context(), "openai", model, []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`), "")
		if failure != nil || !strings.Contains(string(out), "fixture SDK result") {
			t.Errorf("model %s did not execute through the SDK implementation: %v", model, failure)
		}
	}
}
