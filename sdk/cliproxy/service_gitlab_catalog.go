package cliproxy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// gitLabModelsFromAuth reads the previously stored model list locally.
func gitLabModelsFromAuth(auth *cliproxyauth.Auth) []*registry.ModelInfo {
	models := make([]*registry.ModelInfo, 0, 4)
	seen := make(map[string]struct{}, 4)
	addModel := func(id, displayName, provider string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     time.Now().Unix(),
			OwnedBy:     "gitlab",
			Type:        "gitlab",
			DisplayName: displayName,
			Description: provider,
			UserDefined: true,
		})
	}

	addModel("gitlab-duo", "GitLab Duo", "gitlab")
	if auth == nil {
		return models
	}
	for _, model := range extractGitLabCatalogModels(auth.Metadata) {
		name := strings.TrimSpace(model.ModelName)
		if name == "" {
			continue
		}
		displayName := "GitLab Duo"
		if provider := strings.TrimSpace(model.ModelProvider); provider != "" {
			displayName = fmt.Sprintf("GitLab Duo (%s)", provider)
		}
		addModel(name, displayName, strings.TrimSpace(model.ModelProvider))
	}
	return models
}

type gitLabCatalogModel struct {
	ModelProvider string
	ModelName     string
}

func extractGitLabCatalogModels(metadata map[string]any) []gitLabCatalogModel {
	if len(metadata) == 0 {
		return nil
	}

	models := make([]gitLabCatalogModel, 0, 4)
	seen := make(map[string]struct{})
	appendModel := func(provider, name string) {
		provider = strings.TrimSpace(provider)
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, gitLabCatalogModel{
			ModelProvider: provider,
			ModelName:     name,
		})
	}

	if raw, ok := metadata["model_details"]; ok {
		appendgitLabCatalogModels(raw, appendModel)
	}
	appendModel(gitLabCatalogString(metadata["model_provider"]), gitLabCatalogString(metadata["model_name"]))

	for _, key := range []string{"models", "supported_models", "discovered_models"} {
		if raw, ok := metadata[key]; ok {
			appendgitLabCatalogModels(raw, appendModel)
		}
	}

	return models
}
func appendgitLabCatalogModels(raw any, appendModel func(provider, name string)) {
	switch typed := raw.(type) {
	case map[string]any:
		appendModel(gitLabCatalogString(typed["model_provider"]), gitLabCatalogString(typed["model_name"]))
		appendModel(gitLabCatalogString(typed["provider"]), gitLabCatalogString(typed["name"]))
		if nested, ok := typed["models"]; ok {
			appendgitLabCatalogModels(nested, appendModel)
		}
	case []any:
		for _, item := range typed {
			appendgitLabCatalogModels(item, appendModel)
		}
	case []string:
		for _, item := range typed {
			appendModel("", item)
		}
	case string:
		appendModel("", typed)
	}
}
func gitLabCatalogString(raw any) string {
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}
