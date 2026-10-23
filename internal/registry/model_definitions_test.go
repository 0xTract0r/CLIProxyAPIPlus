package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexConfigurationUpdateCapability(t *testing.T) {
	for _, tier := range []struct {
		name    string
		models  func() []*ModelInfo
		capable []string
	}{
		{name: "free", models: GetCodexFreeModels, capable: []string{"gpt-6-luna"}},
		{name: "team", models: GetCodexTeamModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "plus", models: GetCodexPlusModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
		{name: "pro", models: GetCodexProModels, capable: []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"}},
	} {
		t.Run(tier.name, func(t *testing.T) {
			byID := make(map[string]*ModelInfo)
			for _, info := range tier.models() {
				if info != nil {
					byID[info.ID] = info
				}
			}
			for _, id := range append([]string{"gpt-5.5"}, tier.capable...) {
				info := byID[id]
				if info == nil {
					t.Errorf("%s is missing", id)
					continue
				}
				want := id != "gpt-5.5"
				if info.SupportConfigurationUpdate != want {
					t.Errorf("%s configuration_update = %v, want %v", id, info.SupportConfigurationUpdate, want)
				}
				if cloned := cloneModelInfo(info); cloned.SupportConfigurationUpdate != want {
					t.Errorf("%s clone configuration_update = %v, want %v", id, cloned.SupportConfigurationUpdate, want)
				}
				raw, errMarshal := json.Marshal(info)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if strings.Contains(string(raw), "support_configuration_update") || strings.Contains(string(raw), "SupportConfigurationUpdate") {
					t.Errorf("%s exposed configuration_update capability: %s", id, raw)
				}
			}
		})
	}

	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "true", raw: `{"id":"test","support_configuration_update":true}`, want: true},
		{name: "false", raw: `{"id":"test","support_configuration_update":false}`},
		{name: "absent", raw: `{"id":"test"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := ModelInfo{SupportConfigurationUpdate: true}
			if errUnmarshal := json.Unmarshal([]byte(tt.raw), &info); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if info.SupportConfigurationUpdate != tt.want {
				t.Errorf("capability = %v, want %v", info.SupportConfigurationUpdate, tt.want)
			}
		})
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsGeminiInteractions(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("gemini-interactions")
	if len(models) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(gemini-interactions) returned no models")
	}
}

func TestGetStaticModelDefinitionsByChannelSupportsKimiAndKimiAI(t *testing.T) {
	for _, channel := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		models := GetStaticModelDefinitionsByChannel(channel)
		if len(models) == 0 {
			t.Fatalf("GetStaticModelDefinitionsByChannel(%s) returned no models", channel)
		}
	}
}

func TestCodexStaticModelsIncludeGPT55(t *testing.T) {
	tierModels := map[string][]*ModelInfo{
		"free": GetCodexFreeModels(),
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
}

// Spark was removed from the embedded upstream catalog by 94b7cc2e.
// Explicit/custom catalogs must still enforce the fork's Pro-only plan gate.
func TestCodexPlusModelsExcludeSpark(t *testing.T) {
	models := []*ModelInfo{{ID: "gpt-5.3-codex-spark"}, {ID: "gpt-5.5"}}
	for _, plan := range []string{"free", "plus", "team", ""} {
		if model := findModelInfo(FilterCodexModelsForPlan(models, plan), "gpt-5.3-codex-spark"); model != nil {
			t.Fatalf("plan %q must exclude Spark", plan)
		}
	}
	if model := findModelInfo(FilterCodexModelsForPlan(models, "pro"), "gpt-5.3-codex-spark"); model == nil {
		t.Fatal("Pro must retain Spark when explicitly present in a catalog")
	}
	for _, models := range [][]*ModelInfo{GetCodexFreeModels(), GetCodexPlusModels(), GetCodexProModels()} {
		if findModelInfo(models, "gpt-5.3-codex-spark") != nil {
			t.Fatal("retired Spark must not be advertised by the embedded catalog")
		}
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
	if !ClaudePlanAllowsOpus("pro") {
		t.Fatal("Claude Pro must allow ordinary Opus")
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

func TestClaudeModelsForPlanSeparatesOpusAndLongContext(t *testing.T) {
	if model := findModelInfo(GetClaudeModelsForPlan("pro", false), "claude-opus-4-7"); model == nil {
		t.Fatal("expected Claude Pro without usage credits to include base Opus")
	}
	if model := findModelInfo(GetClaudeModelsForPlan("pro", false), "claude-sonnet-4-6"); model == nil {
		t.Fatal("expected Claude Pro without usage credits to keep non-Opus Sonnet route")
	}
	if model := findModelInfo(GetClaudeModelsForPlan("pro", true), "claude-opus-4-7"); model == nil {
		t.Fatal("expected Claude Pro with usage credits to include base Opus")
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
	proModels := FilterClaudeModelsForPlan(aliased, "pro", false)
	for _, blocked := range []string{"claude-opus-4-7[1m]", "opus[1m]"} {
		if model := findModelInfo(proModels, blocked); model != nil {
			t.Fatalf("expected Claude Pro without usage credits to exclude %s, got %+v", blocked, model)
		}
	}
	if model := findModelInfo(proModels, "claude-sonnet-4-6"); model == nil {
		t.Fatal("expected Claude Pro filter to keep Sonnet")
	}
	for _, allowed := range []string{"claude-opus-4-7", "claude-opus-4-6"} {
		if findModelInfo(proModels, allowed) == nil {
			t.Fatalf("expected ordinary Opus %s for Pro without credits", allowed)
		}
	}
	for _, model := range aliased {
		got := findModelInfo(FilterClaudeModelsForPlan(aliased, "pro", true), model.ID) != nil
		want := findModelInfo(proModels, model.ID) != nil
		if got != want {
			t.Fatalf("credits must not change included capabilities for %s", model.ID)
		}
	}
	for _, plan := range []string{"", "free", "unknown"} {
		for _, credits := range []bool{false, true} {
			if ClaudePlanAllowsOpus(plan) || ClaudePlanAllowsOpusLongContext(plan, credits) {
				t.Fatalf("unexpected Opus access for plan %q, credits %v", plan, credits)
			}
		}
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

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	got := ModelOverrideHeaders("gpt-5.6-luna")
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestGeminiVertexModelsUseFlashLiteReleaseID(t *testing.T) {
	const releaseID = "gemini-3.1-flash-lite"
	const previewID = releaseID + "-preview"

	for _, model := range GetGeminiVertexModels() {
		if model == nil {
			continue
		}
		if model.ID == previewID {
			t.Fatalf("Vertex model ID = %q, want release ID %q", model.ID, releaseID)
		}
		if model.ID == releaseID {
			return
		}
	}

	t.Fatalf("Vertex models do not contain %q", releaseID)
}

func TestWithXAIBuiltinsIncludesImage20(t *testing.T) {
	models := WithXAIBuiltins(nil)
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinImage20ModelID {
			if model.Created != 1786060800 {
				t.Fatalf("created = %d, want 1786060800 (2026-08-07)", model.Created)
			}
			return
		}
	}
	t.Fatalf("expected xAI builtin model %s", xaiBuiltinImage20ModelID)
}

func TestWithXAIBuiltinsIncludesSpeechModels(t *testing.T) {
	models := WithXAIBuiltins(nil)
	found := map[string]*ModelInfo{}
	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinSpeechModelID || model.ID == xaiBuiltinSpeechVoiceModelID {
			found[model.ID] = model
		}
	}
	for _, id := range []string{xaiBuiltinSpeechModelID, xaiBuiltinSpeechVoiceModelID} {
		model := found[id]
		if model == nil {
			t.Fatalf("expected xAI builtin model %s", id)
		}
		if model.OwnedBy != "xai" || model.Type != "xai" {
			t.Fatalf("%s owned_by/type = %s/%s, want xai/xai", id, model.OwnedBy, model.Type)
		}
	}
}

func TestWithXAIBuiltinsIncludesVideo15GAAndPreviewAlias(t *testing.T) {
	models := WithXAIBuiltins(nil)
	foundGA := false
	foundPreviewAlias := false

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15ModelID {
			foundGA = true
		}
		if model.ID == xaiBuiltinVideo15PreviewID {
			foundPreviewAlias = true
		}
	}

	if !foundGA {
		t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15ModelID)
	}
	if !foundPreviewAlias {
		t.Fatalf("expected xAI builtin compatibility alias %s", xaiBuiltinVideo15PreviewID)
	}
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}

func TestValidateModelsCatalog_Meta(t *testing.T) {
	valid := &staticModelsJSON{
		Meta: []*ModelInfo{
			{ID: "muse-spark-1.3"},
		},
	}
	if err := validateModelsCatalog(valid); err != nil {
		t.Fatalf("expected valid Meta catalog to pass, got: %v", err)
	}

	withNull := &staticModelsJSON{
		Meta: []*ModelInfo{nil},
	}
	if err := validateModelsCatalog(withNull); err == nil {
		t.Fatal("expected error for Meta section with null model, got nil")
	}

	withEmptyID := &staticModelsJSON{
		Meta: []*ModelInfo{{ID: " "}},
	}
	if err := validateModelsCatalog(withEmptyID); err == nil {
		t.Fatal("expected error for Meta section with empty model id, got nil")
	}

	withDuplicate := &staticModelsJSON{
		Meta: []*ModelInfo{
			{ID: "muse-spark-1.3"},
			{ID: "muse-spark-1.3"},
		},
	}
	if err := validateModelsCatalog(withDuplicate); err == nil {
		t.Fatal("expected error for Meta section with duplicate model id, got nil")
	}
}

func TestWithCodexBuiltinsIncludesImage25Models(t *testing.T) {
	models := WithCodexBuiltins(nil)
	expectedModels := map[string]string{
		"gpt-image-2.5-flare":    "GPT Image 2.5 Flare",
		"gpt-image-2.5-sunburst": "GPT Image 2.5 Sunburst",
		"gpt-image-2.5":          "GPT Image 2.5",
	}

	found := make(map[string]*ModelInfo)
	for _, model := range models {
		if model != nil {
			if _, ok := expectedModels[model.ID]; ok {
				found[model.ID] = model
			}
		}
	}

	for id, wantDisplayName := range expectedModels {
		model, ok := found[id]
		if !ok {
			t.Fatalf("expected builtin model %s in WithCodexBuiltins", id)
		}
		if model.DisplayName != wantDisplayName {
			t.Errorf("model %s DisplayName = %q, want %q", id, model.DisplayName, wantDisplayName)
		}
		if model.Object != "model" {
			t.Errorf("model %s Object = %q, want model", id, model.Object)
		}
		if model.OwnedBy != "openai" {
			t.Errorf("model %s OwnedBy = %q, want openai", id, model.OwnedBy)
		}
		if model.Type != "openai" {
			t.Errorf("model %s Type = %q, want openai", id, model.Type)
		}
		if model.Version != id {
			t.Errorf("model %s Version = %q, want %q", id, model.Version, id)
		}
		if model.Created != 1704067200 {
			t.Errorf("model %s Created = %d, want 1704067200", id, model.Created)
		}
	}
}

func TestGetDevinModelsFallback(t *testing.T) {
	devinModels := GetDevinModels()
	if len(devinModels) == 0 {
		t.Fatal("GetDevinModels() returned empty list")
	}

	foundSWE2 := false
	foundFable := false
	foundGemini38 := false
	foundGrok46 := false
	foundDeepSeekV4Flash := false
	foundDeepSeekV41Flash := false
	for _, m := range devinModels {
		if m != nil && m.ID == "devin/swe-2" {
			foundSWE2 = true
			if m.Type != "devin" {
				t.Errorf("devin/swe-2 Type = %q, want devin", m.Type)
			}
		}
		if m != nil && m.ID == "devin/claude-fable-5-1" {
			foundFable = true
		}
		if m != nil && m.ID == "devin/gemini-3-8-flash" {
			foundGemini38 = true
			if m.OwnedBy != "google" {
				t.Errorf("devin/gemini-3-8-flash OwnedBy = %q, want google", m.OwnedBy)
			}
		}
		if m != nil && m.ID == "devin/grok-4-6" {
			foundGrok46 = true
			if m.OwnedBy != "xai" {
				t.Errorf("devin/grok-4-6 OwnedBy = %q, want xai", m.OwnedBy)
			}
		}
		if m != nil && m.ID == "devin/deepseek-v4-flash" {
			foundDeepSeekV4Flash = true
			if m.OwnedBy != "deepseek" {
				t.Errorf("devin/deepseek-v4-flash OwnedBy = %q, want deepseek", m.OwnedBy)
			}
		}
		if m != nil && m.ID == "devin/deepseek-v4-1-flash" {
			foundDeepSeekV41Flash = true
			if m.OwnedBy != "deepseek" {
				t.Errorf("devin/deepseek-v4-1-flash OwnedBy = %q, want deepseek", m.OwnedBy)
			}
		}
	}
	if !foundSWE2 {
		t.Error("expected devin/swe-2 in GetDevinModels()")
	}
	if !foundFable {
		t.Error("expected devin/claude-fable-5-1 in GetDevinModels()")
	}
	if !foundGemini38 {
		t.Error("expected devin/gemini-3-8-flash in GetDevinModels()")
	}
	if !foundGrok46 {
		t.Error("expected devin/grok-4-6 in GetDevinModels()")
	}
	if !foundDeepSeekV4Flash {
		t.Error("expected devin/deepseek-v4-flash in GetDevinModels()")
	}
	if !foundDeepSeekV41Flash {
		t.Error("expected devin/deepseek-v4-1-flash in GetDevinModels()")
	}

	byChannel := GetStaticModelDefinitionsByChannel("devin")
	if len(byChannel) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(\"devin\") returned empty list")
	}

	info := LookupStaticModelInfo("devin/swe-2")
	if info == nil {
		t.Fatal("LookupStaticModelInfo(\"devin/swe-2\") = nil, want valid model")
	}
	if info.DisplayName != "SWE-2" {
		t.Errorf("info.DisplayName = %q, want SWE-2", info.DisplayName)
	}
}
