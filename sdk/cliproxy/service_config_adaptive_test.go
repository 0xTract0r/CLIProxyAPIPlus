package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// This file covers openspec/changes/add-adaptive-account-scheduling tasks.md
// Phase 1 task 1.2 / Phase 5 task 5.1's newRoutingSelector wiring slice: the
// "adaptive" routing.strategy value is assembled correctly, every other
// strategy value keeps its pre-existing behavior byte-for-byte, and the live
// AccountSchedulingConfig accessor actually hot-reloads without a selector
// rebuild. It deliberately does not re-test Phase 1-4's own weighting/
// rate-limiting/warm-up logic -- that is sdk/cliproxy/auth's own test
// surface (account_weight_test.go, adaptive_selector_test.go, etc.).

func TestNormalizedRoutingRuntimeStateRecognizesAdaptiveStrategy(t *testing.T) {
	scheduling := internalconfig.DefaultAccountSchedulingConfig()
	scheduling.TierWeights.Claude.Max20x = 42

	cfg := &config.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: " Adaptive ",
		},
		AccountScheduling: scheduling,
	}

	state := normalizedRoutingRuntimeState(cfg)

	if state.strategy != internalconfig.RoutingStrategyAdaptive {
		t.Fatalf("strategy = %q, want %q", state.strategy, internalconfig.RoutingStrategyAdaptive)
	}
	if got, want := state.accountScheduling.TierWeights.Claude.Max20x, 42.0; got != want {
		t.Fatalf("accountScheduling.TierWeights.Claude.Max20x = %v, want %v (snapshot not carried through)", got, want)
	}
}

func TestNormalizedRoutingRuntimeStateNonAdaptiveStrategiesUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty defaults to round-robin", input: "", want: "round-robin"},
		{name: "round-robin literal", input: "round-robin", want: "round-robin"},
		{name: "fill-first literal", input: "fill-first", want: "fill-first"},
		{name: "fillfirst alias", input: "fillfirst", want: "fill-first"},
		{name: "ff alias", input: "ff", want: "fill-first"},
		{name: "unrecognized value falls back to round-robin", input: "some-unknown-strategy", want: "round-robin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Routing: internalconfig.RoutingConfig{Strategy: tc.input}}
			state := normalizedRoutingRuntimeState(cfg)
			if state.strategy != tc.want {
				t.Fatalf("strategy = %q, want %q", state.strategy, tc.want)
			}
		})
	}
}

func TestNewRoutingSelectorAdaptiveStrategySelfHostsSessionAffinity(t *testing.T) {
	state := routingRuntimeState{
		strategy:           internalconfig.RoutingStrategyAdaptive,
		sessionAffinity:    true,
		sessionAffinityTTL: time.Hour,
		accountScheduling:  internalconfig.DefaultAccountSchedulingConfig(),
	}

	selector := newRoutingSelector(state)

	// The contract (design.md D5) is that AdaptiveSelector carries its own
	// SessionAffinity-aware maturity grading internally, so the routing
	// wiring must return the *coreauth.AdaptiveSelector directly rather than
	// wrapping it in *coreauth.SessionAffinitySelector -- the wrapper would
	// short-circuit on a cache hit and the adaptive selector would never see
	// (and so never grade) the binding.
	adaptive, ok := selector.(*coreauth.AdaptiveSelector)
	if !ok {
		t.Fatalf("selector type = %T, want *coreauth.AdaptiveSelector (must not be wrapped in SessionAffinitySelector)", selector)
	}
	adaptive.Stop()
}

func TestNewRoutingSelectorNonAdaptiveStrategiesUnchanged(t *testing.T) {
	t.Run("round-robin without session affinity", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{strategy: "round-robin", accountScheduling: internalconfig.DefaultAccountSchedulingConfig()})
		if _, ok := selector.(*coreauth.RoundRobinSelector); !ok {
			t.Fatalf("selector type = %T, want *coreauth.RoundRobinSelector", selector)
		}
	})
	t.Run("fill-first without session affinity", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{strategy: "fill-first", accountScheduling: internalconfig.DefaultAccountSchedulingConfig()})
		if _, ok := selector.(*coreauth.FillFirstSelector); !ok {
			t.Fatalf("selector type = %T, want *coreauth.FillFirstSelector", selector)
		}
	})
	t.Run("round-robin with session affinity still wraps in SessionAffinitySelector", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{
			strategy:           "round-robin",
			sessionAffinity:    true,
			sessionAffinityTTL: time.Hour,
			accountScheduling:  internalconfig.DefaultAccountSchedulingConfig(),
		})
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("selector type = %T, want *coreauth.SessionAffinitySelector", selector)
		}
		affinity.Stop()
	})
}

