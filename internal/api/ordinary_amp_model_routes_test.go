package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestOrdinaryAmpModelRoutes(t *testing.T) {
	models := registry.GetGlobalRegistry()
	const model = "synthetic-amp-directory-model"
	models.RegisterClient("synthetic-amp-directory-client", "gemini", []*registry.ModelInfo{
		{ID: model, Object: "model", DisplayName: "Synthetic amp", OwnedBy: "fixture", Type: "gemini"},
	})
	t.Cleanup(func() { models.UnregisterClient("synthetic-amp-directory-client") })
	previous := home.Current()
	home.ClearCurrent()
	t.Cleanup(func() { home.SetCurrent(previous) })
	config := func(enabled bool) *proxyconfig.Config {
		return &proxyconfig.Config{
			SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}},
			Home:      proxyconfig.HomeConfig{Enabled: enabled},
		}
	}
	server := newTestServerWithConfig(t, config(false))
	homeServer := newTestServerWithConfig(t, config(true))
	call := func(s *Server, method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		// The Amp path must decide the output format even when an
		// optional client header would choose a different format on /v1.
		req.Header.Set("Anthropic-Version", "2023-06-01")
		rr := httptest.NewRecorder()
		s.engine.ServeHTTP(rr, req)
		return rr
	}
	for _, item := range []struct{ path, field string }{
		{"/unknown/models", "object"},
		{"/openai/v1/models", "object"},
		{"/Anthropic/models", "data"},
		{"/google/models", "models"},
		{"/google/v1/models", "models"},
		{"/openai/v1beta/models", "models"},
		{"/google/v1beta/models/" + model, "name"},
	} {
		t.Run(item.path, func(t *testing.T) {
			path := "/api/provider" + item.path
			for _, key := range []string{"", "wrong-key"} {
				r := call(server, http.MethodGet, path, key)
				if r.Code != http.StatusUnauthorized || strings.Contains(r.Body.String(), model) {
					t.Errorf("unauthorized request status=%d", r.Code)
				}
			}
			r := call(server, http.MethodGet, path, "test-key")
			var body map[string]any
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil || r.Code != http.StatusOK || body[item.field] == nil {
				t.Fatalf("status=%d field=%s body=%s err=%v", r.Code, item.field, r.Body.String(), err)
			}
			switch item.field {
			case "object":
				if body["object"] != "list" || strings.Contains(r.Body.String(), "displayName") {
					t.Error("invalid default list payload")
				}
			case "models", "name":
				if !strings.Contains(r.Body.String(), "models/"+model) {
					t.Error("registered synthetic model missing")
				}
			case "data":
				if body["object"] == "list" {
					t.Error("Anthropic route returned OpenAI list")
				}
			}
			h := call(homeServer, http.MethodGet, path, "test-key")
			if h.Code != http.StatusServiceUnavailable || strings.Contains(h.Body.String(), model) {
				t.Errorf("Home failure exposed local catalog or changed status: %d %s", h.Code, h.Body.String())
			}
		})
	}
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/api/provider/google/v1beta/models/missing"},
		{http.MethodPost, "/api/provider/openai/v1/models"},
		{http.MethodPost, "/api/provider/openai/v1/chat/completions"},
	} {
		if got := call(server, item.method, item.path, "test-key").Code; got != http.StatusNotFound {
			t.Errorf("%s %s: status=%d", item.method, item.path, got)
		}
	}
	t.Run("response interception", func(t *testing.T) {
		called := false
		server.handlers.SetPluginHost(&mockModelListInterceptorHost{
			interceptResponse: func(_ context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
				called = true
				if req.Kind != pluginapi.ResponseInterceptKindModelList || req.SourceFormat != "openai" ||
					!strings.Contains(string(req.Body), model) {
					t.Errorf("unexpected response interceptor input: kind=%q format=%q", req.Kind, req.SourceFormat)
				}
				return pluginapi.ResponseInterceptResponse{
					Headers: req.ResponseHeaders,
					Body:    []byte(`{"object":"list","data":[]}`),
				}
			},
		})
		r := call(server, http.MethodGet, "/api/provider/openai/models", "test-key")
		if !called || r.Code != http.StatusOK || strings.Contains(r.Body.String(), model) {
			t.Fatalf("response filter did not take effect: called=%t status=%d", called, r.Code)
		}
	})

}

func TestOrdinaryAmpConnectedHomeCatalogWithServerFixture(t *testing.T) {
	const remote = "synthetic-amp-home-private"
	const local = "synthetic-amp-local-only"
	client := newHomeCatalogClient(t, `{"models":[{"id":"synthetic-amp-home-private","object":"model","owned_by":"fixture"}]}`)
	previous := home.Current()
	home.SetCurrent(client)
	t.Cleanup(func() { home.SetCurrent(previous) })
	models := registry.GetGlobalRegistry()
	models.RegisterClient(local, "openai", []*registry.ModelInfo{{ID: local, Object: "model", Type: "openai"}})
	t.Cleanup(func() { models.UnregisterClient(local) })

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}},
		Home:      proxyconfig.HomeConfig{Enabled: true},
	}
	cfg.ClaudeCode.DisableCloakingModelList = true
	server := newTestServerWithConfig(t, cfg)
	// Route through the production registration and access middleware, as the
	// catalog fixture only simulates GET and does not run a heartbeat lifecycle.
	server.engine = gin.New()
	server.registerAmpModelRoutes(openai.NewOpenAIAPIHandler(server.handlers),
		claude.NewClaudeCodeAPIHandler(server.handlers), gemini.NewGeminiAPIHandler(server.handlers))

	for _, row := range []struct{ path, required, excluded string }{
		{"/openai/v1/models?client_version=cpa", "models", "data"},
		{"/openai/v1/models?client_version=", "models", "data"},
		{"/anthropic/v1/models?client_version=cpa", "data", "models"},
		{"/google/models?client_version=cpa", "models", "data"},
		{"/google/v1beta/models/" + remote, "name", "data"},
	} {
		t.Run(row.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/provider"+row.path, nil)
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Anthropic-Version", "2023-06-01")
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, req)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil ||
				response.Code != http.StatusOK || payload[row.required] == nil || payload[row.excluded] != nil ||
				!strings.Contains(response.Body.String(), remote) || strings.Contains(response.Body.String(), local) {
				t.Fatalf("unexpected Home catalog: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
			}
		})
	}
}
