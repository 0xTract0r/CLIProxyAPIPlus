// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexBuiltinImage15ModelID      = "gpt-image-1.5"
	codexBuiltinImageModelID        = "gpt-image-2"
	codexSparkModelID               = "gpt-5.3-codex-spark"
	codexFastServiceTierID          = "priority"
	xaiBuiltinImageModelID          = "grok-imagine-image"
	xaiBuiltinImageQualityModelID   = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID          = "grok-imagine-video"
	xaiBuiltinVideo15PreviewModelID = "grok-imagine-video-1.5-preview"
)

var codexFastServiceTier = ServiceTierInfo{
	ID:          codexFastServiceTierID,
	Name:        "Fast",
	Description: "1.5x speed, increased usage",
}

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	GeminiCLI   []*ModelInfo `json:"gemini-cli"`
	Vertex      []*ModelInfo `json:"vertex"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
	XAI         []*ModelInfo `json:"xai"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetClaudeModelsForPlan returns Claude model definitions that are safe for a
// known subscription plan. Opus is only available to Claude high-tier plans.
func GetClaudeModelsForPlan(plan string, usageCreditsEnabled bool) []*ModelInfo {
	return FilterClaudeModelsForPlan(GetClaudeModels(), plan, usageCreditsEnabled)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiCLIModels returns Gemini model definitions for the Gemini CLI.
func GetGeminiCLIModels() []*ModelInfo {
	return cloneModelInfos(getModels().GeminiCLI)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetAIStudioModels returns model definitions for AI Studio.
func GetAIStudioModels() []*ModelInfo {
	return cloneModelInfos(getModels().AIStudio)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(FilterCodexModelsForPlan(cloneModelInfos(getModels().CodexFree), "free"))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(FilterCodexModelsForPlan(cloneModelInfos(getModels().CodexTeam), "team"))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(FilterCodexModelsForPlan(cloneModelInfos(getModels().CodexPlus), "plus"))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(FilterCodexModelsForPlan(cloneModelInfos(getModels().CodexPro), "pro"))
}

// GetCodexModelsForPlan returns Codex model definitions for a known plan. An
// unknown plan is intentionally conservative so unsupported premium-only models
// are not routed to accounts whose subscription tier has not been identified.
func GetCodexModelsForPlan(plan string) []*ModelInfo {
	switch NormalizeCodexSubscriptionPlan(plan) {
	case "pro":
		return GetCodexProModels()
	case "plus":
		return GetCodexPlusModels()
	case "team", "business", "go", "enterprise":
		return GetCodexTeamModels()
	case "free":
		return GetCodexFreeModels()
	default:
		return GetCodexFreeModels()
	}
}

// FilterCodexModelsForPlan removes models that are not supported by the given
// Codex subscription plan.
func FilterCodexModelsForPlan(models []*ModelInfo, plan string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	allowSpark := CodexPlanAllowsSpark(plan)
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		if !allowSpark && strings.EqualFold(strings.TrimSpace(model.ID), codexSparkModelID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// FilterClaudeModelsForPlan removes Claude Opus models that need a high-tier
// Claude subscription. Usage credits do not change Opus eligibility.
func FilterClaudeModelsForPlan(models []*ModelInfo, plan string, usageCreditsEnabled bool) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	if ClaudePlanAllowsOpusLongContext(plan, usageCreditsEnabled) {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		if IsClaudeOpusModelInfo(model) {
			continue
		}
		out = append(out, model)
	}
	return out
}

func CodexPlanAllowsSpark(plan string) bool {
	return NormalizeCodexSubscriptionPlan(plan) == "pro"
}

func ClaudePlanAllowsOpus(plan string) bool {
	switch NormalizeClaudeSubscriptionPlan(plan) {
	case "max", "team", "business", "enterprise":
		return true
	default:
		return false
	}
}

func ClaudePlanAllowsOpusLongContext(plan string, _ bool) bool {
	return ClaudePlanAllowsOpus(plan)
}

func IsClaudeOpusModelInfo(model *ModelInfo) bool {
	if model == nil {
		return false
	}
	return IsClaudeOpusModelID(model.ID) || IsClaudeOpusModelID(model.DisplayName)
}

func IsClaudeOpusModelID(modelID string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(modelID)), "opus")
}

func IsClaudeOpus1MAlias(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	return strings.Contains(id, "opus") && strings.Contains(id, "[1m]")
}

func IsCodexSparkModelID(modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(modelID), codexSparkModelID)
}

