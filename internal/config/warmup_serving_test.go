package config

import (
	"math"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWarmupServingConfig(t *testing.T) {
	cfg := DefaultAccountSchedulingConfig()
	if cfg.WarmupServingReserve != 0 || cfg.WarmupServingMaxBindingAgeSeconds != 0 || cfg.WarmupServingMigrationTokenBudget != 0 {
		t.Fatal("serving reserve must default off")
	}
	for _, value := range []float64{-0.1, 1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		bad := cfg
		bad.WarmupServingReserve = value
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid reserve %v", value)
		}
	}
	for _, field := range []string{"warmup-serving-max-binding-age-seconds", "warmup-serving-migration-token-budget"} {
		bad := cfg
		if err := yaml.Unmarshal([]byte(field+": -1"), &bad); err != nil {
			t.Fatal(err)
		}
		if bad.Validate() == nil {
			t.Fatalf("accepted negative %s", field)
		}
	}
	input := []byte("warmup-serving-reserve: 0.15\nwarmup-serving-max-binding-age-seconds: 3600\nwarmup-serving-migration-token-budget: 12000\n")
	if err := yaml.Unmarshal(input, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.WarmupServingReserve != 0.15 || cfg.WarmupServingMaxBindingAgeSeconds != 3600 || cfg.WarmupServingMigrationTokenBudget != 12000 {
		t.Fatalf("fields not loaded: %+v", cfg)
	}
}
