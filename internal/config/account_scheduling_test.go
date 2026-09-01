package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigOptionalAccountSchedulingDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	want := DefaultAccountSchedulingConfig()
	if !reflect.DeepEqual(cfg.AccountScheduling, want) {
		t.Fatalf("AccountScheduling = %+v, want defaults %+v", cfg.AccountScheduling, want)
	}
	if errValidate := cfg.AccountScheduling.Validate(); errValidate != nil {
		t.Fatalf("defaults must validate cleanly: %v", errValidate)
	}
}

func TestLoadConfigOptionalAccountSchedulingPartialOverrideMergesWithDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	// Only override one nested scalar; every other field (including the rest
	// of tier-weights, mature-limits, and the whole warmup-curve) must keep
	// its default value via yaml.v3's field-level merge into the pre-filled
	// struct (see the pre-fill-before-unmarshal comment in config_load.go).
	data := []byte(`account-scheduling:
  tier-weights:
    claude:
      max-20x: 25
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := cfg.AccountScheduling.TierWeights.Claude.Max20x; got != 25 {
		t.Fatalf("tier-weights.claude.max-20x = %v, want 25 (override)", got)
	}
	if got := cfg.AccountScheduling.TierWeights.Claude.Max5x; got != DefaultAccountTierWeightClaudeMax5x {
		t.Fatalf("tier-weights.claude.max-5x = %v, want default %v (untouched sibling)", got, DefaultAccountTierWeightClaudeMax5x)
	}
	if got := cfg.AccountScheduling.TierWeights.Codex.Pro; got != DefaultAccountTierWeightCodexPro {
		t.Fatalf("tier-weights.codex.pro = %v, want default %v (untouched provider)", got, DefaultAccountTierWeightCodexPro)
	}
	if got := cfg.AccountScheduling.MatureLimits.RPMLimit; got != DefaultAccountMatureRPMLimit {
		t.Fatalf("mature-limits.rpm-limit = %v, want default %v (untouched section)", got, DefaultAccountMatureRPMLimit)
	}
	wantCurve := DefaultAccountWarmupCurve()
	if !reflect.DeepEqual(cfg.AccountScheduling.WarmupCurve, wantCurve) {
		t.Fatalf("warmup-curve = %+v, want default curve %+v (untouched)", cfg.AccountScheduling.WarmupCurve, wantCurve)
	}
}

func TestLoadConfigOptionalAccountSchedulingWarmupCurveOverrideReplacesWholesale(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`account-scheduling:
  warmup-curve:
    - name: "only-stage"
      min-age-days: 0
      max-age-days: 0
      daily-budget: 100
      rpm-limit: 2
      concurrency-limit: 1
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if len(cfg.AccountScheduling.WarmupCurve) != 1 {
		t.Fatalf("warmup-curve length = %d, want 1 (custom curve replaces defaults wholesale)", len(cfg.AccountScheduling.WarmupCurve))
	}
	if got := cfg.AccountScheduling.WarmupCurve[0].Name; got != "only-stage" {
		t.Fatalf("warmup-curve[0].name = %q, want %q", got, "only-stage")
	}
}

func TestRoutingStrategyAdaptiveValueRoundTrips(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("routing:\n  strategy: \"adaptive\"\n")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.Routing.Strategy != RoutingStrategyAdaptive {
		t.Fatalf("routing.strategy = %q, want %q", cfg.Routing.Strategy, RoutingStrategyAdaptive)
	}
}

func TestAccountSchedulingConfigValidate(t *testing.T) {
	baseCurve := DefaultAccountWarmupCurve()

	tests := []struct {
		name    string
		mutate  func(c *AccountSchedulingConfig)
		wantErr string
	}{
		{
			name:   "defaults are valid",
			mutate: func(c *AccountSchedulingConfig) {},
		},
		{
			name: "empty curve is valid (defaults used elsewhere)",
			mutate: func(c *AccountSchedulingConfig) {
				c.WarmupCurve = nil
			},
		},
		{
			name: "stage with empty name",
			mutate: func(c *AccountSchedulingConfig) {
				stages := append([]AccountWarmupStage(nil), baseCurve...)
				stages[0].Name = ""
				c.WarmupCurve = stages
			},
			wantErr: "name must not be empty",
		},
		{
			name: "first stage must start at age 0",
			mutate: func(c *AccountSchedulingConfig) {
				stages := append([]AccountWarmupStage(nil), baseCurve...)
				stages[0].MinAgeDays = 1
				c.WarmupCurve = stages
			},
			wantErr: "min-age-days must be 0",
		},
		{
			name: "gap between stages",
			mutate: func(c *AccountSchedulingConfig) {
				stages := append([]AccountWarmupStage(nil), baseCurve...)
				stages[1].MinAgeDays = 10 // baseCurve[0].MaxAgeDays == 7, so this is a gap
				c.WarmupCurve = stages
			},
			wantErr: "must be contiguous",
		},
		{
			name: "unbounded stage not last",
			mutate: func(c *AccountSchedulingConfig) {
				stages := append([]AccountWarmupStage(nil), baseCurve...)
				stages[0].MaxAgeDays = 0
				c.WarmupCurve = stages
			},
			wantErr: "not the last stage",
		},
		{
			name: "max-age-days not greater than min-age-days",
			mutate: func(c *AccountSchedulingConfig) {
				// Single-stage curve where max == min (still "last stage", so
				// the unbounded-only-last rule does not fire first).
				c.WarmupCurve = []AccountWarmupStage{
					{Name: "a", MinAgeDays: 5, MaxAgeDays: 5, DailyBudget: 10, RPMLimit: 1, ConcurrencyLimit: 1},
				}
			},
			wantErr: "greater than min-age-days",
		},
		{
			name: "negative rpm limit",
			mutate: func(c *AccountSchedulingConfig) {
				c.MatureLimits.RPMLimit = 0
			},
			wantErr: "mature-limits.rpm-limit must be positive",
		},
		{
			name: "negative burst",
			mutate: func(c *AccountSchedulingConfig) {
				c.MatureLimits.Burst = -1
			},
			wantErr: "mature-limits.burst must not be negative",
		},
		{
			name: "zero concurrency limit",
			mutate: func(c *AccountSchedulingConfig) {
				c.MatureLimits.ConcurrencyLimit = 0
			},
			wantErr: "mature-limits.concurrency-limit must be positive",
		},
		{
			name: "non-positive tier weight",
			mutate: func(c *AccountSchedulingConfig) {
				c.TierWeights.Claude.Max20x = 0
			},
			wantErr: "tier-weights.claude.max-20x must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultAccountSchedulingConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
