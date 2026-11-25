package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A negative value in an existing settings file must not select unbounded
// credential retries. Both readers normalize it to the default marker.
func TestLoadConfigOptional_MaxRetryCredentialsNegativeUsesDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("max-retry-credentials: -1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.MaxRetryCredentials != 0 {
		t.Fatalf("MaxRetryCredentials = %d, want the bounded default marker 0", cfg.MaxRetryCredentials)
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
func TestParseConfigBytes_MaxRetryCredentialsNegativeUsesDefault(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("max-retry-credentials: -1\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.MaxRetryCredentials != 0 {
		t.Fatalf("MaxRetryCredentials = %d, want the bounded default marker 0", cfg.MaxRetryCredentials)
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
