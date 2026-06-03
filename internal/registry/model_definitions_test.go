package registry

import "testing"

func TestCodexStaticModelsIncludeGPT55(t *testing.T) {
	tierModels := map[string][]*ModelInfo{
		"team": GetCodexTeamModels(),
		"plus": GetCodexPlusModels(),
		"pro":  GetCodexProModels(),
	}

	for tier, models := range tierModels {
		t.Run(tier, func(t *testing.T) {
			model := findModelInfo(models, "gpt-5.5")
			if model == nil {
				t.Fatalf("expected codex %s tier to include gpt-5.5", tier)
			}
			assertGPT55ModelInfo(t, tier, model)
		})
	}

	model := LookupStaticModelInfo("gpt-5.5")
	if model == nil {
		t.Fatal("expected LookupStaticModelInfo to find gpt-5.5")
	}
	assertGPT55ModelInfo(t, "lookup", model)

	if model := findModelInfo(GetCodexFreeModels(), "gpt-5.5"); model != nil {
		t.Fatalf("expected codex free tier to exclude gpt-5.5, got %+v", model)
	}
}

func TestCodexPlusModelsExcludeSpark(t *testing.T) {
	if model := findModelInfo(GetCodexPlusModels(), "gpt-5.3-codex-spark"); model != nil {
		t.Fatalf("expected codex plus tier to exclude Spark, got %+v", model)
	}
	if model := findModelInfo(GetCodexProModels(), "gpt-5.3-codex-spark"); model == nil {
		t.Fatal("expected codex pro tier to include Spark")
	}
	if model := findModelInfo(GetCodexModelsForPlan(""), "gpt-5.3-codex-spark"); model != nil {
		t.Fatalf("expected unknown codex tier to exclude Spark, got %+v", model)
	}
}

func TestCodexFastModeMetadataAppliedToCatalogs(t *testing.T) {
	models := []*ModelInfo{
		{ID: "gpt-5.5", SupportedParameters: []string{"tools"}},
		{ID: "gpt-5.4", SupportedParameters: []string{"tools"}},
		{ID: "gpt-5.4-mini", SupportedParameters: []string{"tools"}},
		{ID: "gpt-5.3-codex-spark", SupportedParameters: []string{"tools"}},
	}
	data := &staticModelsJSON{CodexPro: models}

	applyCodexCatalogCompatibility(data)

	assertCodexFastModeMetadata(t, "gpt-5.5", models[0])
	assertCodexFastModeMetadata(t, "gpt-5.4", models[1])
	assertNoCodexFastModeMetadata(t, "gpt-5.4-mini", models[2])
	assertNoCodexFastModeMetadata(t, "gpt-5.3-codex-spark", models[3])
}

func TestProviderSpecificPlanCapabilities(t *testing.T) {
	if ClaudePlanAllowsOpus("pro") {
		t.Fatal("Claude Pro must not allow Opus")
	}
	if !ClaudePlanAllowsOpus("max") {
		t.Fatal("Claude Max must allow Opus")
	}
	if !CodexPlanAllowsSpark("pro") {
		t.Fatal("Codex Pro must allow Spark")
	}
	if CodexPlanAllowsSpark("plus") || CodexPlanAllowsSpark("") {
		t.Fatal("Codex Plus/unknown must not allow Spark")
	}
}

func TestClaudeSonnet46StaticModelHas1MContext(t *testing.T) {
	model := findModelInfo(GetClaudeModels(), "claude-sonnet-4-6")
	if model == nil {
		t.Fatal("expected claude-sonnet-4-6 in static Claude models")
	}
	if model.ContextLength != 1000000 {
		t.Fatalf("claude-sonnet-4-6 context length = %d, want 1000000", model.ContextLength)
	}
}

