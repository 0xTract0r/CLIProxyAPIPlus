package config

import "testing"

// TestParseConfigBytes_NormalizeAccountEnvDormant closes the all-path anti-misfire gap:
// ParseConfigBytes is the parser used for the home remote config overlay
// (sdk/cliproxy/service.go StartConfigSubscriber -> applyHomeOverlay). Like the
// on-disk LoadConfigOptional path, it must neutralize `normalize-account-env: true`
// so a remotely-pushed enable can never resurrect the retired cwd-normalization
// chain. The gate itself stays honest for a directly-constructed pointer so the
// dormant normalize implementations remain unit-testable.
func TestParseConfigBytes_NormalizeAccountEnvDormant(t *testing.T) {
	// Payload that explicitly tries to turn the switch ON via remote config.
	onCfg, err := ParseConfigBytes([]byte("normalize-account-env: true\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes(on) error = %v", err)
	}
	// The parsed pointer must be neutralized to nil regardless of the payload.
	if onCfg.NormalizeAccountEnv != nil {
		t.Fatalf("NormalizeAccountEnv = %v (non-nil), want nil after neutralization; remote-config enable must not stick", *onCfg.NormalizeAccountEnv)
	}
	// And the effective gate must report OFF.
	if NormalizeAccountEnvEnabled(onCfg) {
		t.Fatal("NormalizeAccountEnvEnabled = true after `normalize-account-env: true` via ParseConfigBytes, want false (dormant / anti-misfire)")
	}

	// A payload that does not mention the switch is off as well.
	offCfg, err := ParseConfigBytes([]byte("port: 8080\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes(off) error = %v", err)
	}
	if NormalizeAccountEnvEnabled(offCfg) {
		t.Fatal("NormalizeAccountEnvEnabled = true with no switch set (ParseConfigBytes), want false")
	}
}
