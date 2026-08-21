package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFarmAutoEnrollEnabledDefaultsTrue asserts the *bool accessor treats an
// unset switch (the Go zero value for the pointer, nil) as enabled, so a stock
// config keeps auto-enrolling brand-new accounts into the device farm without
// an explicit opt-in and never trips the bool zero-value trap.
func TestFarmAutoEnrollEnabledDefaultsTrue(t *testing.T) {
	if !FarmAutoEnrollEnabled(nil) {
		t.Fatal("FarmAutoEnrollEnabled(nil) = false, want true (nil cfg defaults enabled)")
	}
	if !FarmAutoEnrollEnabled(&Config{}) {
		t.Fatal("FarmAutoEnrollEnabled(&Config{}) = false, want true (unset pointer defaults enabled)")
	}

	enabled := true
	if !FarmAutoEnrollEnabled(&Config{FarmAutoEnroll: &enabled}) {
		t.Fatal("FarmAutoEnrollEnabled(true) = false, want true")
	}
	disabled := false
	if FarmAutoEnrollEnabled(&Config{FarmAutoEnroll: &disabled}) {
		t.Fatal("FarmAutoEnrollEnabled(false) = true, want false")
	}
}

// TestLoadConfigFarmAutoEnrollDefaultsEnabled asserts that a config that never
// mentions farm-auto-enroll loads with the switch unset (nil) and therefore
// enabled, matching pre-toggle behavior.
func TestLoadConfigFarmAutoEnrollDefaultsEnabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.FarmAutoEnroll != nil {
		t.Fatalf("FarmAutoEnroll = %v, want nil for a config that omits the key", *cfg.FarmAutoEnroll)
	}
	if !FarmAutoEnrollEnabled(cfg) {
		t.Fatal("farm auto-enroll should default to enabled when the key is absent")
	}
}

// TestLoadConfigFarmAutoEnrollDisabledParsesFalse asserts an explicit
// farm-auto-enroll: false round-trips to a non-nil false pointer so the gate
// reads a real disabled value rather than falling back to the enabled default.
func TestLoadConfigFarmAutoEnrollDisabledParsesFalse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("farm-auto-enroll: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.FarmAutoEnroll == nil {
		t.Fatal("FarmAutoEnroll = nil, want non-nil false for an explicit farm-auto-enroll: false")
	}
	if *cfg.FarmAutoEnroll {
		t.Fatal("FarmAutoEnroll = true, want false")
	}
	if FarmAutoEnrollEnabled(cfg) {
		t.Fatal("FarmAutoEnrollEnabled = true after explicit disable, want false")
	}
}

// TestSaveConfigPreserveCommentsKeepsFarmAutoEnrollFalse guards the persistence
// path: because the field is a *bool (not a plain bool with `omitempty`),
// writing an explicit false must survive a save/reload cycle rather than being
// dropped as an empty value.
func TestSaveConfigPreserveCommentsKeepsFarmAutoEnrollFalse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	disabled := false
	cfg.FarmAutoEnroll = &disabled
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !strings.Contains(string(raw), "farm-auto-enroll: false") {
		t.Fatalf("persisted config missing farm-auto-enroll: false, got:\n%s", raw)
	}

	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.FarmAutoEnroll == nil || *reloaded.FarmAutoEnroll {
		t.Fatalf("reloaded FarmAutoEnroll = %v, want non-nil false", reloaded.FarmAutoEnroll)
	}
	if FarmAutoEnrollEnabled(reloaded) {
		t.Fatal("FarmAutoEnrollEnabled = true after reload of a disabled config, want false")
	}
}
