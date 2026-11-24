package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryFileStorePreservesDisabledLifecycleFields(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"type":"test","disabled":true,"auto_quarantined":true,"quarantine_reason":"terminal_auth_failure","quarantined_at":"2026-01-01T00:00:00Z","expires_at":"2027-01-01T00:00:00Z","prefix":"/fixture/"}`)
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := NewFileTokenStore().WithBaseDir(dir).List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one record, got %d / %v", len(records), err)
	}
	record := records[0]
	if record.Status != coreauth.StatusDisabled || !record.Disabled || !record.AutoQuarantined || !record.Unavailable ||
		record.QuarantineReason != "terminal_auth_failure" || record.QuarantinedAt.IsZero() ||
		record.Prefix != "fixture" || !record.NextRefreshAfter.Equal(time.Date(2026, 12, 31, 23, 40, 0, 0, time.UTC)) {
		t.Fatalf("ordinary lifecycle fields changed: %+v", record)
	}
}
