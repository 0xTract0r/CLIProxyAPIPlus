package cliproxy

import (
	"context"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	registry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"reflect"
	"testing"
)

func TestCodexModelRegistrationPreservesCredentialBoundaries(t *testing.T) {
	t.Run("explicit_alias_prefix_and_exclusion_union", func(t *testing.T) {
		id := "codex-registration-exclusion-union"
		r := registry.GetGlobalRegistry()
		defer r.UnregisterClient(id)
		cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "codex-registration-test-key", ExcludedModels: []string{"config-drop"}, Models: []internalconfig.CodexModel{{Name: "upstream", Alias: "keep", SupportConfigurationUpdate: true}, {Name: "cfg", Alias: "config-drop"}, {Name: "attr", Alias: "auth-drop"}}}}}
		cfg.ForceModelPrefix = true
		a := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Prefix: "pref", Attributes: map[string]string{coreauth.AttributeAPIKey: "codex-registration-test-key", coreauth.AttributeSource: "config:codex:test", coreauth.AttributeConfigIndex: "0", "excluded_models": "auth-drop", "plan_type": "free"}}
		(&Service{cfg: cfg}).registerModelsForAuth(context.Background(), a)
		got := r.GetModelsForClient(id)
		if len(got) != 1 || got[0].ID != "pref/keep" || !got[0].SupportConfigurationUpdate {
			t.Fatal("explicit alias/prefix/exclusion union/capability contract changed")
		}
	})
	t.Run("defaults_do_not_mutate_oauth_catalog", func(t *testing.T) {
		id := "codex-registration-default-capabilities"
		r := registry.GetGlobalRegistry()
		defer r.UnregisterClient(id)
		before := registry.GetCodexProModels()
		cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "codex-registration-test-key"}}}
		a := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeAPIKey: "codex-registration-test-key", coreauth.AttributeSource: "config:codex:test", coreauth.AttributeConfigIndex: "0", "plan_type": "free"}}
		(&Service{cfg: cfg}).registerModelsForAuth(context.Background(), a)
		got := r.GetModelsForClient(id)
		if !reflect.DeepEqual(codexModelIDSet(got), codexModelIDSet(before)) {
			t.Fatal("API key inherited OAuth free restriction")
		}
		for _, m := range got {
			if m.SupportConfigurationUpdate {
				t.Fatal("API key default inherited OAuth capability")
			}
		}
		after := registry.GetCodexProModels()
		if !reflect.DeepEqual(before, after) {
			t.Fatal("default building mutated OAuth model catalog")
		}
	})
	t.Run("oauth_plan_registration_unchanged", func(t *testing.T) {
		for _, plan := range []string{"free", "pro", "unknown"} {
			id := "codex-registration-plan-" + plan
			r := registry.GetGlobalRegistry()
			defer r.UnregisterClient(id)
			(&Service{cfg: &config.Config{}}).registerModelsForAuth(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"plan_type": plan}})
			if !reflect.DeepEqual(codexModelIDSet(r.GetModelsForClient(id)), codexModelIDSet(registry.GetCodexModelsForPlan(plan))) {
				t.Fatal("OAuth plan filter changed")
			}
		}
	})
}
