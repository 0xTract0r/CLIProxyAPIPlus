package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestOrdinaryDevinRetainsSelectedCredentialAfterCatalogChange(t *testing.T) {
	const selected = "devin-retained-selected"
	const alternative = "devin-retained-alternative"
	const original = "devin-session-route(high)"
	const different = "unrelated-session-route"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(selected, "devin", []*registry.ModelInfo{{ID: original}})
	reg.RegisterClient(alternative, "devin", []*registry.ModelInfo{{ID: original}})
	reg.RegisterClient(different, "openai", []*registry.ModelInfo{{ID: different}})
	defer reg.UnregisterClient(selected)
	defer reg.UnregisterClient(alternative)
	defer reg.UnregisterClient(different)

	pins := map[string]responsesWebsocketPinnedAuthState{
		"devin": {authID: selected, modelKey: original, localDevin: true},
	}
	reg.UnregisterClient(selected)
	if got := responsesWebsocketRetainedDevinPin(pins, original); got != selected {
		t.Errorf("continuation changed existing credential after catalog change: %q", got)
	}
	reg.UnregisterClient(alternative)
	if got := responsesWebsocketRetainedDevinPin(pins, original); got != selected {
		t.Errorf("exact continuation lost its pin after route disappeared: %q", got)
	}
	if got := responsesWebsocketRetainedDevinPin(pins, different); got != "" {
		t.Errorf("Devin binding leaked to an unrelated provider: %q", got)
	}
}
