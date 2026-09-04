package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestAuth_ClaudeSubscriptionTier(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want ClaudeTier
	}{
		{
			name: "nil auth is unknown",
			auth: nil,
			want: ClaudeTierUnknown,
		},
		{
			name: "empty auth is unknown",
			auth: &Auth{},
			want: ClaudeTierUnknown,
		},
		{
			// Real production shape (design.md §1.2/1.3: bcd898/APUS-01).
			name: "production max 20x profile",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"account": map[string]any{"has_claude_max": true},
						"organization": map[string]any{
							"rate_limit_tier":         "default_claude_max_20x",
							"subscription_created_at": "2026-03-31T17:41:42Z",
						},
					},
				},
			}},
			want: ClaudeMax20x,
		},
		{
			// Real production shape (design.md §1.2: grassorich543/APUS-03,
			// dormant standby).
			name: "production max 5x profile",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "default_claude_max_5x",
						},
					},
				},
			}},
			want: ClaudeMax5x,
		},
		{
			name: "pro profile",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "default_claude_pro",
						},
					},
				},
			}},
			want: ClaudePro,
		},
		{
			name: "case-insensitive and whitespace tolerant",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "  Default_Claude_Max_20X  ",
						},
					},
				},
			}},
			want: ClaudeMax20x,
		},
		{
			name: "missing rate_limit_tier key falls back to unknown, not a guess",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"subscription_status": "active",
						},
					},
				},
			}},
			want: ClaudeTierUnknown,
		},
		{
			name: "missing organization object falls back to unknown",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"subscription": map[string]any{"has_claude_max": true},
					},
				},
			}},
			want: ClaudeTierUnknown,
		},
		{
			name: "missing quota_snapshot entirely falls back to unknown",
			auth: &Auth{Metadata: map[string]any{
				"plan_type": "max",
			}},
			want: ClaudeTierUnknown,
		},
		{
			name: "unrecognized rate_limit_tier value is unknown, not misjudged into a tier",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "default_claude_team_seat",
						},
					},
				},
			}},
			want: ClaudeTierUnknown,
		},
		{
			name: "empty string rate_limit_tier is unknown",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{
							"rate_limit_tier": "",
						},
					},
				},
			}},
			want: ClaudeTierUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.ClaudeSubscriptionTier(); got != tt.want {
				t.Fatalf("ClaudeSubscriptionTier() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuth_ClaudeSubscriptionTier_DoesNotFold pins down the spec.md
// requirement that this read path SHALL NOT collapse through the existing
// NormalizeClaudeSubscriptionPlan folding function: two accounts that
// SubscriptionPlanType()/NormalizeClaudeSubscriptionPlan both fold to the
// same coarse "max" string must still be distinguishable via
// ClaudeSubscriptionTier().
func TestAuth_ClaudeSubscriptionTier_DoesNotFold(t *testing.T) {
	max20x := &Auth{Metadata: map[string]any{
		"quota_snapshot": map[string]any{
			"profile": map[string]any{
				"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
				"subscription": map[string]any{"has_claude_max": true},
			},
		},
	}}
	max5x := &Auth{Metadata: map[string]any{
		"quota_snapshot": map[string]any{
			"profile": map[string]any{
				"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"},
				"subscription": map[string]any{"has_claude_max": true},
			},
		},
	}}

	// Sanity: the existing coarse folding really does collapse both to "max"
	// (proving this test would catch a regression where someone routed
	// ClaudeSubscriptionTier through the folding function).
	folded20x := registry.NormalizeClaudeSubscriptionPlan(max20x.SubscriptionPlanType())
	folded5x := registry.NormalizeClaudeSubscriptionPlan(max5x.SubscriptionPlanType())
	if folded20x != "max" || folded5x != "max" {
		t.Fatalf("test fixture invalid: expected both fixtures to fold to \"max\", got %q and %q", folded20x, folded5x)
	}

	if got := max20x.ClaudeSubscriptionTier(); got != ClaudeMax20x {
		t.Fatalf("ClaudeSubscriptionTier() for 20x fixture = %v, want %v (unfolded)", got, ClaudeMax20x)
	}
	if got := max5x.ClaudeSubscriptionTier(); got != ClaudeMax5x {
		t.Fatalf("ClaudeSubscriptionTier() for 5x fixture = %v, want %v (unfolded)", got, ClaudeMax5x)
	}
	if max20x.ClaudeSubscriptionTier() == max5x.ClaudeSubscriptionTier() {
		t.Fatalf("20x and 5x fixtures must resolve to distinct tiers, both got %v", max20x.ClaudeSubscriptionTier())
	}
}

// TestAuth_ClaudeSubscriptionTier_RecomputesFromCurrentMetadata pins down
// that this is a pure read of current state with no internal caching, so a
// tier upgrade/downgrade after a token refresh (spec.md "等级刷新防陈旧")
// is reflected immediately once the refreshed quota_snapshot is merged into
// Metadata (the merge-not-overwrite behavior itself is existing metadata-merge
// infra, out of scope for this file — this test only pins that this function
// has no stale cache of its own to fight that merge).
func TestAuth_ClaudeSubscriptionTier_RecomputesFromCurrentMetadata(t *testing.T) {
	a := &Auth{Metadata: map[string]any{
		"quota_snapshot": map[string]any{
			"profile": map[string]any{
				"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"},
			},
		},
	}}
	if got := a.ClaudeSubscriptionTier(); got != ClaudeMax5x {
		t.Fatalf("before upgrade: ClaudeSubscriptionTier() = %v, want %v", got, ClaudeMax5x)
	}

	// Simulate a token-refresh-driven quota_snapshot upgrade merged in place.
	a.Metadata["quota_snapshot"] = map[string]any{
		"profile": map[string]any{
			"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
		},
	}
	if got := a.ClaudeSubscriptionTier(); got != ClaudeMax20x {
		t.Fatalf("after upgrade: ClaudeSubscriptionTier() = %v, want %v (must not be stale)", got, ClaudeMax20x)
	}
}

