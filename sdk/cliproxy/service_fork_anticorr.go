package cliproxy

// fork(anticorr): symbols that upstream's per-concern split does not carry.
// These were previously defined inline in the fork's service.go monolith; when
// adopting upstream's slim service.go + service_*.go structure they are gathered
// here so the anti-correlation / fork-only capabilities survive the merge:
//
//   - authRegistryHook: core-manager registry hook that re-registers
//     plan-filtered models and refreshes scheduler entries on auth add/update.
//   - GetWatcher: external accessor used by the Kiro RefreshManager wiring.
//   - subscription plan-type + usage-credit helpers backing the Opus/Codex plan
//     gate woven into service_models.go.
//   - dynamic Kiro model fetch + agentic-variant synthesis.

import (
	"context"
	"fmt"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// authRegistryHook keeps the global model registry and scheduler in sync with
// core-manager auth lifecycle events. On every add/update it re-runs the
// plan-filtered model registration so subscription-tier gating (Opus/Codex) is
// re-evaluated when an auth's plan attributes change.
type authRegistryHook struct {
	service *Service
}

func (h authRegistryHook) OnAuthRegistered(ctx context.Context, auth *coreauth.Auth) {
	h.refresh(ctx, auth)
}

func (h authRegistryHook) OnAuthUpdated(ctx context.Context, auth *coreauth.Auth) {
	h.refresh(ctx, auth)
}

func (h authRegistryHook) OnResult(context.Context, coreauth.Result) {}

func (h authRegistryHook) refresh(ctx context.Context, auth *coreauth.Auth) {
	if h.service == nil || h.service.coreManager == nil || auth == nil {
		return
	}
	h.service.registerModelsForAuth(ctx, auth)
	h.service.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)
	h.service.coreManager.RefreshSchedulerEntry(auth.ID)
}

// GetWatcher returns the underlying WatcherWrapper instance.
// This allows external components (e.g., RefreshManager) to interact with the watcher.
// Returns nil if the service or watcher is not initialized.
func (s *Service) GetWatcher() *WatcherWrapper {
	if s == nil {
		return nil
	}
	return s.watcher
}

// authSubscriptionPlanType returns the raw subscription plan type declared by the auth.
func authSubscriptionPlanType(auth *coreauth.Auth) string {
	return auth.SubscriptionPlanType()
}

// authClaudeSubscriptionPlanType normalizes the auth's plan to the Claude tier vocabulary.
func authClaudeSubscriptionPlanType(auth *coreauth.Auth) string {
	return registry.NormalizeClaudeSubscriptionPlan(authSubscriptionPlanType(auth))
}

// authCodexSubscriptionPlanType normalizes the auth's plan to the Codex tier vocabulary.
func authCodexSubscriptionPlanType(auth *coreauth.Auth) string {
	return registry.NormalizeCodexSubscriptionPlan(authSubscriptionPlanType(auth))
}

// claudeUsageCreditsEnabled reports whether the auth has extra/credit usage enabled,
// which unlocks Opus-tier models for Pro plans that would otherwise be gated out.
func claudeUsageCreditsEnabled(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		for _, key := range []string{"usage_credits_enabled", "extra_usage_enabled", "has_extra_usage_enabled"} {
			if authTruthy(auth.Attributes[key]) {
				return true
			}
		}
	}
	if authMetadataBool(auth.Metadata, "usage_credits_enabled", "extra_usage_enabled", "has_extra_usage_enabled") {
		return true
	}
	snapshot := authMetadataMap(auth.Metadata, "quota_snapshot")
	usage := nestedStringMap(snapshot, "usage")
	extraUsage := nestedStringMap(usage, "extra_usage")
	if extraUsage == nil {
		extraUsage = nestedStringMap(usage, "extraUsage")
	}
	return mapBool(extraUsage, "is_enabled", "isEnabled", "enabled")
}

func authMetadataBool(meta map[string]any, keys ...string) bool {
	if len(meta) == 0 {
		return false
	}
	for _, key := range keys {
		if authTruthy(meta[key]) {
			return true
		}
	}
	return false
}

func authMetadataMap(meta map[string]any, key string) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	return asStringMap(meta[key])
}

func nestedStringMap(meta map[string]any, key string) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	return asStringMap(meta[key])
}

func mapBool(meta map[string]any, keys ...string) bool {
	if len(meta) == 0 {
		return false
	}
	for _, key := range keys {
		if authTruthy(meta[key]) {
			return true
		}
	}
	return false
}

func asStringMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	default:
		return nil
	}
}

func authTruthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "y", "enabled", "on":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// fetchKiroModels attempts to dynamically fetch Kiro models from the API.
// If dynamic fetch fails, it falls back to static registry.GetKiroModels().
func (s *Service) fetchKiroModels(a *coreauth.Auth) []*ModelInfo {
	if a == nil {
		log.Debug("kiro: auth is nil, using static models")
		return registry.GetKiroModels()
	}

	// Extract token data from auth attributes
	tokenData := s.extractKiroTokenData(a)
	if tokenData == nil || tokenData.AccessToken == "" {
		log.Debug("kiro: no valid token data in auth, using static models")
		return registry.GetKiroModels()
	}

	// Create KiroAuth instance
	kAuth := kiroauth.NewKiroAuth(s.cfg)
	if kAuth == nil {
		log.Warn("kiro: failed to create KiroAuth instance, using static models")
		return registry.GetKiroModels()
	}

	// Use timeout context for API call
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Attempt to fetch dynamic models
	apiModels, err := kAuth.ListAvailableModels(ctx, tokenData)
	if err != nil {
		log.Warnf("kiro: failed to fetch dynamic models: %v, using static models", err)
		return registry.GetKiroModels()
	}

	if len(apiModels) == 0 {
		log.Debug("kiro: API returned no models, using static models")
		return registry.GetKiroModels()
	}

	// Convert API models to ModelInfo
	models := convertKiroAPIModels(apiModels)

	// Generate agentic variants
	models = generateKiroAgenticVariants(models)

	log.Infof("kiro: successfully fetched %d models from API (including agentic variants)", len(models))
	return models
}

