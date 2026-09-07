package auth

import (
	"encoding/json"
	"reflect"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestNormalizeTierOverride(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		value        string
		wantOK       bool
		wantNormaliz string
	}{
		{name: "claude max_5x", provider: "claude", value: "max_5x", wantOK: true, wantNormaliz: "max_5x"},
		{name: "claude trims+lowercases", provider: "Claude", value: "  MAX_20X ", wantOK: true, wantNormaliz: "max_20x"},
		{name: "claude pro", provider: "claude", value: "pro", wantOK: true, wantNormaliz: "pro"},
		{name: "codex pro", provider: "codex", value: "codex_pro", wantOK: true, wantNormaliz: "codex_pro"},
		{name: "codex plus", provider: "codex", value: "codex_plus", wantOK: true, wantNormaliz: "codex_plus"},
		{name: "claude value on codex rejected", provider: "codex", value: "max_5x", wantOK: false},
		{name: "codex value on claude rejected", provider: "claude", value: "codex_pro", wantOK: false},
		{name: "garbage rejected", provider: "claude", value: "enterprise", wantOK: false},
		{name: "blank rejected", provider: "claude", value: "   ", wantOK: false},
		{name: "unknown provider rejected", provider: "gemini", value: "pro", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeTierOverride(tc.provider, tc.value)
			if ok != tc.wantOK {
				t.Fatalf("NormalizeTierOverride(%q,%q) ok = %v, want %v", tc.provider, tc.value, ok, tc.wantOK)
			}
			if ok && got != tc.wantNormaliz {
				t.Fatalf("NormalizeTierOverride(%q,%q) = %q, want %q", tc.provider, tc.value, got, tc.wantNormaliz)
			}
		})
	}
}

func TestLegalTierOverrideValues(t *testing.T) {
	if got, want := LegalTierOverrideValues("claude"), []string{"max_20x", "max_5x", "pro"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LegalTierOverrideValues(claude) = %v, want %v", got, want)
	}
	if got, want := LegalTierOverrideValues("codex"), []string{"codex_plus", "codex_pro"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LegalTierOverrideValues(codex) = %v, want %v", got, want)
	}
	if got := LegalTierOverrideValues("gemini"); got != nil {
		t.Fatalf("LegalTierOverrideValues(gemini) = %v, want nil", got)
	}
}

func TestSetAndClearAccountTierOverride(t *testing.T) {
	t.Run("set writes namespaced object and drives tier source/tier", func(t *testing.T) {
		auth := &Auth{Provider: "claude"}
		auth.SetAccountTierOverride("max_5x")

		obj, ok := auth.Metadata[AccountSchedulingMetadataKey].(map[string]any)
		if !ok {
			t.Fatalf("account_scheduling object not created: %#v", auth.Metadata)
		}
		if obj[TierOverrideMetadataKey] != "max_5x" {
			t.Fatalf("tier_override not written to namespaced object: %#v", obj)
		}
		if src := auth.AccountTierSource(); src != TierSourceOverride {
			t.Fatalf("AccountTierSource() = %q, want %q", src, TierSourceOverride)
		}
		if tier := auth.ClaudeSubscriptionTier(); tier != ClaudeMax5x {
			t.Fatalf("ClaudeSubscriptionTier() = %v, want ClaudeMax5x", tier)
		}
	})

	t.Run("clear removes BOTH namespaced sub-key and legacy bare key", func(t *testing.T) {
		// Seed a stale legacy bare key alongside the namespaced value so we can
		// prove the clear does not let the dual-read fallback resurrect it.
		auth := &Auth{Provider: "claude", Metadata: map[string]any{
			TierOverrideMetadataKey:      "pro",
			AccountSchedulingMetadataKey: map[string]any{TierOverrideMetadataKey: "max_20x"},
		}}
		auth.ClearAccountTierOverride()

		if _, present := auth.Metadata[TierOverrideMetadataKey]; present {
			t.Fatalf("legacy bare tier_override not cleared: %#v", auth.Metadata)
		}
		if _, present := auth.Metadata[AccountSchedulingMetadataKey]; present {
			t.Fatalf("empty account_scheduling object should be dropped: %#v", auth.Metadata)
		}
		if src := auth.AccountTierSource(); src != TierSourceAuto {
			t.Fatalf("AccountTierSource() after clear = %q, want %q", src, TierSourceAuto)
		}
	})

	t.Run("clear preserves sibling scheduling sub-keys", func(t *testing.T) {
		auth := &Auth{Provider: "claude", Metadata: map[string]any{
			AccountSchedulingMetadataKey: map[string]any{
				TierOverrideMetadataKey:       "max_20x",
				accountSchedulingRateScaleKey: 0.25,
			},
		}}
		auth.ClearAccountTierOverride()

		obj, ok := auth.Metadata[AccountSchedulingMetadataKey].(map[string]any)
		if !ok {
			t.Fatalf("account_scheduling object dropped despite surviving sibling: %#v", auth.Metadata)
		}
		if _, present := obj[TierOverrideMetadataKey]; present {
			t.Fatalf("tier_override not cleared: %#v", obj)
		}
		if obj[accountSchedulingRateScaleKey] != 0.25 {
			t.Fatalf("sibling rate_scale sub-key clobbered: %#v", obj)
		}
	})
}

