package usage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestNormalizeCanonicalModelIDExamples(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"gpt-5.4 (openai)":              "gpt-5.4",
		"gpt-5.2-codex(xhigh)":          "gpt-5.2-codex",
		"claude-opus-4.7":               "claude-opus-4-7",
		"claude-opus-4-7-20251001":      "claude-opus-4-7",
		"claude-opus-4-7-latest":        "claude-opus-4-7",
		"chatgpt.com/gpt-5.4":           "gpt-5.4",
		"gpt-5.3-codex-spark(thinking)": "gpt-5.3-codex-spark",
	}

	for input, want := range cases {
		if got := NormalizeCanonicalModelID(input); got != want {
			t.Fatalf("NormalizeCanonicalModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRefreshOfficialPartialFailurePreservesExistingCatalog(t *testing.T) {
	t.Parallel()

	manager := NewPricingCatalogManager()
	manager.mu.Lock()
	manager.officialModels["gpt-5.4"] = PricingModel{
		Model:          "gpt-5.4",
		DisplayName:    "gpt-5.4",
		CanonicalModel: "gpt-5.4",
		Source:         pricingSourceOfficial,
		SourceID:       "openai",
	}
	manager.officialModels["claude-opus-4-7"] = PricingModel{
		Model:          "claude-opus-4-7",
		DisplayName:    "Claude Opus 4.7",
		CanonicalModel: "claude-opus-4-7",
		Source:         pricingSourceOfficial,
		SourceID:       "anthropic",
	}
	manager.sources["openai"] = PricingSourceInfo{ID: "openai", Label: "OpenAI Pricing"}
	manager.sources["anthropic"] = PricingSourceInfo{ID: "anthropic", Label: "Anthropic Pricing"}
	manager.rebuildAliasIndexLocked()
	manager.mu.Unlock()

	manager.SetFetchers([]pricingFetcher{
		{
			ID:    "openai",
			Label: "OpenAI Pricing",
			URL:   "https://example.invalid/openai",
			Fetch: func(context.Context, *http.Client, string) (map[string]PricingModel, error) {
				return map[string]PricingModel{
					"gpt-5.4": pricingModelFromValues("gpt-5.4", "gpt-5.4", 2.75, 0.3, 16, 0, pricingSourceOfficial),
				}, nil
			},
		},
		{
			ID:    "anthropic",
			Label: "Anthropic Pricing",
			URL:   "https://example.invalid/anthropic",
			Fetch: func(context.Context, *http.Client, string) (map[string]PricingModel, error) {
				return nil, errors.New("upstream unavailable")
			},
		},
	})

	err := manager.RefreshOfficial(context.Background())
	if err == nil {
		t.Fatal("RefreshOfficial() error = nil, want partial failure")
	}

	snapshot := manager.Snapshot(nil)
	openAI := snapshot.Models["gpt-5.4"]
	if openAI.InputUSDPerMTok != 2.75 {
		t.Fatalf("openai model not refreshed, input price = %v", openAI.InputUSDPerMTok)
	}
	anthropic := snapshot.Models["claude-opus-4-7"]
	if anthropic.Model == "" {
		t.Fatal("anthropic model missing after partial failure")
	}
	if anthropic.Source != pricingSourceOfficial {
		t.Fatalf("anthropic source = %q, want %q", anthropic.Source, pricingSourceOfficial)
	}
	if snapshot.Official.LastRefreshedAt.IsZero() {
		t.Fatal("LastRefreshedAt is zero after partial success")
	}

	var anthropicStatus string
	for _, source := range snapshot.Official.Sources {
		if source.ID == "anthropic" {
			anthropicStatus = source.Status
		}
	}
	if anthropicStatus != pricingSourceStatusError {
		t.Fatalf("anthropic source status = %q, want %q", anthropicStatus, pricingSourceStatusError)
	}
}

func TestConfigureDefaultPricingCatalogPersistenceRecalculatesDefaultStatistics(t *testing.T) {
	tempDir := t.TempDir()
	pricingPath := filepath.Join(tempDir, "pricing.json")

	previousStats := defaultRequestStatistics
	previousCatalog := defaultPricingCatalog

	testCatalog := NewPricingCatalogManager()
	testStats := NewRequestStatisticsWithCatalog(testCatalog)
	defaultPricingCatalog = testCatalog
	defaultRequestStatistics = testStats
	t.Cleanup(func() {
		defaultPricingCatalog = previousCatalog
		defaultRequestStatistics = previousStats
	})

	testStats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.3-codex-spark",
		RequestedAt: time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
	})

	manualCatalog := pricingCatalogFile{
		Version:         pricingCatalogFileVersion,
		PersistedAt:     time.Now().UTC(),
		LastRefreshedAt: time.Now().UTC(),
		Overrides: map[string]PricingModel{
			"gpt-5.3-codex-spark": {
				Model:                 "gpt-5.3-codex-spark",
				DisplayName:           "gpt-5.3-codex-spark",
				InputUSDPerMTok:       1.75,
				CachedInputUSDPerMTok: 0.175,
				OutputUSDPerMTok:      14,
				CacheWriteUSDPerMTok:  0,
			},
		},
	}
	data, err := json.Marshal(manualCatalog)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(pricingPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := ConfigureDefaultPricingCatalogPersistence(pricingPath); err != nil {
		t.Fatalf("ConfigureDefaultPricingCatalogPersistence() error = %v", err)
	}

	snapshot := testStats.Snapshot()
	if snapshot.TotalCostUSD <= 0 {
		t.Fatalf("TotalCostUSD = %f, want > 0", snapshot.TotalCostUSD)
	}
	if snapshot.PricingStatus != "" {
		t.Fatalf("PricingStatus = %q, want empty", snapshot.PricingStatus)
	}
	if snapshot.UnfinalizedRequestCount != 0 {
		t.Fatalf("UnfinalizedRequestCount = %d, want 0", snapshot.UnfinalizedRequestCount)
	}
}
