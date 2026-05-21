package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigOptionalQuotaSnapshotRefreshDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if !QuotaSnapshotRefreshEnabled(cfg) {
		t.Fatal("quota snapshot refresh should default to enabled")
	}
	if got := QuotaSnapshotRefreshInterval(cfg); got != 45*time.Minute {
		t.Fatalf("interval = %s, want 45m", got)
	}
	if got := QuotaSnapshotRefreshJitter(cfg); got != 10*time.Minute {
		t.Fatalf("jitter = %s, want 10m", got)
	}
	if !QuotaSnapshotRefreshStartupCatchUp(cfg) {
		t.Fatal("startup catch-up should default to enabled")
	}
	if got := QuotaSnapshotRefreshStartupMaxStaleness(cfg); got != 24*time.Hour {
		t.Fatalf("startup max staleness = %s, want 24h", got)
	}
}

func TestLoadConfigOptionalQuotaSnapshotRefreshCustomPolicy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`quota-snapshot-refresh:
  enabled: false
  interval: 30min
  jitter: 2min
  startup-catch-up: false
  startup-max-staleness: 12h
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if QuotaSnapshotRefreshEnabled(cfg) {
		t.Fatal("quota snapshot refresh enabled = true, want false")
	}
	if got := QuotaSnapshotRefreshInterval(cfg); got != 30*time.Minute {
		t.Fatalf("interval = %s, want 30m", got)
	}
	if got := QuotaSnapshotRefreshJitter(cfg); got != 2*time.Minute {
		t.Fatalf("jitter = %s, want 2m", got)
	}
	if QuotaSnapshotRefreshStartupCatchUp(cfg) {
		t.Fatal("startup catch-up = true, want false")
	}
	if got := QuotaSnapshotRefreshStartupMaxStaleness(cfg); got != 12*time.Hour {
		t.Fatalf("startup max staleness = %s, want 12h", got)
	}
}

func TestLoadConfigOptionalQuotaSnapshotRefreshZeroJitterAndStartupStaleness(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`quota-snapshot-refresh:
  enabled: true
  interval: 15m
  jitter: 0m
  startup-catch-up: true
  startup-max-staleness: 0m
`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := QuotaSnapshotRefreshInterval(cfg); got != 15*time.Minute {
		t.Fatalf("interval = %s, want 15m", got)
	}
	if got := QuotaSnapshotRefreshJitter(cfg); got != 0 {
		t.Fatalf("jitter = %s, want 0", got)
	}
	if got := QuotaSnapshotRefreshStartupMaxStaleness(cfg); got != 0 {
		t.Fatalf("startup max staleness = %s, want 0", got)
	}
}

func TestLoadConfigOptionalQuotaSnapshotRefreshCanonicalWebYAML(t *testing.T) {
	tests := []struct {
		name               string
		enabled            string
		startupCatchUp     string
		wantEnabled        bool
		wantStartupCatchUp bool
	}{
		{
			name:               "enabled with startup catch-up",
			enabled:            "true",
			startupCatchUp:     "true",
			wantEnabled:        true,
			wantStartupCatchUp: true,
		},
		{
			name:               "disabled without startup catch-up",
			enabled:            "false",
			startupCatchUp:     "false",
			wantEnabled:        false,
			wantStartupCatchUp: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte(`quota-snapshot-refresh:
  enabled: ` + tt.enabled + `
  interval: 15m
  jitter: 3m
  startup-catch-up: ` + tt.startupCatchUp + `
  startup-max-staleness: 2h
`)
			if err := os.WriteFile(configPath, data, 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadConfigOptional(configPath, false)
			if err != nil {
				t.Fatalf("LoadConfigOptional() error = %v", err)
			}

			if got := QuotaSnapshotRefreshEnabled(cfg); got != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", got, tt.wantEnabled)
			}
			if got := QuotaSnapshotRefreshInterval(cfg); got != 15*time.Minute {
				t.Fatalf("interval = %s, want 15m", got)
			}
			if got := QuotaSnapshotRefreshJitter(cfg); got != 3*time.Minute {
				t.Fatalf("jitter = %s, want 3m", got)
			}
			if got := QuotaSnapshotRefreshStartupCatchUp(cfg); got != tt.wantStartupCatchUp {
				t.Fatalf("startup catch-up = %v, want %v", got, tt.wantStartupCatchUp)
			}
			if got := QuotaSnapshotRefreshStartupMaxStaleness(cfg); got != 2*time.Hour {
				t.Fatalf("startup max staleness = %s, want 2h", got)
			}
		})
	}
}

func TestSaveConfigPreserveCommentsKeepsQuotaSnapshotRefreshFalseFlags(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 18317\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	disabled := false
	cfg.QuotaSnapshotRefresh.Enabled = &disabled
	cfg.QuotaSnapshotRefresh.StartupCatchUp = &disabled

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{"quota-snapshot-refresh:", "enabled: false", "startup-catch-up: false"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("saved config missing %q:\n%s", want, rendered)
		}
	}
}