func NormalizeClaudeSubscriptionPlan(plan string) string {
	normalized := normalizeSubscriptionPlan(plan)
	switch {
	case strings.Contains(normalized, "max"):
		return "max"
	case strings.Contains(normalized, "team"):
		return "team"
	case strings.Contains(normalized, "business"):
		return "business"
	case strings.Contains(normalized, "enterprise"):
		return "enterprise"
	case strings.Contains(normalized, "pro"):
		return "pro"
	case strings.Contains(normalized, "free"):
		return "free"
	default:
		return normalized
	}
}

func NormalizeCodexSubscriptionPlan(plan string) string {
	normalized := normalizeSubscriptionPlan(plan)
	normalized = strings.TrimPrefix(normalized, "chatgpt-")
	switch {
	case strings.Contains(normalized, "pro"):
		return "pro"
	case strings.Contains(normalized, "plus"):
		return "plus"
	case strings.Contains(normalized, "team"):
		return "team"
	case strings.Contains(normalized, "business"):
		return "business"
	case strings.Contains(normalized, "enterprise"):
		return "enterprise"
	case strings.Contains(normalized, "free"):
		return "free"
	case normalized == "go":
		return "go"
	default:
		return normalized
	}
}

func normalizeSubscriptionPlan(plan string) string {
	normalized := strings.ToLower(strings.TrimSpace(plan))
	normalized = strings.TrimPrefix(normalized, "plan_")
	normalized = strings.TrimPrefix(normalized, "plan-")
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "pro-lite", "prolite":
		return "pro"
	default:
		return normalized
	}
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// AntigravityWebSearchModelFor returns the Antigravity model that should run a
// native web search request for modelID.
func AntigravityWebSearchModelFor(modelID string) string {
	modelID = normalizeAntigravityCapabilityModelID(modelID)
	if modelID == "" {
		return ""
	}
	for _, model := range GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if model == nil {
			continue
		}
		currentModelID := normalizeAntigravityCapabilityModelID(model.ID)
		if currentModelID == "" {
			continue
		}
		if currentModelID == modelID {
			if model.SupportsWebSearch {
				return currentModelID
			}
			return ""
		}
	}
	return ""
}

// GetXAIModels returns the standard xAI Grok model definitions.
func GetXAIModels() []*ModelInfo {
	return WithXAIBuiltins(cloneModelInfos(getModels().XAI))
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImage15ModelInfo(), codexBuiltinImageModelInfo())
}

func applyCodexCatalogCompatibility(data *staticModelsJSON) {
	if data == nil {
		return
	}
	applyCodexFastModeMetadata(data.CodexFree)
	applyCodexFastModeMetadata(data.CodexTeam)
	applyCodexFastModeMetadata(data.CodexPlus)
	applyCodexFastModeMetadata(data.CodexPro)
}

func applyCodexFastModeMetadata(models []*ModelInfo) {
	for _, model := range models {
		if model == nil || !isCodexFastModeModelID(model.ID) {
			continue
		}
		applyCodexFastModeMetadataToModel(model)
	}
}

func applyCodexFastModeMetadataToModel(model *ModelInfo) {
	if model == nil {
		return
	}
	model.SupportedParameters = appendUniqueString(model.SupportedParameters, "service_tier")
	model.AdditionalSpeedTiers = appendUniqueString(model.AdditionalSpeedTiers, "fast")
	model.ServiceTiers = appendUniqueServiceTier(model.ServiceTiers, codexFastServiceTier)
}

func applyCodexProviderCompatibilityMetadata(model *ModelInfo) {
	if model == nil || !codexModelInfoSupportsFastMode(model) {
		return
	}
	applyCodexFastModeMetadataToModel(model)
}

func codexModelInfoSupportsFastMode(model *ModelInfo) bool {
	if model == nil {
		return false
	}
	for _, candidate := range []string{model.ID, model.Version, model.Name, model.DisplayName} {
		if isCodexFastModeModelID(candidate) {
			return true
		}
	}
	return false
}

