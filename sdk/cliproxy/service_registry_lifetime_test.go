package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type ordinaryRegistryObserver struct {
	coreauth.NoopHook
	registered int
}

func (h *ordinaryRegistryObserver) OnAuthRegistered(context.Context, *coreauth.Auth) {
	h.registered++
}

func TestBuilderKeepsRegistryObserversSeparate(t *testing.T) {
	ctx := coreauth.WithSkipPersist(context.Background())
	primary := &ordinaryRegistryObserver{}
	m := coreauth.NewManager(nil, nil, primary)
	s, err := NewBuilder().WithConfig(&config.Config{AuthDir: t.TempDir()}).WithConfigPath(filepath.Join(t.TempDir(), "test.yaml")).WithCoreAuthManager(m).Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(ctx) })
	register := func(id string) {
		t.Helper()
		if _, err := m.Register(ctx, &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	register("ordinary-observer-active")
	if primary.registered != 1 || len(registry.GetGlobalRegistry().GetModelsForClient("ordinary-observer-active")) == 0 {
		t.Fatal("both injected and service-owned observers should receive registration")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	register("ordinary-observer-closed")
	if primary.registered != 2 || len(registry.GetGlobalRegistry().GetModelsForClient("ordinary-observer-closed")) != 0 {
		t.Fatal("shutdown must detach only the closing service's observer")
	}
}

func TestBuilderRejectsUnsupportedAdaptiveAndKeepsDrain(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.Strategy = "adaptive"
	_, err := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).Build()
	if err == nil || !strings.Contains(err.Error(), "unsupported routing strategy") {
		t.Fatalf("invalid configuration accepted: %v", err)
	}
	if serviceShutdownGrace != 90*time.Second {
		t.Fatal("incorrect shutdown grace")
	}
}
