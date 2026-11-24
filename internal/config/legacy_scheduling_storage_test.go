package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStandardConfigPreservesDormantSchedulingWithoutAddingDefaults(t *testing.T) {
	for _, example := range []struct {
		name   string
		legacy string
	}{
		{name: "absent"},
		{name: "explicit historical data", legacy: "account-scheduling:\n  enabled: false\n  rate-scale: 0.3\nfarm-auto-enroll: false\n"},
	} {
		t.Run(example.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			original := []byte("routing:\n  strategy: round-robin\n# legacy operator comment\n" + example.legacy)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || cfg.Routing.Strategy != "round-robin" {
				t.Fatalf("normal routing rejected preserved legacy data: %v", err)
			}
			cfg.LogsCompressAfterDays = 2
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]any
			if err := yaml.Unmarshal(original, &before); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(saved, &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before["account-scheduling"], after["account-scheduling"]) ||
				!reflect.DeepEqual(before["farm-auto-enroll"], after["farm-auto-enroll"]) ||
				!strings.Contains(string(saved), "# legacy operator comment") {
				t.Fatal("saving a normal setting changed old scheduling data or introduced an implicit configuration")
			}
			cold, err := LoadConfig(path)
			if err != nil || cold.Routing.Strategy != "round-robin" || cold.LogsCompressAfterDays != 2 {
				t.Fatalf("normal settings did not survive save/load: %v", err)
			}
		})
	}
}
