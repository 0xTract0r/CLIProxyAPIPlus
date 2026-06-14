package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_ManagedHeaderOnlineUpdateDefaultsOff pins requirement ⑥ plan A:
// the managed-header online-update (npm) flag must default to OFF when the config
// file does not mention it, so the runtime never claims a claude-cli version no
// real client here has presented. An explicit "true" must still be honored.
func TestLoadConfig_ManagedHeaderOnlineUpdateDefaultsOff(t *testing.T) {
	dir := t.TempDir()

	// Config without any managed-header-profile section: default must be OFF.
	defaultPath := filepath.Join(dir, "default.yaml")
	if err := os.WriteFile(defaultPath, []byte("port: 8080\n"), 0o600); err != nil {
		t.Fatalf("failed to write default config: %v", err)
	}
	defaultCfg, err := LoadConfigOptional(defaultPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional(default) error = %v", err)
	}
	if defaultCfg.ManagedHeaderProfile.OnlineUpdate == nil {
		t.Fatal("OnlineUpdate = nil, want non-nil default")
	}
	if *defaultCfg.ManagedHeaderProfile.OnlineUpdate {
		t.Fatalf("OnlineUpdate default = true, want false (npm must not be a ceiling)")
	}
	if ManagedHeaderOnlineUpdateEnabled(defaultCfg) {
		t.Fatalf("ManagedHeaderOnlineUpdateEnabled = true by default, want false")
	}

	// Explicit opt-in must still be honored.
	enabledPath := filepath.Join(dir, "enabled.yaml")
	enabledYAML := []byte("managed-header-profile:\n  online-update: true\n")
	if err := os.WriteFile(enabledPath, enabledYAML, 0o600); err != nil {
		t.Fatalf("failed to write enabled config: %v", err)
	}
	enabledCfg, err := LoadConfigOptional(enabledPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional(enabled) error = %v", err)
	}
	if !ManagedHeaderOnlineUpdateEnabled(enabledCfg) {
		t.Fatalf("ManagedHeaderOnlineUpdateEnabled = false, want true when explicitly opted in")
	}
}
