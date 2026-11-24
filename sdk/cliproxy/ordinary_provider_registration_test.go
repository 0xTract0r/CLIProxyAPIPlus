package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaiapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryProvidersRegisterNativeWithoutPlugin(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	transport := &nativeProviderFixtureTransport{}
	service.coreManager.SetConfig(service.cfg)
	service.coreManager.SetRoundTripperProvider(transport)
	for _, tc := range []struct{ provider, model string }{
		{"gitlab", executor.GitLabCodeSuggestionsModel},
		{"kilo", "kilo-auto/efficient"},
	} {
		auth := &coreauth.Auth{ID: "native-fixture-" + tc.provider, Provider: tc.provider, Status: coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributeSourceBackend: coreauth.AuthSourceFile}}
		if tc.provider == "gitlab" {
			auth.Metadata = map[string]any{"base_url": "https://gitlab.example.invalid", "access_token": "fixture-gitlab-key"}
		} else {
			auth.Metadata = map[string]any{"kilocodeToken": "fixture-kilo-device"}
		}
		service.ensureExecutorsForAuth(auth)
		got, ok := service.coreManager.Executor(tc.provider)
		if !ok {
			t.Fatalf("native executor missing for %s", tc.provider)
		}
		switch got.(type) {
		case *executor.GitLabExecutor:
			if tc.provider != "gitlab" {
				t.Fatal("wrong GitLab executor registered")
			}
		case *executor.KiloExecutor:
			if tc.provider != "kilo" {
				t.Fatal("wrong Kilo executor registered")
			}
		default:
			t.Fatalf("%s fell into generic or plugin executor %T", tc.provider, got)
		}
		current, err := service.coreManager.Register(context.Background(), auth)
		if err != nil {
			t.Fatal(err)
		}
		if !service.registerModelsForAuth(context.Background(), current) {
			t.Fatalf("model registration failed for %s", tc.provider)
		}
		r := registry.GetGlobalRegistry()
		t.Cleanup(func() { r.UnregisterClient(auth.ID) })
		models := r.GetModelsForClient(auth.ID)
		if len(models) != 1 || models[0].ID != tc.model {
			t.Fatalf("%s models = %#v, expected %s", tc.provider, models, tc.model)
		}
		service.coreManager.RefreshSchedulerEntry(auth.ID)
	}

	handler := openaiapi.NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&service.cfg.SDKConfig, service.coreManager))
	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletions)
	for _, fixture := range []struct{ request, response string }{
		{`{"model":"gitlab-code-suggestions","current_file":{"file_name":"fixture.go","content_above_cursor":"package fixture"}}`, "gitlab result"},
		{`{"model":"kilo-auto/efficient","messages":[{"role":"user","content":"synthetic request"}]}`, "kilo result"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fixture.request))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), fixture.response) {
			t.Fatalf("native HTTP route: status %d, body %s", out.Code, out.Body.String())
		}
	}
	if transport.calls != 2 {
		t.Fatalf("expected one supplier call per provider, got %d", transport.calls)
	}
}

type nativeProviderFixtureTransport struct{ calls int }

func (f *nativeProviderFixtureTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return f }

func (f *nativeProviderFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	switch req.URL.Host {
	case "gitlab.example.invalid":
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"text":"gitlab result"}]}`))}, nil
	case "api.kilo.ai":
		if req.URL.Path != "/api/gateway/chat/completions" || req.Header.Get("Authorization") != "Bearer fixture-kilo-device" {
			return nil, &unsupportedFixtureHost{host: "unexpected Kilo grant scope"}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion","model":"kilo-auto/efficient","choices":[{"index":0,"message":{"role":"assistant","content":"kilo result"},"finish_reason":"stop"}]}`))}, nil
	}
	return nil, &unsupportedFixtureHost{host: req.URL.Host}
}

type unsupportedFixtureHost struct{ host string }

func (e *unsupportedFixtureHost) Error() string { return "unexpected fixture destination: " + e.host }
