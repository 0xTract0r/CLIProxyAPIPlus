package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestRegisterModelsForAuth_GitLabPublishesSupportedPublicModel(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "gitlab-auth.json",
		Provider: "gitlab",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"model_details": map[string]any{
				"model_provider": "anthropic",
				"model_name":     "claude-sonnet-4-5",
			},
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)
	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != executor.GitLabCodeSuggestionsModel {
		t.Fatalf("published models: %+v, want the supported public GitLab model only", models)
	}
}
