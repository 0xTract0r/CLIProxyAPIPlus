package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestOpenAIModelsPreservesCapabilityMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := registry.GetGlobalRegistry()
	const authID = "openai-models-capability-metadata"
	const modelID = "openai-models-test/gpt-5.5"
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{
		ID:      modelID,
		Object:  "model",
		OwnedBy: "openai",
		Type:    "openai",
	}})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", h.OpenAIModels)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	model := jsonModelByID(body.Data, modelID)
	if model == nil {
		t.Fatalf("expected %s in models response, got %+v", modelID, body.Data)
	}
	if !jsonStringArrayContains(model["supported_parameters"], "service_tier") {
		t.Fatalf("expected supported_parameters to include service_tier, got %+v", model)
	}
	if !jsonStringArrayContains(model["additional_speed_tiers"], "fast") {
		t.Fatalf("expected additional_speed_tiers to include fast, got %+v", model)
	}
	if !jsonObjectArrayContainsID(model["service_tiers"], "priority") {
		t.Fatalf("expected service_tiers to include priority, got %+v", model)
	}
}

func jsonModelByID(models []map[string]any, id string) map[string]any {
	for _, model := range models {
		if model != nil && model["id"] == id {
			return model
		}
	}
	return nil
}

func jsonStringArrayContains(value any, expected string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == expected {
			return true
		}
	}
	return false
}

func jsonObjectArrayContainsID(value any, expected string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if ok && obj["id"] == expected {
			return true
		}
	}
	return false
}
