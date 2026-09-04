package auth

import "testing"

func TestAuth_AccountTierSource(t *testing.T) {
	claudeQuota := func(tier string) map[string]any {
		return map[string]any{
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": tier},
				},
			},
		}
	}

	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{name: "nil auth -> auto", auth: nil, want: TierSourceAuto},
		{
			name: "claude auto-detected tier, no override -> auto",
			auth: &Auth{Provider: "claude", Metadata: claudeQuota("default_claude_max_20x")},
			want: TierSourceAuto,
		},
		{
			name: "claude with legacy bare tier_override -> override",
			auth: &Auth{Provider: "claude", Metadata: map[string]any{TierOverrideMetadataKey: "max_5x"}},
			want: TierSourceOverride,
		},
		{
			name: "claude with namespaced tier_override -> override",
			auth: &Auth{Provider: "claude", Metadata: map[string]any{
				AccountSchedulingMetadataKey: map[string]any{TierOverrideMetadataKey: "max_20x"},
			}},
			want: TierSourceOverride,
		},
		{
			name: "claude with codex-scoped override is ignored -> auto",
			auth: &Auth{Provider: "claude", Metadata: map[string]any{TierOverrideMetadataKey: "codex_pro"}},
			want: TierSourceAuto,
		},
		{
			name: "claude with garbage override is ignored -> auto",
			auth: &Auth{Provider: "claude", Metadata: map[string]any{TierOverrideMetadataKey: "bogus"}},
			want: TierSourceAuto,
		},
		{
			name: "codex with codex override -> override",
			auth: &Auth{Provider: "codex", Metadata: map[string]any{TierOverrideMetadataKey: "codex_pro"}},
			want: TierSourceOverride,
		},
		{
			name: "codex with claude-scoped override is ignored -> auto",
			auth: &Auth{Provider: "codex", Metadata: map[string]any{TierOverrideMetadataKey: "max_20x"}},
			want: TierSourceAuto,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.auth.AccountTierSource(); got != tc.want {
				t.Fatalf("AccountTierSource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTierOverride_DualRead pins the §8.5 dual-read contract for tier_override:
// the namespaced sub-key wins, the legacy bare key is honored as a fallback, and
// the resolved override drives ClaudeSubscriptionTier.
func TestTierOverride_DualRead(t *testing.T) {
	t.Run("namespaced sub-key wins over legacy bare key", func(t *testing.T) {
		auth := &Auth{Provider: "claude", Metadata: map[string]any{
			TierOverrideMetadataKey:      "pro",
			AccountSchedulingMetadataKey: map[string]any{TierOverrideMetadataKey: "max_20x"},
		}}
		if got := auth.ClaudeSubscriptionTier(); got != ClaudeMax20x {
			t.Fatalf("ClaudeSubscriptionTier() = %v, want ClaudeMax20x (namespaced wins)", got)
		}
	})
	t.Run("falls back to legacy bare key when namespaced absent", func(t *testing.T) {
		auth := &Auth{Provider: "claude", Metadata: map[string]any{TierOverrideMetadataKey: "max_5x"}}
		if got := auth.ClaudeSubscriptionTier(); got != ClaudeMax5x {
			t.Fatalf("ClaudeSubscriptionTier() = %v, want ClaudeMax5x (legacy fallback)", got)
		}
	})
	t.Run("no override anywhere falls to auto-detection", func(t *testing.T) {
		auth := &Auth{Provider: "claude", Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "default_claude_pro"},
				},
			},
		}}
		if got := auth.ClaudeSubscriptionTier(); got != ClaudePro {
			t.Fatalf("ClaudeSubscriptionTier() = %v, want ClaudePro (auto-detected)", got)
		}
	})
}

func TestSetAccountSchedulingValue(t *testing.T) {
	t.Run("creates the object when absent", func(t *testing.T) {
		meta := map[string]any{"note": "AC-14"}
		setAccountSchedulingValue(meta, FirstProductionAtMetadataKey, "2026-01-01T00:00:00Z")
		obj, ok := meta[AccountSchedulingMetadataKey].(map[string]any)
		if !ok {
			t.Fatalf("account_scheduling object not created: %#v", meta)
		}
		if obj[FirstProductionAtMetadataKey] != "2026-01-01T00:00:00Z" {
			t.Fatalf("sub-key not written: %#v", obj)
		}
		if meta["note"] != "AC-14" {
			t.Fatalf("sibling top-level key clobbered: %#v", meta)
		}
	})

	t.Run("preserves existing sub-keys on a map[string]any object", func(t *testing.T) {
		meta := map[string]any{
			AccountSchedulingMetadataKey: map[string]any{"tier_override": "max_20x"},
		}
		setAccountSchedulingValue(meta, accountSchedulingRateScaleKey, 0.5)
		obj := meta[AccountSchedulingMetadataKey].(map[string]any)
		if obj["tier_override"] != "max_20x" {
			t.Fatalf("existing sub-key dropped: %#v", obj)
		}
		if obj[accountSchedulingRateScaleKey] != 0.5 {
			t.Fatalf("new sub-key not written: %#v", obj)
		}
	})

	t.Run("materializes a non-map[string]any shape into a writable object", func(t *testing.T) {
		// A foreign write could store the object as map[string]string; the write
		// must still persist (carrying the existing sub-key over) via a
		// reattached map[string]any.
		meta := map[string]any{
			AccountSchedulingMetadataKey: map[string]string{"tier_override": "max_5x"},
		}
		setAccountSchedulingValue(meta, FirstProductionAtMetadataKey, "2026-02-02T00:00:00Z")
		obj, ok := meta[AccountSchedulingMetadataKey].(map[string]any)
		if !ok {
			t.Fatalf("object not materialized to map[string]any: %#v", meta[AccountSchedulingMetadataKey])
		}
		if obj["tier_override"] != "max_5x" {
			t.Fatalf("existing sub-key not carried over: %#v", obj)
		}
		if obj[FirstProductionAtMetadataKey] != "2026-02-02T00:00:00Z" {
			t.Fatalf("new sub-key not written: %#v", obj)
		}
		// Read-back through the dual-read helper confirms the write is visible.
		if raw, ok := accountSchedulingRawValue(meta, FirstProductionAtMetadataKey); !ok || raw != "2026-02-02T00:00:00Z" {
			t.Fatalf("accountSchedulingRawValue after materialized write = (%#v,%v)", raw, ok)
		}
	})
}
