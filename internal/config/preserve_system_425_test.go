package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreserveSystem425SaveClone(t *testing.T) {
	for _, initial := range []string{"claude-api-key:\n  - api-key: test425\n", "claude-api-key:\n  - api-key: test425\n    cloak:\n      preserve-caller-system: true\n      sensitive-words: [SECRET]\n"} {
		t.Run(initial, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			for _, value := range []bool{false, true, false} {
				cfg := &Config{ClaudeKey: []ClaudeKey{{APIKey: "test425", Cloak: &CloakConfig{PreserveCallerSystem: &value, SensitiveWords: []string{"SECRET"}}}}}
				clone := cfg.CloneForRuntime()
				*clone.ClaudeKey[0].Cloak.PreserveCallerSystem = !value
				if *cfg.ClaudeKey[0].Cloak.PreserveCallerSystem != value {
					t.Fatal("pointer clone aliases")
				}
				if err := SaveConfigPreserveComments(p, cfg); err != nil {
					t.Fatal(err)
				}
				raw, _ := os.ReadFile(p)
				if !strings.Contains(string(raw), "preserve-caller-system:") {
					t.Fatal("pointer flag pruned")
				}
				loaded, err := LoadConfigOptional(p, false)
				if err != nil {
					t.Fatal(err)
				}
				c := loaded.ClaudeKey[0].Cloak
				if c == nil || c.PreserveCallerSystem == nil || *c.PreserveCallerSystem != value || len(c.SensitiveWords) != 1 || c.SensitiveWords[0] != "SECRET" {
					t.Fatal("save/load lost flag or sibling")
				}
			}
			cfg := &Config{ClaudeKey: []ClaudeKey{{APIKey: "test425", Cloak: &CloakConfig{}}}}
			if err := SaveConfigPreserveComments(p, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfigOptional(p, false)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.ClaudeKey[0].Cloak != nil && loaded.ClaudeKey[0].Cloak.PreserveCallerSystem != nil {
				t.Fatal("nil not removed")
			}
		})
	}
}
