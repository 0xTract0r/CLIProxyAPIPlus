package registry

import "testing"

func TestOrdinaryRegistryHasNoLegacyKiroCatalogPublication(t *testing.T) {
	for _, item := range []struct {
		channel string
		modelID string
	}{
		{"kiro", "kiro-auto"},
		{"amazonq", "amazonq-auto"},
	} {
		if got := GetStaticModelDefinitionsByChannel(item.channel); len(got) != 0 {
			t.Errorf("legacy channel %q published %d default entries", item.channel, len(got))
		}
		if got := LookupStaticModelInfoByChannel(item.modelID, item.channel); got != nil {
			t.Errorf("channel lookup exposed former default %q", item.modelID)
		}
		if got := LookupStaticModelInfo(item.modelID); got != nil {
			t.Errorf("global lookup exposed former default %q", item.modelID)
		}
	}
	for _, channel := range []string{"github-copilot", "kimi", "gemini-interactions"} {
		if got := GetStaticModelDefinitionsByChannel(channel); len(got) == 0 {
			t.Errorf("ordinary channel %q no longer publishes its model catalog", channel)
		}
	}
}