func TestParseRateScaleValue(t *testing.T) {
	tests := []struct {
		name   string
		raw    any
		want   float64
		wantOK bool
	}{
		{name: "float64", raw: float64(0.5), want: 0.5, wantOK: true},
		{name: "json.Number", raw: json.Number("2.5"), want: 2.5, wantOK: true},
		{name: "int", raw: 3, want: 3, wantOK: true},
		{name: "numeric string", raw: "1.25", want: 1.25, wantOK: true},
		{name: "zero rejected", raw: float64(0), wantOK: false},
		{name: "negative rejected", raw: float64(-1), wantOK: false},
		{name: "non-numeric string rejected", raw: "fast", wantOK: false},
		{name: "nil rejected", raw: nil, wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateScaleValue(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ParseRateScaleValue(%#v) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseRateScaleValue(%#v) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSetAndClearAccountRateScale(t *testing.T) {
	cfg := internalconfig.AccountSchedulingConfig{RateScale: 0} // no config default -> fall back to 1.0

	t.Run("set makes AccountRateScale return the override", func(t *testing.T) {
		auth := &Auth{Provider: "claude"}
		auth.SetAccountRateScale(0.5)
		if got := AccountRateScale(auth, cfg); got != 0.5 {
			t.Fatalf("AccountRateScale after set = %v, want 0.5", got)
		}
	})

	t.Run("clear removes both keys and falls back to config default then 1.0", func(t *testing.T) {
		auth := &Auth{Provider: "claude", Metadata: map[string]any{
			accountSchedulingRateScaleKey: 0.5,
			AccountSchedulingMetadataKey:  map[string]any{accountSchedulingRateScaleKey: 0.25},
		}}
		auth.ClearAccountRateScale()

		if _, present := auth.Metadata[accountSchedulingRateScaleKey]; present {
			t.Fatalf("legacy bare rate_scale not cleared: %#v", auth.Metadata)
		}
		if _, present := auth.Metadata[AccountSchedulingMetadataKey]; present {
			t.Fatalf("empty account_scheduling object should be dropped: %#v", auth.Metadata)
		}
		if got := AccountRateScale(auth, cfg); got != 1.0 {
			t.Fatalf("AccountRateScale after clear (no config default) = %v, want 1.0", got)
		}
		if got := AccountRateScale(auth, internalconfig.AccountSchedulingConfig{RateScale: 2.0}); got != 2.0 {
			t.Fatalf("AccountRateScale after clear (with config default) = %v, want 2.0", got)
		}
	})
}
