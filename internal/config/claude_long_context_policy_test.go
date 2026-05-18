package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_ClaudeSonnetLongContextPolicyDefaultsToFailWithHint(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if got := cfg.Claude.SonnetLongContextPolicy; got != ClaudeSonnetLongContextPolicyFailWithHint {
		t.Fatalf("SonnetLongContextPolicy = %q, want %q", got, ClaudeSonnetLongContextPolicyFailWithHint)
	}
}

func TestLoadConfigOptional_ClaudeSonnetLongContextPolicyAcceptsRouteToOpus1M(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("claude:\n  sonnet_long_context_policy: route_to_opus_1m\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if got := cfg.Claude.SonnetLongContextPolicy; got != ClaudeSonnetLongContextPolicyRouteToOpus1M {
		t.Fatalf("SonnetLongContextPolicy = %q, want %q", got, ClaudeSonnetLongContextPolicyRouteToOpus1M)
	}
}

func TestLoadConfigOptional_ClaudeSonnetLongContextPolicyAcceptsCompactRequired(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("claude:\n  sonnet_long_context_policy: compact_required\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if got := cfg.Claude.SonnetLongContextPolicy; got != ClaudeSonnetLongContextPolicyCompact {
		t.Fatalf("SonnetLongContextPolicy = %q, want %q", got, ClaudeSonnetLongContextPolicyCompact)
	}
}
