package config

import "fmt"

// WarmupTrafficPacingConfig controls admission and the continuously refilled
// request balance. Effective per-account limits still come from warmup stages.
type WarmupTrafficPacingConfig struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	RequestBurst             int  `yaml:"request-burst" json:"request-burst"`
	MinAdmissionRequests     int  `yaml:"min-admission-requests" json:"min-admission-requests"`
	MaxActiveBindings        int  `yaml:"max-active-bindings" json:"max-active-bindings"`
	ActiveBindingIdleSeconds int  `yaml:"active-binding-idle-seconds" json:"active-binding-idle-seconds"`
}

func DefaultWarmupTrafficPacingConfig() WarmupTrafficPacingConfig {
	return WarmupTrafficPacingConfig{RequestBurst: 8, MinAdmissionRequests: 4, MaxActiveBindings: 1, ActiveBindingIdleSeconds: 300}
}

func (c WarmupTrafficPacingConfig) Validate() error {
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
