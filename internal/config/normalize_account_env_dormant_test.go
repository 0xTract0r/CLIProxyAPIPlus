package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_NormalizeAccountEnvDormant pins the anticorr decision that cwd
// normalization (requirement ⑦) is DORMANT: even if an operator writes
// `normalize-account-env: true` in config.yaml, LoadConfig neutralizes it so the
// runtime never re-enables the retired cwd-normalization chain. This is the
// anti-misfire guard (方案甲): the value is zeroed at load time rather than in the
// gate function, so function-level unit tests can still construct a Config with
// the pointer set and exercise the dormant normalize implementations directly.
func TestLoadConfig_NormalizeAccountEnvDormant(t *testing.T) {
	dir := t.TempDir()

	// Config that explicitly tries to turn the switch ON.
	onPath := filepath.Join(dir, "on.yaml")
	if err := os.WriteFile(onPath, []byte("normalize-account-env: true\n"), 0o600); err != nil {
		t.Fatalf("failed to write on config: %v", err)
	}
	onCfg, err := LoadConfigOptional(onPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional(on) error = %v", err)
	}
	// The loaded pointer must be neutralized to nil regardless of the file.
	if onCfg.NormalizeAccountEnv != nil {
		t.Fatalf("NormalizeAccountEnv = %v (non-nil), want nil after neutralization; config-file enable must not stick", *onCfg.NormalizeAccountEnv)
	}
	// And the effective gate must report OFF.
	if NormalizeAccountEnvEnabled(onCfg) {
		t.Fatal("NormalizeAccountEnvEnabled = true after `normalize-account-env: true`, want false (dormant / anti-misfire)")
	}

	// A config that does not mention the switch is off as well.
	offPath := filepath.Join(dir, "off.yaml")
	if err := os.WriteFile(offPath, []byte("port: 8080\n"), 0o600); err != nil {
		t.Fatalf("failed to write off config: %v", err)
	}
	offCfg, err := LoadConfigOptional(offPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional(off) error = %v", err)
	}
	if NormalizeAccountEnvEnabled(offCfg) {
		t.Fatal("NormalizeAccountEnvEnabled = true with no switch set, want false")
	}

	// The gate itself still honors a directly-constructed pointer so unit tests of
	// the dormant implementations keep working; only the config-file path is severed.
	on := true
	if !NormalizeAccountEnvEnabled(&Config{NormalizeAccountEnv: &on}) {
		t.Fatal("NormalizeAccountEnvEnabled(direct &Config{on}) = false, want true (gate must stay honest for unit tests)")
	}
}
