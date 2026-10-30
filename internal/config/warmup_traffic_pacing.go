package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
)

// WarmupTrafficPacingConfig controls admission and the continuously refilled
// request balance. Effective per-account limits still come from warmup stages.
type WarmupTrafficPacingConfig struct {
	Enabled                  bool                       `yaml:"enabled" json:"enabled"`
	RequestBurst             int                        `yaml:"request-burst" json:"request-burst"`
	MinAdmissionRequests     int                        `yaml:"min-admission-requests" json:"min-admission-requests"`
	MaxActiveBindings        int                        `yaml:"max-active-bindings" json:"max-active-bindings"`
	ActiveBindingIdleSeconds int                        `yaml:"active-binding-idle-seconds" json:"active-binding-idle-seconds"`
	AdmissionWait            *WarmupAdmissionWaitConfig `yaml:"admission-wait,omitempty" json:"admission-wait,omitempty"`
}

type WarmupAdmissionWaitConfig struct {
	MaxWaitSeconds       int `yaml:"max-wait-seconds" json:"max-wait-seconds"`
	MaxPendingPerAccount int `yaml:"max-pending-per-account" json:"max-pending-per-account"`
	MaxPendingTotal      int `yaml:"max-pending-total" json:"max-pending-total"`
}

func (c *WarmupAdmissionWaitConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain WarmupAdmissionWaitConfig
	value := plain{MaxWaitSeconds: 30, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = WarmupAdmissionWaitConfig(value)
	return nil
}

func (c WarmupTrafficPacingConfig) WaitConfig() WarmupAdmissionWaitConfig {
	if c.AdmissionWait != nil {
		return *c.AdmissionWait
	}
	return WarmupAdmissionWaitConfig{MaxWaitSeconds: 30, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
}

func DefaultWarmupTrafficPacingConfig() WarmupTrafficPacingConfig {
	return WarmupTrafficPacingConfig{RequestBurst: 8, MinAdmissionRequests: 4, MaxActiveBindings: 1, ActiveBindingIdleSeconds: 300}
}

func (c WarmupTrafficPacingConfig) Validate() error {
	w := c.WaitConfig()
	if w.MaxWaitSeconds < 0 || w.MaxWaitSeconds > 60 || w.MaxPendingPerAccount < 1 || w.MaxPendingPerAccount > 16 || w.MaxPendingTotal < 1 || w.MaxPendingTotal > 128 || w.MaxPendingPerAccount > w.MaxPendingTotal {
		return fmt.Errorf("account-scheduling.warmup-traffic-pacing.admission-wait requires 0-60 seconds, 1-16 per account and 1-128 total (per-account <= total)")
	}
	if !c.Enabled {
		return nil
	}
	if c.RequestBurst <= 0 || c.MinAdmissionRequests <= 0 || c.MaxActiveBindings <= 0 || c.ActiveBindingIdleSeconds <= 0 {
		return fmt.Errorf("account-scheduling.warmup-traffic-pacing requires positive integer limits when enabled")
	}
	if c.MinAdmissionRequests > c.RequestBurst {
		return fmt.Errorf("account-scheduling.warmup-traffic-pacing.min-admission-requests must not exceed request-burst")
	}
	return nil
}