// extractKiroTokenData extracts KiroTokenData from auth attributes and metadata.
// It supports both config-based tokens (stored in Attributes) and file-based tokens (stored in Metadata).
func (s *Service) extractKiroTokenData(a *coreauth.Auth) *kiroauth.KiroTokenData {
	if a == nil {
		return nil
	}

	var accessToken, profileArn, refreshToken string

	// Priority 1: Try to get from Attributes (config.yaml source)
	if a.Attributes != nil {
		accessToken = strings.TrimSpace(a.Attributes["access_token"])
		profileArn = strings.TrimSpace(a.Attributes["profile_arn"])
		refreshToken = strings.TrimSpace(a.Attributes["refresh_token"])
	}

	// Priority 2: If not found in Attributes, try Metadata (JSON file source)
	if accessToken == "" && a.Metadata != nil {
		if at, ok := a.Metadata["access_token"].(string); ok {
			accessToken = strings.TrimSpace(at)
		}
		if pa, ok := a.Metadata["profile_arn"].(string); ok {
			profileArn = strings.TrimSpace(pa)
		}
		if rt, ok := a.Metadata["refresh_token"].(string); ok {
			refreshToken = strings.TrimSpace(rt)
		}
	}

	// access_token is required
	if accessToken == "" {
		return nil
	}

	return &kiroauth.KiroTokenData{
		AccessToken:  accessToken,
		ProfileArn:   profileArn,
		RefreshToken: refreshToken,
	}
}

// convertKiroAPIModels converts Kiro API models to ModelInfo slice.
func convertKiroAPIModels(apiModels []*kiroauth.KiroModel) []*ModelInfo {
	if len(apiModels) == 0 {
		return nil
	}

	now := time.Now().Unix()
	models := make([]*ModelInfo, 0, len(apiModels))

	for _, m := range apiModels {
		if m == nil || m.ModelID == "" {
			continue
		}

		// Create model ID with kiro- prefix
		modelID := "kiro-" + normalizeKiroModelID(m.ModelID)

		info := &ModelInfo{
			ID:                  modelID,
			Object:              "model",
			Created:             now,
			OwnedBy:             "aws",
			Type:                "kiro",
			DisplayName:         formatKiroDisplayName(m.ModelName, m.RateMultiplier),
			Description:         m.Description,
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		}

		if m.MaxInputTokens > 0 {
			info.ContextLength = m.MaxInputTokens
		}

		models = append(models, info)
	}

	return models
}

// normalizeKiroModelID normalizes a Kiro model ID by converting dots to dashes
// and removing common prefixes.
func normalizeKiroModelID(modelID string) string {
	// Remove common prefixes
	modelID = strings.TrimPrefix(modelID, "anthropic.")
	modelID = strings.TrimPrefix(modelID, "amazon.")

	// Replace dots with dashes for consistency
	modelID = strings.ReplaceAll(modelID, ".", "-")

	// Replace underscores with dashes
	modelID = strings.ReplaceAll(modelID, "_", "-")

	return strings.ToLower(modelID)
}

// formatKiroDisplayName formats the display name with rate multiplier info.
func formatKiroDisplayName(modelName string, rateMultiplier float64) string {
	if modelName == "" {
		return ""
	}

	displayName := "Kiro " + modelName
	if rateMultiplier > 0 && rateMultiplier != 1.0 {
		displayName += fmt.Sprintf(" (%.1fx credit)", rateMultiplier)
	}

	return displayName
}

// generateKiroAgenticVariants generates agentic variants for Kiro models.
// Agentic variants have optimized system prompts for coding agents.
func generateKiroAgenticVariants(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}

	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		if m == nil {
			continue
		}

		// Skip if already an agentic variant
		if strings.HasSuffix(m.ID, "-agentic") {
			continue
		}

		// Skip auto models from agentic variant generation
		if strings.Contains(m.ID, "-auto") {
			continue
		}

		// Create agentic variant
		agentic := &ModelInfo{
			ID:                  m.ID + "-agentic",
			Object:              m.Object,
			Created:             m.Created,
			OwnedBy:             m.OwnedBy,
			Type:                m.Type,
			DisplayName:         m.DisplayName + " (Agentic)",
			Description:         m.Description + " - Optimized for coding agents (chunked writes)",
			ContextLength:       m.ContextLength,
			MaxCompletionTokens: m.MaxCompletionTokens,
		}

		// Copy thinking support if present
		if m.Thinking != nil {
			agentic.Thinking = &registry.ThinkingSupport{
				Min:            m.Thinking.Min,
				Max:            m.Thinking.Max,
				ZeroAllowed:    m.Thinking.ZeroAllowed,
				DynamicAllowed: m.Thinking.DynamicAllowed,
			}
		}

		result = append(result, agentic)
	}

	return result
}
