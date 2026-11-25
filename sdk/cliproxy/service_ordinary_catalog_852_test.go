package cliproxy

import "testing"

func TestOrdinaryPublishedModelExclusionUsesCanonicalMetadata(t *testing.T) {
	models := []*ModelInfo{
		{ID: "allowed", MetadataModelID: "copilot/gpt-other"},
		{ID: "alias-to-blocked", MetadataModelID: "copilot/gpt-blocked"},
		{ID: "direct-hidden", MetadataModelID: "native"},
		{ID: "devin/swe-1-6-slow", MetadataModelID: "swe-1-6-slow"},
	}
	got := applyExcludedModels(models, []string{"copilot/gpt-block*", "direct-hidden"})
	if len(got) != 2 || got[0].ID != "allowed" || got[1].ID != "devin/swe-1-6-slow" {
		t.Fatalf("wrong surviving models: %+v", got)
	}
}
