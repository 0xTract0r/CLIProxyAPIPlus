package auth

import (
	"context"
	"encoding/json"
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

func TestOrdinaryFileStoreNumericMetadataUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinary.json")
	const prior = "{\"type\":\"test\",\"disabled\":false,\"sequence\":9007199254740992,\"nested\":{\"other\":9007199254740997}}"
	if err := os.WriteFile(path, []byte(prior), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewFileTokenStore().WithBaseDir(dir)
	records, err := s.List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("list returned %d records, err=%v", len(records), err)
	}
	records[0].Metadata["sequence"] = json.Number("9007199254740993")
	if _, err := s.Save(context.Background(), records[0]); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Sequence int64
		Nested   struct{ Other int64 }
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Sequence != 9007199254740993 || saved.Nested.Other != 9007199254740997 {
		t.Fatalf("stored sequence %d, nested %d, err=%v", saved.Sequence, saved.Nested.Other, err)
	}
}
