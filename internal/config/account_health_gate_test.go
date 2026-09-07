package config

import (
	"strings"
	"testing"
)

// TestDefaultAccountHealthGateConfig asserts the ANCHOR-Q4 health-gate defaults
// (design §10.6): enabled by default with the conservative fail-safe thresholds.
func TestDefaultAccountHealthGateConfig(t *testing.T) {
	h := DefaultAccountHealthGateConfig()
	if !h.Enabled {
		t.Fatal("health gate must be enabled by default (design §10.6)")
	}
	if h.FailureClusterThreshold != DefaultAccountHealthGateFailureClusterThreshold {
		t.Fatalf("FailureClusterThreshold = %d, want %d", h.FailureClusterThreshold, DefaultAccountHealthGateFailureClusterThreshold)
	}
	if h.BackoffLevelThreshold != DefaultAccountHealthGateBackoffLevelThreshold {
		t.Fatalf("BackoffLevelThreshold = %d, want %d", h.BackoffLevelThreshold, DefaultAccountHealthGateBackoffLevelThreshold)
	}
	if h.ObservationWindowMinutes != DefaultAccountHealthGateObservationWindowMinutes {
		t.Fatalf("ObservationWindowMinutes = %d, want %d", h.ObservationWindowMinutes, DefaultAccountHealthGateObservationWindowMinutes)
	}
	if h.DemoteStep != DefaultAccountHealthGateDemoteStep {
		t.Fatalf("DemoteStep = %d, want %d", h.DemoteStep, DefaultAccountHealthGateDemoteStep)
	}
	if h.PromoteCooldownMinutes != DefaultAccountHealthGatePromoteCooldownMinutes {
		t.Fatalf("PromoteCooldownMinutes = %d, want %d", h.PromoteCooldownMinutes, DefaultAccountHealthGatePromoteCooldownMinutes)
	}
	// Defaults must validate cleanly as part of the whole scheduling config.
	if err := DefaultAccountSchedulingConfig().Validate(); err != nil {
		t.Fatalf("default scheduling config (incl. health gate) must validate: %v", err)
	}
}

func TestAccountHealthGateConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(h *AccountHealthGateConfig)
		wantErr string
	}{
		{name: "defaults valid", mutate: func(h *AccountHealthGateConfig) {}},
		{
			name:   "disabled skips validation entirely",
			mutate: func(h *AccountHealthGateConfig) { *h = AccountHealthGateConfig{Enabled: false} },
		},
		{
			name: "enabled with no signals is rejected",
			mutate: func(h *AccountHealthGateConfig) {
				h.FailureClusterThreshold = 0
				h.BackoffLevelThreshold = 0
			},
			wantErr: "at least one of",
		},
		{
			name: "negative failure-cluster-threshold",
			mutate: func(h *AccountHealthGateConfig) {
				h.FailureClusterThreshold = -1
			},
			wantErr: "failure-cluster-threshold must not be negative",
		},
		{
			name: "failure cluster set but zero window",
			mutate: func(h *AccountHealthGateConfig) {
				h.ObservationWindowMinutes = 0
			},
			wantErr: "observation-window-minutes must be positive",
		},
		{
			name: "backoff-only signal allows zero window",
			mutate: func(h *AccountHealthGateConfig) {
				h.FailureClusterThreshold = 0
				h.ObservationWindowMinutes = 0
				h.BackoffLevelThreshold = 1
			},
		},
		{
			name: "demote-step below one",
			mutate: func(h *AccountHealthGateConfig) {
				h.DemoteStep = 0
			},
			wantErr: "demote-step must be at least 1",
		},
		{
			name: "non-positive promote cooldown",
			mutate: func(h *AccountHealthGateConfig) {
				h.PromoteCooldownMinutes = 0
			},
			wantErr: "promote-cooldown-minutes must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultAccountSchedulingConfig()
			tt.mutate(&cfg.HealthGate)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
