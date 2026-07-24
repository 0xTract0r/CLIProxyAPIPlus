package usage

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
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

func TestNormalizeCanonicalModelIDKeepsVariantsSeparate(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Canonical base tiers and their benign aliases still fold as before.
		{"gpt-5.5", "gpt-5.5"},
		{"gpt-5-5", "gpt-5.5"},
		{"GPT 5.5", "gpt-5.5"},
		{"gpt-5.5-20260101", "gpt-5.5"}, // trailing date snapshot folds
		{"gpt-5.5-latest", "gpt-5.5"},   // snapshot alias folds
		{"gpt-5.4", "gpt-5.4"},
		{"gpt-5.2-codex", "gpt-5.2-codex"},
		{"gpt-5.3-codex", "gpt-5.3-codex"},
		{"gpt-5.3-codex-spark", "gpt-5.3-codex-spark"},
		{"claude-opus-4-7", "claude-opus-4-7"},
		// Distinct variant tiers must NOT collapse onto the base canonical.
		{"gpt-5.5-cyber", "gpt-5-5-cyber"},
		{"gpt-5.4-cyber", "gpt-5-4-cyber"},
		{"gpt-5.2-codex-preview", "gpt-5-2-codex-preview"},
	}
	for _, tc := range cases {
		if got := normalizeCanonicalModelID(tc.in); got != tc.want {
			t.Fatalf("normalizeCanonicalModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The variant must never share the canonical id of the base tier, otherwise
	// its pricing row can shadow the correct base tier while scraping.
	if normalizeCanonicalModelID("gpt-5.5-cyber") == normalizeCanonicalModelID("gpt-5.5") {
		t.Fatal("gpt-5.5-cyber collapsed onto canonical gpt-5.5")
	}
}

func TestParseOpenAIPricingHTMLVariantDoesNotOverwriteCanonical(t *testing.T) {
	// The cyber variant row appears BEFORE the canonical gpt-5.5 row. With the
	// old greedy folding, the variant collapsed onto "gpt-5.5"; because the
	// parser keeps the first row seen per canonical, the true 5/0.5/30 tier was
	// dropped and the ~12.5/75 variant price shadowed the canonical model.
	body := strings.Join([]string{
		`[[0,"gpt-5.5-cyber"],[0,12.5],[0,1.25],[0,75]]`,
		`[[0,"gpt-5.5"],[0,5],[0,0.5],[0,30]]`,
	}, "\n")

	models, err := parseOpenAIPricingHTML(body)
	if err != nil {
		t.Fatalf("parseOpenAIPricingHTML() error = %v", err)
	}

	base, ok := models["gpt-5.5"]
	if !ok {
		t.Fatal("canonical gpt-5.5 missing from scraped catalog")
	}
	if base.InputUSDPerMTok != 5 || base.CachedInputUSDPerMTok != 0.5 || base.OutputUSDPerMTok != 30 {
		t.Fatalf("gpt-5.5 pricing = in %v / cached %v / out %v, want 5 / 0.5 / 30",
			base.InputUSDPerMTok, base.CachedInputUSDPerMTok, base.OutputUSDPerMTok)
	}

	variant, ok := models["gpt-5-5-cyber"]
	if !ok {
		t.Fatal("variant gpt-5.5-cyber missing; it must keep its own canonical id")
	}
	if variant.InputUSDPerMTok != 12.5 || variant.OutputUSDPerMTok != 75 {
		t.Fatalf("gpt-5.5-cyber pricing = in %v / out %v, want 12.5 / 75",
			variant.InputUSDPerMTok, variant.OutputUSDPerMTok)
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
