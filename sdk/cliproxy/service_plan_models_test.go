package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRegisterModelsForAuth_CodexPlanFiltersSpark(t *testing.T) {
	service := &Service{cfg: &config.Config{}}

	tests := []struct {
		name      string
		planType  string
		wantSpark bool
	}{
		{name: "plus", planType: "plus", wantSpark: false},
		{name: "pro", planType: "pro", wantSpark: true},
		{name: "unknown", planType: "", wantSpark: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &coreauth.Auth{
				ID:       "codex-" + tt.name + "-plan-filter",
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"plan_type": tt.planType,
				},
			}
			reg := registry.GetGlobalRegistry()
			reg.UnregisterClient(auth.ID)
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

			service.registerModelsForAuth(auth)
			gotSpark := modelListContains(reg.GetModelsForClient(auth.ID), "gpt-5.3-codex-spark")
			if gotSpark != tt.wantSpark {
				t.Fatalf("Spark registered = %v, want %v", gotSpark, tt.wantSpark)
			}
		})
	}
}

func TestRegisterModelsForAuth_CodexConfigModelsAdvertiseFastMetadata(t *testing.T) {
	const authID = "codex-config-fast-metadata"
	cfg := &config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:  "codex-key",
			BaseURL: "https://codex.example.test",
			Models: []internalconfig.CodexModel{{
				Name:  "gpt-5.5",
				Alias: "codex-fast-alias",
			}},
		}},
	}
	service := &Service{cfg: cfg}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "codex-key",
			"base_url":  "https://codex.example.test",
		},
	}
	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(auth)
	models := reg.GetAvailableModels("openai")
	model := openAIModelByID(models, "codex-fast-alias")
	if model == nil {
		t.Fatalf("expected codex-fast-alias in /v1/models data, got %+v", models)
	}
	params, _ := model["supported_parameters"].([]string)
	if !stringSliceContains(params, "service_tier") {
		t.Fatalf("expected alias to advertise service_tier, got %+v", model)
	}
	speedTiers, _ := model["additional_speed_tiers"].([]string)
	if !stringSliceContains(speedTiers, "fast") {
		t.Fatalf("expected alias to advertise fast tier, got %+v", model)
	}
	serviceTiers, _ := model["service_tiers"].([]registry.ServiceTierInfo)
	if !serviceTierSliceContains(serviceTiers, "priority") {
		t.Fatalf("expected alias to advertise priority service tier, got %+v", model)
	}
}

func TestCoreAuthUpdateHookRefreshesPlanFilteredModelRegistry(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	cfg.SanitizeOAuthModelAlias()
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	manager.SetHook(authRegistryHook{service: service})
	reg := registry.GetGlobalRegistry()

	tests := []struct {
		name         string
		initialAuth  *coreauth.Auth
		updatedAuth  *coreauth.Auth
		premiumModel string
		baseModel    string
	}{
		{
			name: "claude max to pro removes opus",
			initialAuth: &coreauth.Auth{
				ID:       "claude-plan-update-hook",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{"account": map[string]any{"has_claude_max": true}},
					},
				},
			},
			updatedAuth: &coreauth.Auth{
				ID:       "claude-plan-update-hook",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "pro"},
			},
			premiumModel: "claude-opus-4-7",
			baseModel:    "claude-sonnet-4-6",
		},
		{
			name: "codex pro to plus removes spark",
			initialAuth: &coreauth.Auth{
				ID:       "codex-plan-update-hook",
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "pro"},
			},
			updatedAuth: &coreauth.Auth{
				ID:       "codex-plan-update-hook",
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "plus"},
			},
			premiumModel: "gpt-5.3-codex-spark",
			baseModel:    "gpt-5.3-codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg.UnregisterClient(tt.initialAuth.ID)
			t.Cleanup(func() { reg.UnregisterClient(tt.initialAuth.ID) })

			if _, err := manager.Register(ctx, tt.initialAuth); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if !modelListContains(reg.GetModelsForClient(tt.initialAuth.ID), tt.premiumModel) {
				t.Fatalf("expected initial registry to include %s", tt.premiumModel)
			}

			if _, err := manager.Update(ctx, tt.updatedAuth); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			models := reg.GetModelsForClient(tt.updatedAuth.ID)
			if modelListContains(models, tt.premiumModel) {
				t.Fatalf("expected updated registry to remove %s", tt.premiumModel)
			}
			if !modelListContains(models, tt.baseModel) {
				t.Fatalf("expected updated registry to keep %s", tt.baseModel)
			}
		})
	}
}