func TestAuth_CodexSubscriptionTier(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want CodexTier
	}{
		{
			name: "nil auth is unknown",
			auth: nil,
			want: CodexTierUnknown,
		},
		{
			name: "empty auth is unknown",
			auth: &Auth{},
			want: CodexTierUnknown,
		},
		{
			name: "codex pro",
			auth: &Auth{Attributes: map[string]string{"plan_type": "pro"}},
			want: CodexPro,
		},
		{
			name: "codex plus",
			auth: &Auth{Attributes: map[string]string{"plan_type": "plus"}},
			want: CodexPlus,
		},
		{
			name: "case-insensitive and whitespace tolerant",
			auth: &Auth{Attributes: map[string]string{"plan_type": "  Pro  "}},
			want: CodexPro,
		},
		{
			name: "empty plan_type is unknown",
			auth: &Auth{Attributes: map[string]string{"plan_type": ""}},
			want: CodexTierUnknown,
		},
		{
			name: "missing plan_type key is unknown",
			auth: &Auth{Attributes: map[string]string{"other": "value"}},
			want: CodexTierUnknown,
		},
		{
			name: "unmapped team/business plan_type is unknown, not misjudged",
			auth: &Auth{Attributes: map[string]string{"plan_type": "team"}},
			want: CodexTierUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.CodexSubscriptionTier(); got != tt.want {
				t.Fatalf("CodexSubscriptionTier() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeTier_String(t *testing.T) {
	cases := map[ClaudeTier]string{
		ClaudeMax20x:      "max_20x",
		ClaudeMax5x:       "max_5x",
		ClaudePro:         "pro",
		ClaudeTierUnknown: "unknown",
		ClaudeTier(99):    "unknown", // out-of-range never panics
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Fatalf("ClaudeTier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestCodexTier_String(t *testing.T) {
	cases := map[CodexTier]string{
		CodexPro:         "pro",
		CodexPlus:        "plus",
		CodexTierUnknown: "unknown",
		CodexTier(99):    "unknown", // out-of-range never panics
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Fatalf("CodexTier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestClaudeTierBaseWeight(t *testing.T) {
	weights := internalconfig.ClaudeTierWeights{
		Max20x:  20,
		Max5x:   5,
		Pro:     1,
		Unknown: 0.5,
	}
	tests := []struct {
		tier ClaudeTier
		want float64
	}{
		{ClaudeMax20x, 20},
		{ClaudeMax5x, 5},
		{ClaudePro, 1},
		{ClaudeTierUnknown, 0.5},
	}
	for _, tt := range tests {
		if got := ClaudeTierBaseWeight(tt.tier, weights); got != tt.want {
			t.Fatalf("ClaudeTierBaseWeight(%v) = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

func TestCodexTierBaseWeight(t *testing.T) {
	weights := internalconfig.CodexTierWeights{
		Pro:     10,
		Plus:    1,
		Unknown: 0.5,
	}
	tests := []struct {
		tier CodexTier
		want float64
	}{
		{CodexPro, 10},
		{CodexPlus, 1},
		{CodexTierUnknown, 0.5},
	}
	for _, tt := range tests {
		if got := CodexTierBaseWeight(tt.tier, weights); got != tt.want {
			t.Fatalf("CodexTierBaseWeight(%v) = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

// TestClaudeTierBaseWeight_MatchesConfigDefaults pins the design §5.2
// defaults end-to-end: tier identification -> weight lookup, using the real
// DefaultAccountTierWeights() config rather than hand-rolled test weights.
func TestClaudeTierBaseWeight_MatchesConfigDefaults(t *testing.T) {
	defaults := internalconfig.DefaultAccountTierWeights()
	auth20x := &Auth{Metadata: map[string]any{
		"quota_snapshot": map[string]any{
			"profile": map[string]any{
				"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
			},
		},
	}}
	got := ClaudeTierBaseWeight(auth20x.ClaudeSubscriptionTier(), defaults.Claude)
	if got != internalconfig.DefaultAccountTierWeightClaudeMax20x {
		t.Fatalf("ClaudeTierBaseWeight() = %v, want default max-20x weight %v", got, internalconfig.DefaultAccountTierWeightClaudeMax20x)
	}
}

func TestAuth_AccountTierBaseWeight(t *testing.T) {
	weights := internalconfig.AccountTierWeightsConfig{
		Claude: internalconfig.ClaudeTierWeights{Max20x: 20, Max5x: 5, Pro: 1, Unknown: 0.5},
		Codex:  internalconfig.CodexTierWeights{Pro: 10, Plus: 1, Unknown: 0.5},
	}

	tests := []struct {
		name string
		auth *Auth
		want float64
	}{
		{
			name: "nil auth",
			auth: nil,
			want: 0,
		},
		{
			name: "claude provider dispatches to claude weights",
			auth: &Auth{
				Provider: "claude",
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
						},
					},
				},
			},
			want: 20,
		},
		{
			name: "provider casing is tolerated",
			auth: &Auth{
				Provider: "Claude",
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"organization": map[string]any{"rate_limit_tier": "default_claude_pro"},
						},
					},
				},
			},
			want: 1,
		},
		{
			// §8.2: Codex is deliberately dropped from adaptive scheduling by
			// returning a 0 base weight, so it退回普通轮询. CodexTierBaseWeight
			// itself still maps plan_type -> configured weight (see
			// TestCodexTierBaseWeight), but AccountTierBaseWeight no longer
			// dispatches Codex through it.
			name: "codex provider returns 0 base weight (claude-only收敛, §8.2)",
			auth: &Auth{
				Provider:   "codex",
				Attributes: map[string]string{"plan_type": "plus"},
			},
			want: 0,
		},
		{
			name: "codex pro also returns 0 base weight (§8.2)",
			auth: &Auth{
				Provider:   "codex",
				Attributes: map[string]string{"plan_type": "pro"},
			},
			want: 0,
		},
		{
			name: "claude account with unrecognized tier falls back to claude unknown weight",
			auth: &Auth{
				Provider: "claude",
				Metadata: map[string]any{
					"quota_snapshot": map[string]any{
						"profile": map[string]any{
							"organization": map[string]any{"rate_limit_tier": "something_new"},
						},
					},
				},
			},
			want: 0.5,
		},
		{
			name: "unrecognized provider returns 0, not a guess",
			auth: &Auth{Provider: "gemini"},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.AccountTierBaseWeight(weights); got != tt.want {
				t.Fatalf("AccountTierBaseWeight() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuth_ClaudeSubscriptionTier_Override covers the manual tier_override
// (TierOverrideMetadataKey) path: a legal Claude override wins over the
// rate_limit_tier auto-detection, while an absent/blank/illegal/Codex-scoped
// override is ignored and the unchanged auto-detection runs.
func TestAuth_ClaudeSubscriptionTier_Override(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want ClaudeTier
	}{
		{
			// Override wins over a DIFFERENT recognized rate_limit_tier: proves
			// precedence, not just fallthrough.
			name: "override max_20x beats rate_limit_tier default_claude_pro",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: "max_20x",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_pro"},
					},
				},
			}},
			want: ClaudeMax20x,
		},
		{
			// The real production test-account scenario: upstream reports the
			// unrecognized "default_claude_ai" (auto-detect -> Unknown), and the
			// override makes it testable as max_5x.
			name: "override max_5x rescues an unrecognized rate_limit_tier",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: "max_5x",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_ai"},
					},
				},
			}},
			want: ClaudeMax5x,
		},
		{
			name: "override pro with no quota_snapshot at all",
			auth: &Auth{Metadata: map[string]any{TierOverrideMetadataKey: "pro"}},
			want: ClaudePro,
		},
		{
			name: "override is case-insensitive and whitespace tolerant",
			auth: &Auth{Metadata: map[string]any{TierOverrideMetadataKey: "  Max_20X  "}},
			want: ClaudeMax20x,
		},
		{
			// Illegal override value must NOT be guessed; falls back to the
			// recognized rate_limit_tier auto-detection (existing behavior).
			name: "illegal override falls back to auto-detection",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: "super_ultra",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
					},
				},
			}},
			want: ClaudeMax20x,
		},
		{
			name: "empty override falls back to auto-detection",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: "",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"},
					},
				},
			}},
			want: ClaudeMax5x,
		},
		{
			// A Codex-scoped override value is not a legal Claude value, so it is
			// ignored on a Claude read and auto-detection runs.
			name: "codex-scoped override is ignored by the claude reader",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: "codex_pro",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_pro"},
					},
				},
			}},
			want: ClaudePro,
		},
		{
			// Non-string override value (defensive): ignored, auto-detect runs.
			name: "non-string override value is ignored",
			auth: &Auth{Metadata: map[string]any{
				TierOverrideMetadataKey: 42,
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
					},
				},
			}},
			want: ClaudeMax20x,
		},
		{
			// No override key and no recognized tier -> unchanged Unknown.
			name: "no override and unrecognized tier stays unknown",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_ai"},
					},
				},
			}},
			want: ClaudeTierUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.ClaudeSubscriptionTier(); got != tt.want {
				t.Fatalf("ClaudeSubscriptionTier() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuth_CodexSubscriptionTier_Override covers the Codex side of the manual
// override: a legal codex_* override wins over plan_type (and works even with an
// empty Attributes map), while anything else falls back to plan_type unchanged.
func TestAuth_CodexSubscriptionTier_Override(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want CodexTier
	}{
		{
			name: "override codex_pro beats plan_type plus",
			auth: &Auth{
				Attributes: map[string]string{"plan_type": "plus"},
				Metadata:   map[string]any{TierOverrideMetadataKey: "codex_pro"},
			},
			want: CodexPro,
		},
		{
			name: "override codex_plus with empty attributes",
			auth: &Auth{Metadata: map[string]any{TierOverrideMetadataKey: "codex_plus"}},
			want: CodexPlus,
		},
		{
			name: "override case-insensitive",
			auth: &Auth{Metadata: map[string]any{TierOverrideMetadataKey: "  Codex_Pro  "}},
			want: CodexPro,
		},
		{
			name: "illegal override falls back to plan_type",
			auth: &Auth{
				Attributes: map[string]string{"plan_type": "pro"},
				Metadata:   map[string]any{TierOverrideMetadataKey: "nonsense"},
			},
			want: CodexPro,
		},
		{
			// A Claude-scoped override value is not a legal Codex value, so it is
			// ignored on a Codex read and plan_type auto-detection runs.
			name: "claude-scoped override is ignored by the codex reader",
			auth: &Auth{
				Attributes: map[string]string{"plan_type": "plus"},
				Metadata:   map[string]any{TierOverrideMetadataKey: "max_20x"},
			},
			want: CodexPlus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.CodexSubscriptionTier(); got != tt.want {
				t.Fatalf("CodexSubscriptionTier() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuth_AccountTierBaseWeight_Override ties the override end-to-end to the
// weight lookup: a Claude account whose upstream rate_limit_tier is
// unrecognized (weight -> Unknown) is lifted to the Max5x weight by a manual
// override -- the exact mechanism the Phase-1 real-account validation relies on.
func TestAuth_AccountTierBaseWeight_Override(t *testing.T) {
	weights := internalconfig.AccountTierWeightsConfig{
		Claude: internalconfig.ClaudeTierWeights{Max20x: 20, Max5x: 5, Pro: 1, Unknown: 0.5},
		Codex:  internalconfig.CodexTierWeights{Pro: 10, Plus: 1, Unknown: 0.5},
	}
	base := func() *Auth {
		return &Auth{
			Provider: "claude",
			Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"organization": map[string]any{"rate_limit_tier": "default_claude_ai"},
					},
				},
			},
		}
	}

	noOverride := base()
	if got := noOverride.AccountTierBaseWeight(weights); got != 0.5 {
		t.Fatalf("without override: AccountTierBaseWeight() = %v, want 0.5 (unknown)", got)
	}

	withOverride := base()
	withOverride.Metadata[TierOverrideMetadataKey] = "max_5x"
	if got := withOverride.AccountTierBaseWeight(weights); got != 5 {
		t.Fatalf("with override: AccountTierBaseWeight() = %v, want 5 (max_5x)", got)
	}
}