func isCodexFastModeModelID(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	normalized = strings.TrimPrefix(normalized, "models/")
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		normalized = normalized[idx+1:]
	}
	switch normalized {
	case "gpt-5.4", "gpt-5.5":
		return true
	default:
		return false
	}
}

func appendUniqueString(values []string, value string) []string {
	needle := strings.TrimSpace(value)
	if needle == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(strings.TrimSpace(existing), needle) {
			return values
		}
	}
	return append(values, needle)
}

func appendUniqueServiceTier(values []ServiceTierInfo, value ServiceTierInfo) []ServiceTierInfo {
	needle := strings.TrimSpace(value.ID)
	if needle == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(strings.TrimSpace(existing.ID), needle) {
			return values
		}
	}
	return append(values, value)
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15PreviewModelInfo())
}

func normalizeAntigravityCapabilityModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if open := strings.LastIndex(modelID, "("); open >= 0 && strings.HasSuffix(modelID, ")") {
		modelID = strings.TrimSpace(modelID[:open])
	}
	return modelID
}

func codexBuiltinImage15ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImage15ModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 1.5",
		Version:     codexBuiltinImage15ModelID,
	}
}

func codexBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImageModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2",
		Version:     codexBuiltinImageModelID,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func xaiBuiltinVideo15PreviewModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15PreviewModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5 Preview",
		Name:        xaiBuiltinVideo15PreviewModelID,
		Description: "xAI Grok preview video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - aistudio
//   - codex
//   - kimi
//   - github-copilot
//   - kiro
//   - kilo
//   - amazonq
//   - antigravity
//   - xai
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetCodexProModels()
	case "kimi":
		return GetKimiModels()
	case "github-copilot":
		return GetGitHubCopilotModels()
	case "kiro":
		return GetKiroModels()
	case "kilo":
		return GetKiloModels()
	case "amazonq":
		return GetAmazonQModels()
	case "antigravity":
		return GetAntigravityModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.AIStudio,
		data.CodexPro,
		data.Kimi,
		data.Antigravity,
		data.XAI,
		GetGitHubCopilotModels(),
		GetKiroModels(),
		GetKiloModels(),
		GetAmazonQModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}

