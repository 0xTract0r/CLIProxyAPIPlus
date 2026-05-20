package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
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

func TestRegisterModelsForAuth_ClaudePlanFiltersOneMillionContext(t *testing.T) {
	service := &Service{cfg: &config.Config{}}

	tests := []struct {
		name       string
		auth       *coreauth.Auth
		wantOpus1M bool
	}{
		{
			name: "pro without usage credits",
			auth: &coreauth.Auth{
				ID:       "claude-pro-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "pro"},
			},
			wantOpus1M: false,
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
			wantOpus1M: true,
		},
		{
			name: "max",
			auth: &coreauth.Auth{
				ID:       "claude-max-plan-filter",
				Provider: "claude",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"plan_type": "max"},
			},
			wantOpus1M: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.GetGlobalRegistry()
			reg.UnregisterClient(tt.auth.ID)
			t.Cleanup(func() { reg.UnregisterClient(tt.auth.ID) })

			service.registerModelsForAuth(tt.auth)
			gotOpus1M := modelListContains(reg.GetModelsForClient(tt.auth.ID), "claude-opus-4-7")
			if gotOpus1M != tt.wantOpus1M {
				t.Fatalf("Opus 1M registered = %v, want %v", gotOpus1M, tt.wantOpus1M)
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
