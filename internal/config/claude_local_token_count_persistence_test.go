package config

import (
	"bytes"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeLocalTokenCountCanonicalPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"omitted", "# operator comment\nport: 8317\n", false},
		{"legacy true", "# operator comment\nport: 8317\nclaude:\n  local-token-count: true\n", true},
		{"legacy false", "# operator comment\nport: 8317\nclaude:\n  local-token-count: false\n", false},
		{"v8 true", "# operator comment\nserver:\n  port: 8317\nupstream:\n  claude:\n    local-token-count: true\n", true},
		{"v8 false", "# operator comment\nserver:\n  port: 8317\nupstream:\n  claude:\n    local-token-count: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Claude.LocalTokenCount != tc.want {
				t.Fatalf("initial loaded local=%t want=%t", cfg.Claude.LocalTokenCount, tc.want)
			}
			for step, want := range []bool{tc.want, true, false, true} {
				cfg.Claude.LocalTokenCount = want
				if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
					t.Fatal(err)
				}
				persisted, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				var tree map[string]any
				if err = yaml.Unmarshal(persisted, &tree); err != nil {
					t.Fatal(err)
				}
				upstream, _ := tree["upstream"].(map[string]any)
				claude, _ := upstream["claude"].(map[string]any)
				got, present := claude["local-token-count"].(bool)
				if got != want || (want && !present) {
					t.Fatalf("step%d canonical value=%v present=%t want=%t", step, got, present, want)
				}
				if _, legacy := tree["claude"]; legacy {
					t.Fatal("explicit v8 save retained legacy claude root")
				}
				loaded, errLoad := LoadConfig(path)
				if errLoad != nil {
					t.Fatal(errLoad)
				}
				parsed, errParse := ParseConfigBytes(persisted)
				if errParse != nil {
					t.Fatal(errParse)
				}
				if loaded.Claude.LocalTokenCount != want || parsed.Claude.LocalTokenCount != want {
					t.Fatalf("step%d disk/load/parse mismatch", step)
				}
				if !bytes.Contains(persisted, []byte("operator comment")) {
					t.Fatal("lost operator comment")
				}
				cfg = loaded
			}
		})
	}
}
