package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV8MigrationPreservesForkRuntimeSettings(t *testing.T) {
	raw := []byte(`max-retry-credentials: -1
farm-auto-enroll: false
codex:
  identity-confuse: true
managed-header-profile:
  online-update: false
  fetch-timeout-seconds: 7
  cache-ttl-seconds: 123
quota-snapshot-refresh:
  enabled: false
  startup-catch-up: false
account-scheduling:
  warmup-serving-reserve: 0.15
ampcode:
  upstream-url: https://amp.example.invalid
  upstream-api-key: local-fixture
  restrict-management-to-localhost: false
logging-display-timezone-offset-hours: 8
kiro:
  - token-file: /fixture/kiro.json
claude:
  sonnet_long_context_policy: compact_required
  normalize-sdk-cli-entrypoint: false
claude-header-defaults:
  pin-software-baseline: true
  farm-os: Linux
  farm-arch: x64
  replay-wire-header-order: true
codex-api-key:
  - api-key: fixture-only
    base-url: https://codex.example.invalid
    fast-models: [gpt-5.6]
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(migrated); err != nil {
		t.Fatal(err)
	}
	actual, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !actual.Codex.IdentityConfuse {
		t.Fatal("Codex identity-confuse lost")
	}
	if actual.ManagedHeaderProfile.OnlineUpdate == nil || *actual.ManagedHeaderProfile.OnlineUpdate || actual.ManagedHeaderProfile.FetchTimeoutSeconds != 7 || actual.ManagedHeaderProfile.CacheTTLSeconds != 123 {
		t.Fatal("managed header profile lost")
	}
	if actual.MaxRetryCredentials != -1 {
		t.Fatalf("retry escape hatch lost: %d", actual.MaxRetryCredentials)
	}
	if actual.FarmAutoEnroll == nil || *actual.FarmAutoEnroll {
		t.Fatal("farm enrollment false lost")
	}
	if actual.QuotaSnapshotRefresh.Enabled == nil || *actual.QuotaSnapshotRefresh.Enabled || actual.QuotaSnapshotRefresh.StartupCatchUp == nil || *actual.QuotaSnapshotRefresh.StartupCatchUp {
		t.Fatal("quota explicit false lost")
	}
	if actual.AmpCode.UpstreamURL != "https://amp.example.invalid" || actual.AmpCode.UpstreamAPIKey != "local-fixture" || actual.AmpCode.RestrictManagementToLocalhost {
		t.Fatal("Amp runtime fields lost")
	}
	if len(actual.KiroKey) != 1 || actual.KiroKey[0].TokenFile != "/fixture/kiro.json" {
		t.Fatal("Kiro settings lost")
	}
	if actual.Claude.SonnetLongContextPolicy != "compact_required" || actual.Claude.NormalizeSdkCliEntrypoint == nil || *actual.Claude.NormalizeSdkCliEntrypoint {
		t.Fatal("Claude fork policy lost")
	}
	if !actual.ClaudeHeaderDefaults.PinSoftwareBaseline {
		t.Fatal("explicit software pin lost")
	}
	if actual.ClaudeHeaderDefaults.FarmOS != "Linux" || actual.ClaudeHeaderDefaults.FarmArch != "x64" || actual.ClaudeHeaderDefaults.ReplayWireHeaderOrder == nil || !*actual.ClaudeHeaderDefaults.ReplayWireHeaderOrder {
		t.Fatal("farm wire profile lost")
	}
	if len(actual.CodexKey) != 1 || len(actual.CodexKey[0].FastModels) != 1 || actual.CodexKey[0].FastModels[0] != "gpt-5.6" {
		t.Fatal("Fast opt-in lost")
	}
}

func TestV8ClaudeSoftwarePinHistoricalAlias(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"old true", "oauth: {providers: {claude: {header-defaults: {pin-software-baseline: true}}}}", true},
		{"old false", "oauth: {providers: {claude: {header-defaults: {pin-software-baseline: false}}}}", false},
		{"canonical false overrides old true", "oauth: {providers: {claude: {header-defaults: {pin-software-baseline: true}}}}\nupstream: {claude: {header-defaults: {pin-software-baseline: false}}}", false},
		{"canonical true overrides old false", "oauth: {providers: {claude: {header-defaults: {pin-software-baseline: false}}}}\nupstream: {claude: {header-defaults: {pin-software-baseline: true}}}", true},
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
			if cfg.ClaudeHeaderDefaults.PinSoftwareBaseline != tc.want {
				t.Fatalf("runtime pin=%t want=%t", cfg.ClaudeHeaderDefaults.PinSoftwareBaseline, tc.want)
			}
			if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateV8Config(raw); err != nil {
				t.Fatal(err)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			node := yamlPath(doc.Content[0], "upstream.claude.header-defaults.pin-software-baseline")
			if node == nil {
				t.Fatal("migration lost explicit pin setting")
			}
			var value bool
			if err := node.Decode(&value); err != nil || value != tc.want {
				t.Fatalf("canonical pin=%t err=%v", value, err)
			}
			if yamlPath(doc.Content[0], "oauth.providers.claude.header-defaults.pin-software-baseline") != nil {
				t.Fatal("historical pin alias remains active")
			}
			back, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if back.ClaudeHeaderDefaults.PinSoftwareBaseline != tc.want {
				t.Fatal("saved pin changed on reload")
			}
		})
	}
}
