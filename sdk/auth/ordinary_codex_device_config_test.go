package auth

import (
	"context"
	"strings"
	"testing"
)

func TestCodexDeviceLoginWithoutConfigurationRejectsLocally(t *testing.T) {
	opts := &LoginOptions{
		NoBrowser: true,
		Metadata:  map[string]string{codexLoginModeMetadataKey: codexLoginModeDevice},
	}
	record, err := (&CodexAuthenticator{}).Login(context.Background(), nil, opts)
	if record != nil || err == nil || !strings.Contains(err.Error(), "configuration is required") {
		t.Fatalf("unconfigured device login returned record=%t, error=%v", record != nil, err)
	}
}
