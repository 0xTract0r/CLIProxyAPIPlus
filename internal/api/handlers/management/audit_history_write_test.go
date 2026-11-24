package management

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryAuditHistoryLateWriteAndFailure(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: oldDir}, nil)
	source, err := h.openAuthStatusHistorySource()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	event := authStatusHistoryEvent{
		source: source, EventType: "fixture", Status: "disabled",
		Error: "client_secret=secret-fixture",
	}
	h.SetConfig(&config.Config{AuthDir: newDir})
	if err := h.appendAuthStatusHistoryEvent(event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(authStatusHistoryPath(oldDir))
	if err != nil || !strings.Contains(string(data), "[redacted]") || strings.Contains(string(data), "secret-fixture") {
		t.Fatal("old source did not retain safe history")
	}
	if _, err := os.Stat(authStatusHistoryPath(newDir)); !os.IsNotExist(err) {
		t.Fatal("wrong directory received an old event")
	}
	event.source = nil
	if err := h.appendAuthStatusHistoryEvent(event); err == nil {
		t.Fatal("missing source was accepted")
	}
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, authStatusHistoryDirName), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	blockedSource, err := os.OpenRoot(blocked)
	if err != nil {
		t.Fatal(err)
	}
	defer blockedSource.Close()
	event.source = blockedSource
	if err := h.appendAuthStatusHistoryEvent(event); err == nil {
		t.Fatal("write failure was hidden")
	}
}
