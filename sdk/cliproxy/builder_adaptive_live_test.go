package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// This file covers openspec/changes/add-adaptive-account-scheduling G2: a
// Service that boots straight into routing.strategy=adaptive must read
// AccountSchedulingConfig LIVE, so an account-scheduling edit (warmup-curve /
// tier-weights / mature-limits) takes effect without a restart -- even though
// such an edit does not change the selector "shape" and so never triggers a
// rebuild (routingRuntimeState.selectorShapeEqual deliberately excludes
// accountScheduling). Before the fix, builder.go installed a static-snapshot
// selector (newRoutingSelector, live == nil) whose config could only ever be
// the boot snapshot, so the edit was silently ignored until the next restart.

func claudeTierAuthForLiveTest(id, rateLimitTier string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			// ClaudeSubscriptionTier reads
			// Metadata.quota_snapshot.profile.organization.rate_limit_tier
			// (account_tier.go). No usage windows are set, so quota headroom is
			// "unknown" (a positive weight factor) and there is no
			// first_production_at anchor, so freshness is 1 (mature) -- the
			// selection weight reduces cleanly to the tier base weight.
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{
						"rate_limit_tier": rateLimitTier,
					},
				},
			},
		},
	}
}

// claudeOnlyTierWeights returns a default scheduling config whose only non-zero
// Claude tier weights are the two supplied. A zero weight excludes a candidate
// outright (AccountSelectionWeight base <= 0 -> dropped in scoreCandidates), so
// the resulting Pick is deterministic with no randomness to fight.
func claudeOnlyTierWeights(max20x, pro float64) internalconfig.AccountSchedulingConfig {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.TierWeights.Claude.Max20x = max20x
	cfg.TierWeights.Claude.Max5x = 0
	cfg.TierWeights.Claude.Pro = pro
	cfg.TierWeights.Claude.Unknown = 0
	return cfg
}

func TestBuilderAdaptiveSelectorReadsLiveSchedulingConfig(t *testing.T) {
	max20xAuth := claudeTierAuthForLiveTest("claude-max20x", "default_claude_max_20x")
	proAuth := claudeTierAuthForLiveTest("claude-pro", "default_claude_pro")
	candidates := []*coreauth.Auth{max20xAuth, proAuth}

	authDir := t.TempDir()
	bootCfg := &config.Config{
		AuthDir:           authDir,
		Routing:           internalconfig.RoutingConfig{Strategy: internalconfig.RoutingStrategyAdaptive},
		AccountScheduling: claudeOnlyTierWeights(1, 0), // boot: only Max20x eligible
	}
	service, errBuild := NewBuilder().
		WithConfig(bootCfg).
		WithConfigPath(t.TempDir() + "/config.yaml").
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	selector, ok := service.coreManager.Selector().(*coreauth.AdaptiveSelector)
	if !ok {
		t.Fatalf("built selector = %T, want *coreauth.AdaptiveSelector (adaptive strategy at boot)", service.coreManager.Selector())
	}
	defer selector.Stop()

	// Boot config weights only Max20x -> only the Max20x account is eligible.
	picked, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, candidates)
	if errPick != nil {
		t.Fatalf("initial Pick error = %v", errPick)
	}
	if picked == nil || picked.ID != "claude-max20x" {
		t.Fatalf("initial pick = %v, want claude-max20x under Max20x-only weights", authID(picked))
	}

	// Hot reload ONLY the account-scheduling section (strategy/affinity
	// unchanged). commitConfigUpdate swaps s.cfg in place; the selector must NOT
	// be rebuilt (selectorShapeEqual is true), so the very same live selector
	// instance has to observe the new weights on its next Pick.
	before := service.coreManager.Selector()
	service.commitConfigUpdate(&config.Config{
		AuthDir:           authDir,
		Routing:           internalconfig.RoutingConfig{Strategy: internalconfig.RoutingStrategyAdaptive},
		AccountScheduling: claudeOnlyTierWeights(0, 1), // reload: now only Pro eligible
	})
	if after := service.coreManager.Selector(); after != before {
		t.Fatalf("selector instance changed on a scheduling-only edit (%p -> %p); this test must exercise the same live selector", before, after)
	}

	// If the builder had wired a static snapshot (the pre-fix bug), this Pick
	// would still return claude-max20x. Live-read means it returns claude-pro.
	picked2, errPick2 := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, candidates)
	if errPick2 != nil {
		t.Fatalf("post-reload Pick error = %v", errPick2)
	}
	if picked2 == nil || picked2.ID != "claude-pro" {
		t.Fatalf("post-reload pick = %v, want claude-pro (live account-scheduling edit was ignored -> static-snapshot bug)", authID(picked2))
	}
}

func authID(a *coreauth.Auth) string {
	if a == nil {
		return "<nil>"
	}
	return a.ID
}
