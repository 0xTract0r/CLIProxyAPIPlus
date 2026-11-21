package usage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageSnapshotStaleSaveReturnsConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	open := func() *RequestStatistics {
		t.Helper()
		s := NewRequestStatistics()
		if err := s.SetPersistencePath(path); err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, next := open(), open()
	if err := next.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); !errors.Is(err, ErrUsageSnapshotChanged) {
		t.Fatalf("stale close error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale close replaced snapshot")
	}
}
