package usage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func transactionFixture(name string, input int64) StatisticsSnapshot {
	return StatisticsSnapshot{APIs: map[string]APISnapshot{
		name: {Models: map[string]ModelSnapshot{"fixture": {Details: []RequestDetail{{
			Timestamp: time.Unix(1800000000, 0).UTC(),
			Source:    "synthetic-group", LatencyMs: 54, TTFTMs: 12,
			RequestID: name, Tokens: TokenStats{InputTokens: input, TotalTokens: input},
		}}}}},
	}}
}

func TestImportAndSaveIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	first := NewRequestStatistics()
	stale := NewRequestStatistics()
	for _, store := range []*RequestStatistics{first, stale} {
		if err := store.SetPersistencePath(path); err != nil {
			t.Fatal(err)
		}
	}

	if result, err := first.ImportAndSave(transactionFixture("old-label", 7)); err != nil || result.Added != 1 {
		t.Fatalf("import = %+v, %v", result, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ImportAndSave(transactionFixture("invalid", -1)); !errors.Is(err, ErrUsageImportInvalid) {
		t.Fatalf("invalid import error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || first.Snapshot().TotalRequests != 1 {
		t.Fatalf("rejected import modified history: %v", err)
	}

	if _, err := stale.ImportAndSave(transactionFixture("second", 3)); err != nil {
		t.Fatal(err)
	}
	restored := NewRequestStatistics()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	snapshot := restored.Snapshot()
	if snapshot.TotalRequests != 2 {
		t.Fatalf("restored request count = %d", snapshot.TotalRequests)
	}
	rows := snapshot.APIs["old-label"].Models["fixture"].Details
	if len(rows) != 1 || rows[0].RequestID != "old-label" || rows[0].Source != "synthetic-group" ||
		rows[0].LatencyMs != 54 || rows[0].TTFTMs != 12 {
		t.Fatalf("old group or details changed: %+v", rows)
	}
}

func TestImportAndSaveUnwritableDoesNotPublish(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	path := filepath.Join(parent, "history.json")
	store := NewRequestStatistics()
	if err := store.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	incoming := StatisticsSnapshot{APIs: map[string]APISnapshot{
		"group": {Models: map[string]ModelSnapshot{"fixture": {Details: []RequestDetail{{
			Timestamp: time.Unix(1800000000, 0).UTC(),
			Tokens:    TokenStats{InputTokens: 1, TotalTokens: 1},
		}}}}},
	}}
	if result, err := store.ImportAndSave(incoming); err == nil || result.Added != 0 {
		t.Fatalf("unwritable import = %+v, %v", result, err)
	}
	if snapshot := store.Snapshot(); snapshot.TotalRequests != 0 || len(snapshot.APIs) != 0 {
		t.Fatalf("unwritable import published: %+v", snapshot)
	}
}

func TestImportAndSaveAfterFailedRestore(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "history.json")
	bad := filepath.Join(dir, "bad.json")
	for _, path := range []string{history, bad} {
		if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := NewRequestStatistics()
	if err := store.SetPersistencePath(history); err == nil {
		t.Fatal("initial restore unexpectedly succeeded")
	}
	if _, err := store.ImportAndSave(transactionFixture("first", 1)); !errors.Is(err, ErrUsageSnapshotChanged) {
		t.Fatalf("import after restore failure = %v", err)
	}
	if bytes, err := os.ReadFile(history); err != nil || string(bytes) != "broken" ||
		store.Snapshot().TotalRequests != 0 {
		t.Fatalf("failed restore changed original history or memory: %v", err)
	}
	if err := writeUsageSnapshot(history, []byte(`{"version":2,"usage":{"apis":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersistencePath(history); err != nil {
		t.Fatal(err)
	}
	if result, err := store.ImportAndSave(transactionFixture("first", 1)); err != nil || result.Added != 1 {
		t.Fatalf("import after successful rebind = %+v, %v", result, err)
	}
	if err := store.SetPersistencePath(bad); err == nil {
		t.Fatal("switch to bad history unexpectedly succeeded")
	}
	if result, err := store.ImportAndSave(transactionFixture("second", 2)); err != nil || result.Added != 1 {
		t.Fatalf("import to previously bound history = %+v, %v", result, err)
	}
	if bytes, err := os.ReadFile(bad); err != nil || string(bytes) != "broken" {
		t.Fatalf("failed switch changed candidate file: %v", err)
	}
	reopened := NewRequestStatistics()
	if err := reopened.SetPersistencePath(history); err != nil || reopened.Snapshot().TotalRequests != 2 {
		t.Fatalf("reopen original bound history: %v", err)
	}
	memoryOnly := NewRequestStatistics()
	if result, err := memoryOnly.ImportAndSave(transactionFixture("standalone", 3)); err != nil ||
		result.Added != 1 || memoryOnly.Snapshot().TotalRequests != 1 {
		t.Fatalf("intentional in-memory import = %+v, %v", result, err)
	}
}
