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
