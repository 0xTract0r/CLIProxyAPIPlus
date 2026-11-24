package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

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

func TestNewRoutingSelectorNonAdaptiveStrategiesUnchanged(t *testing.T) {
	t.Run("round-robin without session affinity", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{strategy: "round-robin"})
		if _, ok := selector.(*coreauth.RoundRobinSelector); !ok {
			t.Fatalf("selector type = %T, want *coreauth.RoundRobinSelector", selector)
		}
	})
	t.Run("fill-first without session affinity", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{strategy: "fill-first"})
		if _, ok := selector.(*coreauth.FillFirstSelector); !ok {
			t.Fatalf("selector type = %T, want *coreauth.FillFirstSelector", selector)
		}
	})
	t.Run("round-robin with session affinity still wraps in SessionAffinitySelector", func(t *testing.T) {
		selector := newRoutingSelector(routingRuntimeState{
			strategy:           "round-robin",
			sessionAffinity:    true,
			sessionAffinityTTL: time.Hour,
		})
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("selector type = %T, want *coreauth.SessionAffinitySelector", selector)
		}
		affinity.Stop()
	})
}

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
