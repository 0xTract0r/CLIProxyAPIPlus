package config

import (
	"gopkg.in/yaml.v3"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestWarmupAdmissionWaitPartialConfig(t *testing.T) {
	var got WarmupTrafficPacingConfig
	if err := yaml.Unmarshal([]byte("admission-wait:\n  max-wait-seconds: 0\n"), &got); err != nil {
		t.Fatal(err)
	}
	if w := got.WaitConfig(); w.MaxWaitSeconds != 0 || w.MaxPendingPerAccount != 4 || w.MaxPendingTotal != 32 {
		t.Fatal("partial wait config lost defaults", w)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	got.AdmissionWait.MaxPendingPerAccount = 0
	if got.Validate() == nil {
		t.Fatal("explicit zero queue capacity accepted")
	}
}

func TestAccountSchedulingRateScaleFiniteBounds(t *testing.T) {
	for _, value := range []float64{math.Inf(1), math.NaN(), 1e18} {
		cfg := DefaultAccountSchedulingConfig()
		cfg.RateScale = value
		if cfg.Validate() == nil {
			t.Fatal("invalid scale accepted", value)
		}
	}
}

func TestWarmupTrafficPacingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("account-scheduling:\n  warmup-traffic-pacing:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultWarmupTrafficPacingConfig()
	want.Enabled = true
	if cfg.AccountScheduling.WarmupTrafficPacing != want {
		t.Fatalf("partial config lost defaults: %+v", cfg.AccountScheduling.WarmupTrafficPacing)
	}
	if DefaultAccountSchedulingConfig().WarmupTrafficPacing.Enabled {
		t.Fatal("pacing enabled by default")
	}
	for _, name := range []string{"burst", "admission", "groups", "idle", "admission-exceeds-burst"} {
		t.Run(name, func(t *testing.T) {
			c := want
			switch name {
			case "burst":
				c.RequestBurst = 0
			case "admission":
				c.MinAdmissionRequests = -1
			case "groups":
				c.MaxActiveBindings = 0
			case "idle":
				c.ActiveBindingIdleSeconds = -1
			case "admission-exceeds-burst":
				c.MinAdmissionRequests = 9
			}
			if c.Validate() == nil {
				t.Fatal("invalid enabled pacing accepted")
			}
			c.Enabled = false
			if err := c.Validate(); err != nil {
				t.Fatal("disabled pacing changed legacy validation", err)
			}
		})
	}
}
