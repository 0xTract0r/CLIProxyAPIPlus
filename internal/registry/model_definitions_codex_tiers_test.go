package registry

import "testing"

func containsModelID(models []*ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}

func TestGetCodexPlusModelsExcludesSpark(t *testing.T) {
	if containsModelID(GetCodexPlusModels(), "gpt-5.3-codex-spark") {
		t.Fatalf("expected codex-plus tier to exclude gpt-5.3-codex-spark")
	}
}

func TestGetCodexProModelsKeepsSpark(t *testing.T) {
	if !containsModelID(GetCodexProModels(), "gpt-5.3-codex-spark") {
		t.Fatalf("expected codex-pro tier to keep gpt-5.3-codex-spark")
	}
}
