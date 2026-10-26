package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptionalLogRetentionDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 18317\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.LogsCompressAfterDays != DefaultLogsCompressAfterDays {
		t.Fatalf("LogsCompressAfterDays = %d, want %d", cfg.LogsCompressAfterDays, DefaultLogsCompressAfterDays)
	}
	if cfg.LogsDeleteAfterDays != DefaultLogsDeleteAfterDays {
		t.Fatalf("LogsDeleteAfterDays = %d, want %d", cfg.LogsDeleteAfterDays, DefaultLogsDeleteAfterDays)
	}
}

func TestLoadConfigOptionalLogRetentionNegativeFallsBackToDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("logs-compress-after-days: -1\nlogs-delete-after-days: -2\n")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.LogsCompressAfterDays != DefaultLogsCompressAfterDays {
		t.Fatalf("LogsCompressAfterDays = %d, want %d", cfg.LogsCompressAfterDays, DefaultLogsCompressAfterDays)
	}
	if cfg.LogsDeleteAfterDays != DefaultLogsDeleteAfterDays {
		t.Fatalf("LogsDeleteAfterDays = %d, want %d", cfg.LogsDeleteAfterDays, DefaultLogsDeleteAfterDays)
	}
}

func TestLoadConfigOptionalErrorLogAlertConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("error-log-alert:\n  feishu-webhook-url: ' https://open.feishu.cn/open-apis/bot/v2/hook/test-hook '\n")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	want := "https://open.feishu.cn/open-apis/bot/v2/hook/test-hook"
	if cfg.ErrorLogAlert.FeishuWebhookURL != want {
		t.Fatalf("ErrorLogAlert.FeishuWebhookURL = %q, want %q", cfg.ErrorLogAlert.FeishuWebhookURL, want)
	}
}

func TestLoadConfigOptionalErrorLogAlertDefaultsDisabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 18317\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.ErrorLogAlert.FeishuWebhookURL != "" {
		t.Fatalf("ErrorLogAlert.FeishuWebhookURL = %q, want empty", cfg.ErrorLogAlert.FeishuWebhookURL)
	}
}

func TestParseConfigLogRetentionMatchesFileLoad(t *testing.T) {
	for _, layout := range []struct{ name, prefix string }{
		{"legacy", "port: 8317\n"},
		{"v8", "config-version: 8\nserver: {port: 8317}\n"},
	} {
		for _, tc := range []struct {
			name, fields     string
			compress, remove int
		}{
			{"omitted", "", DefaultLogsCompressAfterDays, DefaultLogsDeleteAfterDays},
			{"disabled", "logs-compress-after-days: 0\nlogs-delete-after-days: 0\n", 0, 0},
			{"positive", "logs-compress-after-days: 9\nlogs-delete-after-days: 47\n", 9, 47},
			{"negative", "logs-compress-after-days: -1\nlogs-delete-after-days: -2\n", DefaultLogsCompressAfterDays, DefaultLogsDeleteAfterDays},
			{"independent", "logs-compress-after-days: 0\nlogs-delete-after-days: -2\n", 0, DefaultLogsDeleteAfterDays},
		} {
			t.Run(layout.name+"/"+tc.name, func(t *testing.T) {
				data := []byte(layout.prefix + tc.fields)
				parsed, err := ParseConfigBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				loaded, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				for name, cfg := range map[string]*Config{"parsed": parsed, "loaded": loaded} {
					if cfg.LogsCompressAfterDays != tc.compress || cfg.LogsDeleteAfterDays != tc.remove {
						t.Fatalf("%s retention = %d/%d, want %d/%d", name, cfg.LogsCompressAfterDays, cfg.LogsDeleteAfterDays, tc.compress, tc.remove)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(data) {
					t.Fatal("normalization rewrote operator YAML")
				}
			})
		}
	}
}