// TestApplyManagerConfigAdaptiveStrategyWiresAdaptiveSelectorIntoManager is the
// end-to-end analogue of TestServiceAppliesSameValueNewestSelectorCommit
// (service_executionregistry_test.go), covering the "adaptive" strategy: the
// live Manager's selector actually becomes *coreauth.AdaptiveSelector, and a
// provider this scheduler has no tier weight for (anything other than
// claude/codex -- design.md D7 backward compatibility) still resolves via the
// AdaptiveSelector's wrapped fallback, exactly as it would have under plain
// round-robin.
func TestApplyManagerConfigAdaptiveStrategyWiresAdaptiveSelectorIntoManager(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(serviceTestPluginExecutor{})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "auth-a", Provider: "plugin-provider", Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	commit := service.commitConfigUpdate(&config.Config{Routing: internalconfig.RoutingConfig{Strategy: internalconfig.RoutingStrategyAdaptive}})
	if !service.applyConfigRuntime(context.Background(), commit, false) {
		t.Fatal("adaptive-strategy config runtime apply failed")
	}

	selector := manager.Selector()
	adaptive, ok := selector.(*coreauth.AdaptiveSelector)
	if !ok {
		t.Fatalf("manager selector type = %T, want *coreauth.AdaptiveSelector", selector)
	}
	defer adaptive.Stop()

	// plugin-provider has no configured tier weight (design.md D7: only
	// claude/codex are scored), so AccountSelectionWeight is 0 for it and the
	// AdaptiveSelector must serve it via its wrapped fallback selector rather
	// than denying the request.
	selected, errSelect := manager.SelectAuth(context.Background(), "plugin-provider", "", cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v", errSelect)
	}
	if selected == nil || selected.ID != "auth-a" {
		t.Fatalf("selector picked = %+v, want auth-a via adaptive selector's fallback", selected)
	}
}

// TestApplyManagerConfigAdaptiveStrategyHotReloadsSchedulingWithoutRebuild
// covers the newRoutingSelectorWithLiveScheduling / liveAccountSchedulingConfig
// design: a config reload that changes ONLY the account-scheduling section
// (routing.strategy / session-affinity unchanged) must NOT rebuild the
// selector (that would drop in-flight session stickiness bindings and reset
// per-account token buckets for no reason -- see routingRuntimeState.
// selectorShapeEqual), yet the live scheduling snapshot the already-built
// AdaptiveSelector reads on its next Pick must reflect the new value.
func TestApplyManagerConfigAdaptiveStrategyHotReloadsSchedulingWithoutRebuild(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}

	initialScheduling := internalconfig.DefaultAccountSchedulingConfig()
	initialScheduling.TierWeights.Claude.Max20x = 20
	initial := service.commitConfigUpdate(&config.Config{
		Routing:           internalconfig.RoutingConfig{Strategy: internalconfig.RoutingStrategyAdaptive},
		AccountScheduling: initialScheduling,
	})
	if !service.applyConfigRuntime(context.Background(), initial, false) {
		t.Fatal("initial adaptive config runtime apply failed")
	}
	initialSelector := manager.Selector()
	initialAdaptive, ok := initialSelector.(*coreauth.AdaptiveSelector)
	if !ok {
		t.Fatalf("initial selector type = %T, want *coreauth.AdaptiveSelector", initialSelector)
	}
	defer initialAdaptive.Stop()

	updatedScheduling := internalconfig.DefaultAccountSchedulingConfig()
	updatedScheduling.TierWeights.Claude.Max20x = 999
	updated := service.commitConfigUpdate(&config.Config{
		Routing:           internalconfig.RoutingConfig{Strategy: internalconfig.RoutingStrategyAdaptive},
		AccountScheduling: updatedScheduling,
	})
	if !service.applyConfigRuntime(context.Background(), updated, false) {
		t.Fatal("updated-scheduling config runtime apply failed")
	}

	if got := manager.Selector(); got != initialSelector {
		t.Fatalf("selector identity changed on scheduling-only reload = %p, want unchanged %p (unnecessary rebuild)", got, initialSelector)
	}
	if got, want := service.liveAccountSchedulingConfig().TierWeights.Claude.Max20x, 999.0; got != want {
		t.Fatalf("liveAccountSchedulingConfig().TierWeights.Claude.Max20x = %v, want %v (hot reload not observed)", got, want)
	}
}

// TestApplyManagerConfigRoundRobinStrategyStillWorksEndToEnd is the explicit
// "round-robin 时行为不变" regression the assignment asked for -- a plain
// (non-fill-first) config still resolves and serves requests exactly as
// before this change.
func TestApplyManagerConfigRoundRobinStrategyStillWorksEndToEnd(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(serviceTestPluginExecutor{})
	for _, id := range []string{"auth-a", "auth-b"} {
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "plugin-provider", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	commit := service.commitConfigUpdate(&config.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin"}})
	if !service.applyConfigRuntime(context.Background(), commit, false) {
		t.Fatal("round-robin config runtime apply failed")
	}

	if _, ok := manager.Selector().(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("manager selector type = %T, want *coreauth.RoundRobinSelector", manager.Selector())
	}

	seen := make(map[string]bool, 2)
	for range 2 {
		selected, errSelect := manager.SelectAuth(context.Background(), "plugin-provider", "", cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("SelectAuth() error = %v", errSelect)
		}
		if selected == nil {
			t.Fatal("SelectAuth() returned nil auth")
		}
		seen[selected.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("round-robin selections = %v, want both auth-a and auth-b visited", seen)
	}
}