// GetGitHubCopilotModels returns the available models for GitHub Copilot.
// These models are available through the GitHub Copilot API at api.githubcopilot.com.
func GetGitHubCopilotModels() []*ModelInfo {
	now := int64(1732752000) // 2024-11-27
	gpt4oEntries := []struct {
		ID          string
		DisplayName string
		Description string
	}{
		{ID: "gpt-4o-2024-11-20", DisplayName: "GPT-4o (2024-11-20)", Description: "OpenAI GPT-4o 2024-11-20 via GitHub Copilot"},
		{ID: "gpt-4o-2024-08-06", DisplayName: "GPT-4o (2024-08-06)", Description: "OpenAI GPT-4o 2024-08-06 via GitHub Copilot"},
		{ID: "gpt-4o-2024-05-13", DisplayName: "GPT-4o (2024-05-13)", Description: "OpenAI GPT-4o 2024-05-13 via GitHub Copilot"},
		{ID: "gpt-4o", DisplayName: "GPT-4o", Description: "OpenAI GPT-4o via GitHub Copilot"},
		{ID: "gpt-4-o-preview", DisplayName: "GPT-4-o Preview", Description: "OpenAI GPT-4-o Preview via GitHub Copilot"},
	}

	models := []*ModelInfo{
		{
			ID:                  "gpt-4.1",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-4.1",
			Description:         "OpenAI GPT-4.1 via GitHub Copilot",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
		},
	}

	for _, entry := range gpt4oEntries {
		models = append(models, &ModelInfo{
			ID:                  entry.ID,
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         entry.DisplayName,
			Description:         entry.Description,
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
		})
	}

	return append(models, []*ModelInfo{
		{
			ID:                  "gpt-5",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5",
			Description:         "OpenAI GPT-5 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5-mini",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5 Mini",
			Description:         "OpenAI GPT-5 Mini via GitHub Copilot",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5-codex",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5 Codex",
			Description:         "OpenAI GPT-5 Codex via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.1",
			Description:         "OpenAI GPT-5.1 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.1 Codex",
			Description:         "OpenAI GPT-5.1 Codex via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex-mini",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.1 Codex Mini",
			Description:         "OpenAI GPT-5.1 Codex Mini via GitHub Copilot",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high"}},
		},
		{
			ID:                  "gpt-5.1-codex-max",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.1 Codex Max",
			Description:         "OpenAI GPT-5.1 Codex Max via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh"}},
		},
		{
			ID:                  "gpt-5.2",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.2",
			Description:         "OpenAI GPT-5.2 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh"}},
		},
		{
			ID:                  "gpt-5.2-codex",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.2 Codex",
			Description:         "OpenAI GPT-5.2 Codex via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh"}},
		},
		{
			ID:                  "gpt-5.3-codex",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "GPT-5.3 Codex",
			Description:         "OpenAI GPT-5.3 Codex via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			SupportedEndpoints:  []string{"/responses"},
			Thinking:            &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh"}},
		},
		{
			ID:                  "claude-haiku-4.5",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Haiku 4.5",
			Description:         "Anthropic Claude Haiku 4.5 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-opus-4.1",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Opus 4.1",
			Description:         "Anthropic Claude Opus 4.1 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 32000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-opus-4.5",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Opus 4.5",
			Description:         "Anthropic Claude Opus 4.5 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-opus-4.6",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Opus 4.6",
			Description:         "Anthropic Claude Opus 4.6 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-sonnet-4",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Sonnet 4",
			Description:         "Anthropic Claude Sonnet 4 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-sonnet-4.5",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Sonnet 4.5",
			Description:         "Anthropic Claude Sonnet 4.5 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "claude-sonnet-4.6",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Claude Sonnet 4.6",
			Description:         "Anthropic Claude Sonnet 4.6 via GitHub Copilot",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			SupportedEndpoints:  []string{"/chat/completions"},
		},
		{
			ID:                  "gemini-2.5-pro",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Gemini 2.5 Pro",
			Description:         "Google Gemini 2.5 Pro via GitHub Copilot",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
		},
		{
			ID:                  "gemini-3-pro-preview",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Gemini 3 Pro (Preview)",
			Description:         "Google Gemini 3 Pro Preview via GitHub Copilot",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
		},
		{
			ID:                  "gemini-3.1-pro-preview",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Gemini 3.1 Pro (Preview)",
			Description:         "Google Gemini 3.1 Pro Preview via GitHub Copilot",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
		},
		{
			ID:                  "gemini-3-flash-preview",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Gemini 3 Flash (Preview)",
			Description:         "Google Gemini 3 Flash Preview via GitHub Copilot",
			ContextLength:       1048576,
			MaxCompletionTokens: 65536,
		},
		{
			ID:                  "grok-code-fast-1",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Grok Code Fast 1",
			Description:         "xAI Grok Code Fast 1 via GitHub Copilot",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
		},
		{
			ID:                  "oswe-vscode-prime",
			Object:              "model",
			Created:             now,
			OwnedBy:             "github-copilot",
			Type:                "github-copilot",
			DisplayName:         "Raptor mini (Preview)",
			Description:         "Raptor mini via GitHub Copilot",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedEndpoints:  []string{"/chat/completions", "/responses"},
		},
	}...)
}

