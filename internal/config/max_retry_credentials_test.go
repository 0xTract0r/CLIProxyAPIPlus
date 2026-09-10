package config

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests guard the config-load leg of the harden-account-scheduling-limiter
// ERR-2 escape hatch (fork upstream-sync guard).
//
// Contract: config load/parse must preserve `max-retry-credentials` verbatim.
// A negative value (e.g. -1) is the explicit "unbounded try-all" escape hatch and
// MUST survive un-clamped so it can reach auth.Manager.SetRetryConfig, which is the
// ONLY layer that maps 0/negative to their runtime meaning (0 -> bounded default
// cap 2, negative -> unbounded). An explicit 0 (indistinguishable from an omitted
// key in yaml) must stay 0 at this layer; the default-to-2 mapping lives in
// SetRetryConfig, not here.
//
// Intent of these assertions: if a future upstream sync re-introduces the old
// clamp -- `if cfg.MaxRetryCredentials < 0 { cfg.MaxRetryCredentials = 0 }` -- in
// LoadConfigOptional (config_load.go) or ParseConfigBytes (parse.go), the -1 cases
// below turn to 0 and these tests MUST go red, flagging that the escape hatch was
// silently severed before it could reach SetRetryConfig.

func TestLoadConfigOptional_MaxRetryCredentialsNegativeSurvivesUnclamped(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("max-retry-credentials: -1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	// The escape hatch: -1 must NOT be clamped to 0 here. If this fails, the load
	// leg re-introduced the clamp and the "-1 = unbounded try-all" hatch never
	// reaches SetRetryConfig.
	if cfg.MaxRetryCredentials != -1 {
		t.Fatalf("MaxRetryCredentials = %d, want -1 (negative escape hatch must survive un-clamped through config load)", cfg.MaxRetryCredentials)
	}
}

func TestLoadConfigOptional_MaxRetryCredentialsZeroStaysZero(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("max-retry-credentials: 0\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	// An explicit 0 stays 0 at this layer; the 0 -> bounded-default (2) mapping is
	// owned by SetRetryConfig, not config load.
	if cfg.MaxRetryCredentials != 0 {
		t.Fatalf("MaxRetryCredentials = %d, want 0 (explicit 0 preserved; default-to-2 mapping lives in SetRetryConfig)", cfg.MaxRetryCredentials)
	}
}

func TestLoadConfigOptional_MaxRetryCredentialsOmittedIsZero(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	// Key omitted entirely: yaml cannot distinguish this from an explicit 0, so it
	// must land as the Go zero value 0 (the default-to-2 mapping is in SetRetryConfig).
	if err := os.WriteFile(configPath, []byte("port: 18317\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.MaxRetryCredentials != 0 {
		t.Fatalf("MaxRetryCredentials = %d, want 0 (omitted key is indistinguishable from explicit 0 at config load)", cfg.MaxRetryCredentials)
	}
}

func TestLoadConfigOptional_MaxRetryCredentialsPositivePreserved(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("max-retry-credentials: 5\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	// A positive explicit cap passes through untouched (used as-is by SetRetryConfig).
	if cfg.MaxRetryCredentials != 5 {
		t.Fatalf("MaxRetryCredentials = %d, want 5 (explicit positive cap preserved as-is)", cfg.MaxRetryCredentials)
	}
}

// ParseConfigBytes is the home remote-config overlay leg; it carries the same
// fork(anticorr) no-clamp note as LoadConfigOptional, so the escape hatch must
// survive un-clamped here too.
func TestParseConfigBytes_MaxRetryCredentialsNegativeSurvivesUnclamped(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("max-retry-credentials: -1\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.MaxRetryCredentials != -1 {
		t.Fatalf("MaxRetryCredentials = %d, want -1 (negative escape hatch must survive un-clamped through the overlay parse leg)", cfg.MaxRetryCredentials)
	}
}

func TestParseConfigBytes_MaxRetryCredentialsZeroStaysZero(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("max-retry-credentials: 0\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.MaxRetryCredentials != 0 {
		t.Fatalf("MaxRetryCredentials = %d, want 0 (explicit 0 preserved on the overlay parse leg)", cfg.MaxRetryCredentials)
	}
}
