package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrdinaryMergedConfigEntryPoints(t *testing.T) {
	const configYAML = "logs-compress-after-days: 0\nlogs-delete-after-days: -3\nlogging-display-timezone-offset-hours: 99\nerror-log-alert: {feishu-webhook-url: '  https://example.invalid/test  '}\nrouting: {strategy: round-robin, claude-code-session-affinity: true}\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := ParseConfigBytes([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []*Config{file, mem} {
		if cfg.LogsCompressAfterDays != 0 || cfg.LogsDeleteAfterDays != DefaultLogsDeleteAfterDays || cfg.LoggingDisplayTimezoneOffsetHours != DefaultLoggingDisplayTimezoneOffsetHours || cfg.ErrorLogAlert.FeishuWebhookURL != "https://example.invalid/test" || !cfg.Routing.ClaudeCodeSessionAffinity {
			t.Fatal("configuration entry points lost the old values or accepted an invalid bound")
		}
	}
	prepared, err := PrepareConfigFileForUploadWithOptional(path, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePreparedConfigBytes(prepared)
	if err != nil || got.SourceHash != ComputeSourceHash(prepared) {
		t.Fatal("source hash must identify the prepared bytes")
	}
}
func TestUnsupportedRoutingKeepsOriginalFile(t *testing.T) {
	input := []byte("routing: {strategy: ' ADAPTIVE '}\naccount-scheduling: {rate-scale: 0.3}\n# retained operator note\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	_, a := LoadConfig(path)
	_, b := ParseConfigBytes(input)
	_, c := PrepareConfigFileForUploadWithOptional(path, true)
	for _, err := range []error{a, b, c} {
		if err == nil || !strings.Contains(err.Error(), "adaptive") {
			t.Fatal("unsupported scheduling must not default to round-robin")
		}
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != string(input) {
		t.Fatal("unsupported input was rewritten")
	}
}
