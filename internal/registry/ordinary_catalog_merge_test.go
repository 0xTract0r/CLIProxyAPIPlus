package registry

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOrdinaryCatalogExplicitSourcePreservesMissingSections(t *testing.T) {
	old := getModels()
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &staticModelsJSON{
		Claude:    []*ModelInfo{{ID: "fixture-claude"}},
		CodexFree: []*ModelInfo{{ID: "gpt-5.4"}},
		CodexTeam: []*ModelInfo{{ID: "gpt-5.5"}},
		CodexPlus: []*ModelInfo{{ID: "gpt-5.5"}},
		Meta:      []*ModelInfo{{ID: "fixture-meta"}},
	}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = old
		modelsCatalogStore.mu.Unlock()
	})

	source := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(source, []byte(`{"gemini":[{"id":"fixture-gemini"}],"copilot":[{"id":"fixture-copilot"}],"codex-team":[{"id":"fixture-current","description":"current upstream metadata"}],"codex-plus":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	data, usedSource, err := generalCatalogUpdater.fetchCatalog(context.Background(), source)
	if err != nil || usedSource != source {
		t.Fatalf("source = %q, err = %v", usedSource, err)
	}
	changed, err := generalCatalogUpdater.publish(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gemini", "gemini-interactions", "copilot"} {
		if !slices.Contains(changed, want) {
			t.Errorf("missing notification for %s: %v", want, changed)
		}
	}
	if got := LookupStaticModelInfoByChannel("fixture-gemini", "gemini-interactions"); got == nil {
		t.Fatal("official Gemini Interactions lookup lost the explicit source")
	}
	if got := LookupStaticModelInfoByChannel("fixture-copilot", "copilot"); got == nil {
		t.Fatal("official Copilot lookup lost the explicit source")
	}
	if got := LookupStaticModelInfoByChannel("fixture-claude", "claude"); got == nil {
		t.Fatal("production fallback lost a missing Claude section")
	}
	if got := LookupStaticModelInfoByChannel("fixture-meta", "meta"); got == nil {
		t.Fatal("official fallback lost the missing Meta section")
	}
	model := getModels().CodexFree[0]
	if !slices.Contains(model.SupportedParameters, "service_tier") ||
		!slices.Contains(model.AdditionalSpeedTiers, "fast") {
		t.Fatalf("production Fast metadata missing: %+v", model)
	}

	team := getModels().CodexTeam
	if len(team) != 1 || team[0].ID != "fixture-current" || team[0].Description != "current upstream metadata" {
		t.Fatalf("explicit team catalog changed: %+v", team)
	}
	if plus := getModels().CodexPlus; plus == nil || len(plus) != 0 {
		t.Fatalf("explicit empty section was not preserved: %+v", plus)
	}

	previous := getModels()
	if err := os.WriteFile(source, []byte(`{"gemini":[{"id":"duplicate"},{"id":"duplicate"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generalCatalogUpdater.fetchCatalog(context.Background(), source); err == nil {
		t.Fatal("invalid explicit source accepted")
	}
	if getModels() != previous {
		t.Fatal("failed fetch changed published model definitions")
	}
}

func TestOrdinaryCatalogFetchProxyUsesExplicitConfiguration(t *testing.T) {
	modelsFetchConfig.mu.RLock()
	previous := modelsFetchConfig.proxyURL
	modelsFetchConfig.mu.RUnlock()
	t.Cleanup(func() { SetModelsFetchProxy(previous) })
	req, err := http.NewRequest(http.MethodGet, "https://catalog.invalid/models.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	SetModelsFetchProxy("http://127.0.0.1:33445")
	client := newModelsFetchClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("ordinary catalog does not use the explicit proxy")
	}
	got, err := transport.Proxy(req)
	if err != nil || got == nil || got.Host != "127.0.0.1:33445" {
		t.Fatalf("catalog proxy = %v, %v", got, err)
	}
	SetModelsFetchProxy("direct")
	client = newModelsFetchClient()
	transport, ok = client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("explicit direct catalog mode must use no proxy")
	}
}