func TestRegisterModelsForAuth_ClaudePlanFiltersOpusByHighTier(t *testing.T) {
	cfg := &config.Config{}
	cfg.SanitizeOAuthModelAlias()
	service := &Service{cfg: cfg}

	tests := []struct {
		name            string
		auth            *coreauth.Auth
		wantBaseOpus    bool
		wantOpus1MAlias bool
	}{
		{
			name: "pro without usage credits",
			auth: &coreauth.Auth{
				ID:       "claude-pro-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "pro"},
			},
			wantBaseOpus:    false,
			wantOpus1MAlias: false,
		},
		{
			name: "pro with usage credits",
			auth: &coreauth.Auth{
				ID:       "claude-pro-credits-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"plan_type": "pro",
					"quota_snapshot": map[string]any{
						"usage": map[string]any{
							"extra_usage": map[string]any{"is_enabled": true},
						},
					},
				},
			},
			wantBaseOpus:    false,
			wantOpus1MAlias: false,
		},
		{
			name: "max",
			auth: &coreauth.Auth{
				ID:       "claude-max-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "max"},
			},
			wantBaseOpus:    true,
			wantOpus1MAlias: true,
		},
		{
			name: "nested max profile",
			auth: &coreauth.Auth{
				ID:       "claude-nested-max-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"subscription": map[string]any{"has_claude_max": true},
						},
					},
				},
			},
			wantBaseOpus:    true,
			wantOpus1MAlias: true,
		},
		{
			name: "nested pro profile",
			auth: &coreauth.Auth{
				ID:       "claude-nested-pro-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"subscription": map[string]any{"has_claude_pro": true},
						},
						"usage": map[string]any{
							"extra_usage": map[string]any{"is_enabled": true},
						},
					},
				},
			},
			wantBaseOpus:    false,
			wantOpus1MAlias: false,
		},
		{
			name: "unknown local plan",
			auth: &coreauth.Auth{
				ID:       "claude-unknown-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
			},
			wantBaseOpus:    false,
			wantOpus1MAlias: false,
		},
		{
			name: "attributes fallback max",
			auth: &coreauth.Auth{
				ID:       "claude-attrs-max-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"plan_type": "max",
				},
			},
			wantBaseOpus:    true,
			wantOpus1MAlias: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.GetGlobalRegistry()
			reg.UnregisterClient(tt.auth.ID)
			t.Cleanup(func() { reg.UnregisterClient(tt.auth.ID) })

			service.registerModelsForAuth(tt.auth)
			models := reg.GetModelsForClient(tt.auth.ID)
			gotBaseOpus := modelListContains(models, "claude-opus-4-7")
			if gotBaseOpus != tt.wantBaseOpus {
				t.Fatalf("base Opus registered = %v, want %v", gotBaseOpus, tt.wantBaseOpus)
			}
			gotOpus1MAlias := modelListContains(models, "opus[1m]") || modelListContains(models, "claude-opus-4-7[1m]")
			if gotOpus1MAlias != tt.wantOpus1MAlias {
				t.Fatalf("Opus 1M alias registered = %v, want %v", gotOpus1MAlias, tt.wantOpus1MAlias)
			}
			if modelListContains(models, "claude-opus-4-6") != tt.wantBaseOpus {
				t.Fatalf("claude-opus-4-6 registered = %v, want %v", modelListContains(models, "claude-opus-4-6"), tt.wantBaseOpus)
			}
		})
	}
}

func modelListContains(models []*ModelInfo, id string) bool {
	for _, model := range models {
		if model != nil && model.ID == id {
			return true
		}
	}
	return false
}

func openAIModelByID(models []map[string]any, id string) map[string]any {
	for _, model := range models {
		if model != nil && model["id"] == id {
			return model
		}
	}
	return nil
}

func stringSliceContains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func serviceTierSliceContains(values []registry.ServiceTierInfo, id string) bool {
	for _, existing := range values {
		if existing.ID == id {
			return true
		}
	}
	return false
}
