package usage

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestGPT55BuiltinPricingAndAliases(t *testing.T) {
	catalog := NewPricingCatalogManager()
	tokens := TokenStats{
		InputTokens:     2_000_000,
		OutputTokens:    1_000_000,
		CacheReadTokens: 1_000_000,
		TotalTokens:     3_000_000,
	}

	for _, model := range []string{"gpt-5.5", "gpt-5-5", "gpt-55", "GPT 5.5"} {
		got := catalog.ComputeDetailPricing(model, tokens)
		if got.State != pricingStatePriced {
			t.Fatalf("%s pricing state = %q, want priced", model, got.State)
		}
		if got.UniqueModelName != "gpt-5.5" {
			t.Fatalf("%s canonical = %q, want gpt-5.5", model, got.UniqueModelName)
		}
		if got.CostMicros != 35_500_000 {
			t.Fatalf("%s cost micros = %d, want 35500000", model, got.CostMicros)
		}
	}
}

func TestPricingCatalogPersistenceRebuildsGPT55Alias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	catalog := NewPricingCatalogManager()
	catalog.SetFetchers([]pricingFetcher{
		{
			ID:    "openai",
			Label: "OpenAI Pricing",
			URL:   "https://example.test/pricing",
			Fetch: func(context.Context, *http.Client, string) (map[string]PricingModel, error) {
				return map[string]PricingModel{
					"gpt-5.5": pricingModelFromValues("gpt-5.5", "GPT 5.5", 5, 0.5, 30, 0, pricingSourceOfficial),
				}, nil
			},
		},
	})
	if err := catalog.SetPersistencePath(path); err != nil {
		t.Fatalf("SetPersistencePath() error = %v", err)
	}
	if err := catalog.RefreshOfficial(context.Background()); err != nil {
		t.Fatalf("RefreshOfficial() error = %v", err)
	}
	snapshot := catalog.Snapshot(nil)
	if snapshot.Official.LastRefreshedAt == nil {
		t.Fatal("LastRefreshedAt is nil after successful refresh")
	}
	if snapshot.Official.PersistedAt == nil {
		t.Fatal("PersistedAt is nil after successful refresh")
	}

	restored := NewPricingCatalogManager()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatalf("restored.SetPersistencePath() error = %v", err)
	}
	restoredSnapshot := restored.Snapshot(nil)
	if restoredSnapshot.Official.LastRefreshedAt == nil {
		t.Fatal("restored LastRefreshedAt is nil")
	}
	if restoredSnapshot.Official.PersistedAt == nil {
		t.Fatal("restored PersistedAt is nil")
	}
	got := restored.ComputeDetailPricing("gpt-55", TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000})
	if got.State != pricingStatePriced {
		t.Fatalf("restored gpt-55 state = %q, want priced", got.State)
	}
	if got.Source != pricingSourceOfficial {
		t.Fatalf("restored gpt-55 source = %q, want official", got.Source)
	}
}

func TestPricingSnapshotOmitsZeroPersistenceTimes(t *testing.T) {
	catalog := NewPricingCatalogManager()
	snapshot := catalog.Snapshot(nil)
	if snapshot.Official.LastRefreshedAt != nil {
		t.Fatalf("LastRefreshedAt = %v, want nil for zero time", snapshot.Official.LastRefreshedAt)
	}
	if snapshot.Official.PersistedAt != nil {
		t.Fatalf("PersistedAt = %v, want nil for zero time", snapshot.Official.PersistedAt)
	}
}

func TestPricingAutoRefreshDelayUsesInitialJitterWhenEmpty(t *testing.T) {
	catalog := NewPricingCatalogManager()
	now := time.Date(2026, 5, 20, 6, 30, 0, 123456789, time.UTC)

	delay := catalog.nextAutoRefreshDelay(now, time.Hour)
	if delay < 0 || delay >= pricingInitialJitterMax {
		t.Fatalf("initial delay = %s, want [0,%s)", delay, pricingInitialJitterMax)
	}
}
