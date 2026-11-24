package config

import (
	"fmt"
	"strings"
)

// ValidateOrdinaryRouting rejects routing policies that would move a session
// to a different credential after an upstream failure.
func (cfg *Config) ValidateOrdinaryRouting() error {
	if cfg.Routing.MaxConcurrentPerCredential < 0 {
		return fmt.Errorf("routing.max-concurrent-per-credential must not be negative")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Routing.Strategy), "adaptive") {
		return fmt.Errorf("unsupported routing strategy %q", cfg.Routing.Strategy)
	}
	if cfg.Routing.ClaudeCodeSessionAffinity || cfg.Routing.SessionAffinity {
		switch strings.ToLower(strings.TrimSpace(cfg.Routing.SessionAffinityRebindOn)) {
		case "", "never":
		default:
			return fmt.Errorf("unsupported session affinity rebinding policy")
		}
	}
	return nil
}