func TestClaudeModelsForPlanFiltersAllOpusForNonHighTier(t *testing.T) {
	if model := findModelInfo(GetClaudeModelsForPlan("pro", false), "claude-opus-4-7"); model != nil {
		t.Fatalf("expected Claude Pro without usage credits to exclude base Opus, got %+v", model)
	}
	if model := findModelInfo(GetClaudeModelsForPlan("pro", false), "claude-sonnet-4-6"); model == nil {
		t.Fatal("expected Claude Pro without usage credits to keep non-Opus Sonnet route")
	}
	if model := findModelInfo(GetClaudeModelsForPlan("pro", true), "claude-opus-4-7"); model != nil {
		t.Fatalf("expected Claude Pro with usage credits to still exclude base Opus, got %+v", model)
	}
	if model := findModelInfo(GetClaudeModelsForPlan("max", false), "claude-opus-4-7"); model == nil {
		t.Fatal("expected Claude Max to include base Opus")
	}
	if model := findModelInfo(GetClaudeModelsForPlan("", false), "claude-opus-4-7"); model != nil {
		t.Fatalf("expected unknown Claude plan to exclude base Opus, got %+v", model)
	}

	aliased := []*ModelInfo{
		{ID: "claude-opus-4-7"},
		{ID: "claude-opus-4-6"},
		{ID: "claude-opus-4-7[1m]"},
		{ID: "opus[1m]"},
		{ID: "claude-sonnet-4-6"},
	}
	proModels := FilterClaudeModelsForPlan(aliased, "pro", true)
	for _, blocked := range []string{"claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-7[1m]", "opus[1m]"} {
		if model := findModelInfo(proModels, blocked); model != nil {
			t.Fatalf("expected Claude Pro with usage credits to exclude %s, got %+v", blocked, model)
		}
	}
	if model := findModelInfo(proModels, "claude-sonnet-4-6"); model == nil {
		t.Fatal("expected Claude Pro filter to keep Sonnet")
	}
	maxModels := FilterClaudeModelsForPlan(aliased, "max", false)
	for _, allowed := range []string{"claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-7[1m]", "opus[1m]"} {
		if model := findModelInfo(maxModels, allowed); model == nil {
			t.Fatalf("expected Claude Max filter to keep %s", allowed)
		}
	}
}

func findModelInfo(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func assertGPT55ModelInfo(t *testing.T, source string, model *ModelInfo) {
	t.Helper()

	if model.ID != "gpt-5.5" {
		t.Fatalf("%s id mismatch: got %q", source, model.ID)
	}
	if model.Object != "model" {
		t.Fatalf("%s object mismatch: got %q", source, model.Object)
	}
	if model.Created != 1776902400 {
		t.Fatalf("%s created timestamp mismatch: got %d", source, model.Created)
	}
	if model.OwnedBy != "openai" {
		t.Fatalf("%s owned_by mismatch: got %q", source, model.OwnedBy)
	}
	if model.Type != "openai" {
		t.Fatalf("%s type mismatch: got %q", source, model.Type)
	}
	if model.DisplayName != "GPT 5.5" {
		t.Fatalf("%s display name mismatch: got %q", source, model.DisplayName)
	}
	if model.Version != "gpt-5.5" {
		t.Fatalf("%s version mismatch: got %q", source, model.Version)
	}
	if model.Description != "Frontier model for complex coding, research, and real-world work." {
		t.Fatalf("%s description mismatch: got %q", source, model.Description)
	}
	if model.ContextLength != 272000 {
		t.Fatalf("%s context length mismatch: got %d", source, model.ContextLength)
	}
	if model.MaxCompletionTokens != 128000 {
		t.Fatalf("%s max completion tokens mismatch: got %d", source, model.MaxCompletionTokens)
	}
	if len(model.SupportedParameters) != 2 ||
		model.SupportedParameters[0] != "tools" ||
		model.SupportedParameters[1] != "service_tier" {
		t.Fatalf("%s supported parameters mismatch: got %v", source, model.SupportedParameters)
	}
	assertCodexFastModeMetadata(t, source, model)
	if model.Thinking == nil {
		t.Fatalf("%s missing thinking support", source)
	}

	want := []string{"low", "medium", "high", "xhigh"}
	if len(model.Thinking.Levels) != len(want) {
		t.Fatalf("%s thinking level count mismatch: got %d, want %d", source, len(model.Thinking.Levels), len(want))
	}
	for i, level := range want {
		if model.Thinking.Levels[i] != level {
			t.Fatalf("%s thinking level %d mismatch: got %q, want %q", source, i, model.Thinking.Levels[i], level)
		}
	}
}

func assertCodexFastModeMetadata(t *testing.T, source string, model *ModelInfo) {
	t.Helper()
	if !hasString(model.SupportedParameters, "service_tier") {
		t.Fatalf("%s missing service_tier supported parameter: %+v", source, model.SupportedParameters)
	}
	if !hasString(model.AdditionalSpeedTiers, "fast") {
		t.Fatalf("%s missing fast speed tier: %+v", source, model.AdditionalSpeedTiers)
	}
	if !hasServiceTier(model.ServiceTiers, "priority") {
		t.Fatalf("%s missing priority service tier: %+v", source, model.ServiceTiers)
	}
}

func assertNoCodexFastModeMetadata(t *testing.T, source string, model *ModelInfo) {
	t.Helper()
	if hasString(model.AdditionalSpeedTiers, "fast") {
		t.Fatalf("%s must not advertise fast speed tier: %+v", source, model.AdditionalSpeedTiers)
	}
	if hasServiceTier(model.ServiceTiers, "priority") {
		t.Fatalf("%s must not advertise priority service tier: %+v", source, model.ServiceTiers)
	}
}

func hasString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func hasServiceTier(values []ServiceTierInfo, id string) bool {
	for _, existing := range values {
		if existing.ID == id {
			return true
		}
	}
	return false
}

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}
