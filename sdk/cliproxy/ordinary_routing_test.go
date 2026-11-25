package cliproxy

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestOrdinaryRoutingRejectsUnsafeReload(t *testing.T) {
	base := &config.Config{AuthDir: t.TempDir()}
	base.Routing.ClaudeCodeSessionAffinity = true
	base.Routing.SessionAffinityTTL = "19m"
	s := &Service{cfg: base}
	r := normalizedRoutingRuntimeState(base)
	if !r.sessionAffinity || r.sessionAffinityTTL != 19*time.Minute || r.sessionAffinityRebindOn != coreauth.SessionAffinityRebindOnNever {
		t.Fatalf("legacy affinity lost or enabled failover: %+v", r)
	}
	for _, policy := range []string{"quota", "any", "unexpected", "adaptive"} {
		t.Run(policy, func(t *testing.T) {
			next := base.CloneForRuntime()
			next.AuthDir = t.TempDir()
			if policy == "adaptive" {
				next.Routing.Strategy = policy
			} else {
				next.Routing.SessionAffinityRebindOn = policy
			}
			if c := s.commitConfigUpdate(next); c.cfg != nil || s.currentConfig() != base {
				t.Fatal("unsafe update was accepted or altered the previously accepted config")
			}
			if _, err := NewBuilder().WithConfig(next).WithConfigPath("fixture.yaml").Build(); err == nil {
				t.Fatal("startup accepted unsupported routing")
			}
		})
	}
	next := base.CloneForRuntime()
	next.Routing.SessionAffinityRebindOn = "never"
	if c := s.commitConfigUpdate(next); c.cfg == nil {
		t.Fatal("ordinary sticky configuration rejected")
	}
}