// GetKiroModels returns the Kiro (AWS CodeWhisperer) model definitions
func GetKiroModels() []*ModelInfo {
	return []*ModelInfo{
		// --- Base Models ---
		{
			ID:                  "kiro-auto",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Auto",
			Description:         "Automatic model selection by Kiro",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-opus-4-6",
			Object:              "model",
			Created:             1736899200, // 2025-01-15
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Opus 4.6",
			Description:         "Claude Opus 4.6 via Kiro (2.2x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4-6",
			Object:              "model",
			Created:             1739836800, // 2025-02-18
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4.6",
			Description:         "Claude Sonnet 4.6 via Kiro (1.3x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-opus-4-5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Opus 4.5",
			Description:         "Claude Opus 4.5 via Kiro (2.2x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4-5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4.5",
			Description:         "Claude Sonnet 4.5 via Kiro (1.3x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4",
			Description:         "Claude Sonnet 4 via Kiro (1.3x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-haiku-4-5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Haiku 4.5",
			Description:         "Claude Haiku 4.5 via Kiro (0.4x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		// --- 第三方模型 (通过 Kiro 接入) ---
		{
			ID:                  "kiro-deepseek-3-2",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro DeepSeek 3.2",
			Description:         "DeepSeek 3.2 via Kiro",
			ContextLength:       128000,
			MaxCompletionTokens: 32768,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-minimax-m2-1",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro MiniMax M2.1",
			Description:         "MiniMax M2.1 via Kiro",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-qwen3-coder-next",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Qwen3 Coder Next",
			Description:         "Qwen3 Coder Next via Kiro",
			ContextLength:       128000,
			MaxCompletionTokens: 32768,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-gpt-4o",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro GPT-4o",
			Description:         "OpenAI GPT-4o via Kiro",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
		},
		{
			ID:                  "kiro-gpt-4",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro GPT-4",
			Description:         "OpenAI GPT-4 via Kiro",
			ContextLength:       128000,
			MaxCompletionTokens: 8192,
		},
		{
			ID:                  "kiro-gpt-4-turbo",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro GPT-4 Turbo",
			Description:         "OpenAI GPT-4 Turbo via Kiro",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
		},
		{
			ID:                  "kiro-gpt-3-5-turbo",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro GPT-3.5 Turbo",
			Description:         "OpenAI GPT-3.5 Turbo via Kiro",
			ContextLength:       16384,
			MaxCompletionTokens: 4096,
		},
		// --- Agentic Variants (Optimized for coding agents with chunked writes) ---
		{
			ID:                  "kiro-claude-opus-4-6-agentic",
			Object:              "model",
			Created:             1736899200, // 2025-01-15
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Opus 4.6 (Agentic)",
			Description:         "Claude Opus 4.6 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4-6-agentic",
			Object:              "model",
			Created:             1739836800, // 2025-02-18
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4.6 (Agentic)",
			Description:         "Claude Sonnet 4.6 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-opus-4-5-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Opus 4.5 (Agentic)",
			Description:         "Claude Opus 4.5 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4-5-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4.5 (Agentic)",
			Description:         "Claude Sonnet 4.5 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-sonnet-4-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Sonnet 4 (Agentic)",
			Description:         "Claude Sonnet 4 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-claude-haiku-4-5-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Claude Haiku 4.5 (Agentic)",
			Description:         "Claude Haiku 4.5 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-deepseek-3-2-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro DeepSeek 3.2 (Agentic)",
			Description:         "DeepSeek 3.2 optimized for coding agents (chunked writes)",
			ContextLength:       128000,
			MaxCompletionTokens: 32768,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-minimax-m2-1-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro MiniMax M2.1 (Agentic)",
			Description:         "MiniMax M2.1 optimized for coding agents (chunked writes)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
		{
			ID:                  "kiro-qwen3-coder-next-agentic",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Kiro Qwen3 Coder Next (Agentic)",
			Description:         "Qwen3 Coder Next optimized for coding agents (chunked writes)",
			ContextLength:       128000,
			MaxCompletionTokens: 32768,
			Thinking:            &ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		},
	}
}

// GetAmazonQModels returns the Amazon Q (AWS CodeWhisperer) model definitions.
// These models use the same API as Kiro and share the same executor.
func GetAmazonQModels() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "amazonq-auto",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro", // Uses Kiro executor - same API
			DisplayName:         "Amazon Q Auto",
			Description:         "Automatic model selection by Amazon Q",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
		{
			ID:                  "amazonq-claude-opus-4.5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Amazon Q Claude Opus 4.5",
			Description:         "Claude Opus 4.5 via Amazon Q (2.2x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
		{
			ID:                  "amazonq-claude-sonnet-4.5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Amazon Q Claude Sonnet 4.5",
			Description:         "Claude Sonnet 4.5 via Amazon Q (1.3x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
		{
			ID:                  "amazonq-claude-sonnet-4",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Amazon Q Claude Sonnet 4",
			Description:         "Claude Sonnet 4 via Amazon Q (1.3x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
		{
			ID:                  "amazonq-claude-haiku-4.5",
			Object:              "model",
			Created:             1732752000,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         "Amazon Q Claude Haiku 4.5",
			Description:         "Claude Haiku 4.5 via Amazon Q (0.4x credit)",
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
		},
	}
}
