package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageSnapshotFailedRebindKeepsOriginalDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	stats := NewRequestStatistics()
	if err := stats.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "recovery.json")
	previous := []byte("{incomplete historical export")
	if err := os.WriteFile(broken, previous, 0600); err != nil {
		t.Fatal(err)
	}
	if err := stats.SetPersistencePath(broken); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if err := stats.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	untouched, err := os.ReadFile(broken)
	if err != nil || !bytes.Equal(untouched, previous) {
		t.Fatalf("failed restore replaced historical bytes: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("previous destination lost: %v", err)
	}
}
func TestUsageSnapshotIndependentTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	sidecar := path + ".tmp"
	keep := []byte("unrelated file")
	if err := os.WriteFile(sidecar, keep, 0644); err != nil {
		t.Fatal(err)
	}
	stats := NewRequestStatistics()
	if err := stats.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if err := stats.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sidecar); err != nil || !bytes.Equal(data, keep) {
		t.Fatalf("save consumed an unrelated file: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("usage snapshot has non-private permissions: %v", err)
	}
}
func TestUsageSnapshotConcurrentSavesKeepAllDetails(t *testing.T) {
	stats := NewRequestStatistics()
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := stats.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			detail := RequestDetail{Timestamp: time.Unix(1700000000+int64(i), 0).UTC(), Tokens: TokenStats{InputTokens: 1, TotalTokens: 1}}
			snap := StatisticsSnapshot{APIs: map[string]APISnapshot{"sample": {Models: map[string]ModelSnapshot{"sample-model": {Details: []RequestDetail{detail}}}}}}
			if stats.mergeSnapshot(snap, false).Added != 1 {
				failures <- errors.New("unique detail not imported")
				return
			}
			if err := stats.SaveToPersistence(); err != nil {
				failures <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	restored := NewRequestStatistics()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	got := restored.Snapshot()
	if got.TotalRequests != writers || got.TotalTokens != writers {
		t.Fatalf("on-disk totals requests=%d tokens=%d", got.TotalRequests, got.TotalTokens)
	}
}
func TestUsageSnapshotFormats(t *testing.T) {
	source := usageRestoreFixture()
	full := source.Snapshot()
	page := source.SnapshotPageWithOptions(SnapshotOptions{}).Snapshot
	model := full.APIs["fixture-key"].Models["fixture-model"]
	modelDetailsOnly := map[string]any{"apis": map[string]any{
		"fixture-key": map[string]any{"models": map[string]any{"fixture-model": map[string]any{"details": model.Details}}},
	}}
	for _, fixture := range []struct {
		name             string
		data             any
		wantRequestTotal int64
		wantTokens       int64
	}{
		{"bare", full, 2, 18},
		{"unversioned", map[string]any{"usage": full, "failed_requests": full.FailureCount}, 2, 18},
		{"v0", map[string]any{"version": 0, "usage": full}, 2, 18},
		{"v1", map[string]any{"version": 1, "exported_at": time.Unix(1700000000, 0).UTC(), "usage": full}, 2, 18},
		{"v2-full-export", map[string]any{"version": 2, "exported_at": time.Unix(1700000000, 0).UTC(), "usage": page}, 2, 18},
		{"details-alone", modelDetailsOnly, 2, 18},
		{"empty", NewRequestStatistics().Snapshot(), 0, 0},
		{"empty-zero-value", StatisticsSnapshot{}, 0, 0},
		{"sparse-empty", map[string]any{"apis": map[string]any{}}, 0, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := writeUsageRestoreFixture(t, fixture.data)
			restored := NewRequestStatistics()
			if err := restored.SetPersistencePath(path); err != nil {
				t.Fatal(err)
			}
			got := restored.Snapshot()
			if got.TotalRequests != fixture.wantRequestTotal || got.TotalTokens != fixture.wantTokens {
				t.Fatalf("restore = %d requests, %d tokens, want %d requests, %d tokens",
					got.TotalRequests, got.TotalTokens, fixture.wantRequestTotal, fixture.wantTokens)
			}
			if fixture.wantRequestTotal == 2 && (got.SuccessCount != 1 || got.FailureCount != 1) {
				t.Fatalf("restore lost success/failure classification: %+v", got)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := source.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	restored := NewRequestStatistics()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if got := restored.Snapshot(); got.TotalRequests != 2 || got.TotalTokens != 18 || got.FailureCount != 1 {
		t.Fatalf("real versioned writer did not round trip: %+v", got)
	}
}

func TestUsageSnapshotRejectsUnrestorableHistory(t *testing.T) {
	source := usageRestoreFixture()
	paged := source.SnapshotPageWithOptions(SnapshotOptions{DetailLimit: 1})
	if !paged.HasMore {
		t.Fatal("fixture must be an actual partial page")
	}
	lastPage := source.SnapshotPageWithOptions(SnapshotOptions{Since: time.Unix(1700000001, 0).UTC()})
	if lastPage.HasMore {
		t.Fatal("fixture must be the last page")
	}
	partial := source.Snapshot()
	api := partial.APIs["fixture-key"]
	model := api.Models["fixture-model"]
	model.Details = model.Details[:1]
	api.Models["fixture-model"] = model
	partial.APIs["fixture-key"] = api

	for _, fixture := range []struct {
		name string
		data any
	}{
		{"unrelated-json", map[string]any{"accounts": []string{"fixture"}}},
		{"nonzero-aggregate", source.SnapshotWithOptions(SnapshotOptions{ExcludeDetails: true})},
		{"fewer-stored-details", partial},
		{"truncated-export", map[string]any{"version": 2, "usage": paged.Snapshot, "has_more": paged.HasMore, "next_since": paged.NextSince}},
		{"last-page-without-cursor", map[string]any{"version": 2, "usage": lastPage.Snapshot}},
		{"malformed-detail", json.RawMessage(`{"apis":{"fixture":{"models":{"fixture":{"details":[null]}}}}}`)},
		{"malformed-wrapper", json.RawMessage(`{"version":2}`)},
		{"null-contents", json.RawMessage(`{"version":2,"usage":null}`)},
		{"unknown-version", json.RawMessage(`{"version":9,"usage":{"apis":{}}}`)},
		{"invalid-version-type", json.RawMessage(`{"version":"future","usage":{"apis":{}}}`)},
		{"null-file", json.RawMessage(`null`)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			brokenPath := writeUsageRestoreFixture(t, fixture.data)
			before, err := os.ReadFile(brokenPath)
			if err != nil {
				t.Fatal(err)
			}
			stats := usageRestoreFixture()
			boundPath := filepath.Join(t.TempDir(), "previous.json")
			if err := stats.SetPersistencePath(boundPath); err != nil {
				t.Fatal(err)
			}
			if err := stats.SetPersistencePath(brokenPath); err == nil {
				t.Fatal("unrestorable snapshot was accepted")
			}
			if err := stats.SaveToPersistence(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(brokenPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected history was altered: %v", err)
			}
			if _, err := os.Stat(boundPath); err != nil {
				t.Fatalf("previous binding was lost: %v", err)
			}
			if got := stats.Snapshot(); got.TotalRequests != 2 || got.TotalTokens != 18 {
				t.Fatalf("failed restore changed memory: %+v", got)
			}
		})
	}
}

func TestUsageSnapshotRestorePreservesRepeatedStoredRows(t *testing.T) {
	source := NewRequestStatistics()
	timestamp := time.Unix(1700000000, 0).UTC()
	for _, id := range []string{"fixture-request-a", "fixture-request-b"} {
		source.Record(internallogging.WithRequestID(context.Background(), id), coreusage.Record{
			APIKey: "fixture-key", Model: "fixture-model", RequestedAt: timestamp,
		})
	}
	path := writeUsageRestoreFixture(t, source.Snapshot())
	restored := NewRequestStatistics()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if err := restored.LoadFromPersistence(); err != nil {
		t.Fatal(err)
	}
	rows := restored.Snapshot().APIs["fixture-key"].Models["fixture-model"].Details
	if len(rows) != 2 || rows[0].RequestID != "fixture-request-a" || rows[1].RequestID != "fixture-request-b" {
		t.Fatalf("restoring the same file must keep both recorded rows exactly once: %+v", rows)
	}
	imported := NewRequestStatistics()
	if result := imported.MergeSnapshot(source.Snapshot()); result.Added != 1 || result.Skipped != 1 {
		t.Fatalf("import must retain deduplication semantics: %+v", result)
	}
}

func usageRestoreFixture() *RequestStatistics {
	stats := NewRequestStatistics()
	for i, tokens := range []int64{7, 11} {
		stats.Record(context.Background(), coreusage.Record{
			APIKey: "fixture-key", Model: "fixture-model",
			RequestedAt: time.Unix(1700000000+int64(i), 0).UTC(), Failed: i == 1,
			Detail: coreusage.Detail{InputTokens: tokens, TotalTokens: tokens},
		})
	}
	return stats
}

func writeUsageRestoreFixture(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
