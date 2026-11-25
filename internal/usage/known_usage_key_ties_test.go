package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKnownUsageKeyTiesFromDisk(t *testing.T) {
	key := "synthetic-fixture-key"
	h := usageKeyBucket(key)
	makeDetail := func(id string) RequestDetail {
		return RequestDetail{RequestID: id, Timestamp: time.Unix(1700, 0), Tokens: TokenStats{InputTokens: 2, TotalTokens: 2}}
	}
	entry := func(ds ...RequestDetail) APISnapshot {
		return APISnapshot{Models: map[string]ModelSnapshot{"fixture": {Details: ds}}}
	}
	old := StatisticsSnapshot{APIs: map[string]APISnapshot{
		key: entry(makeDetail("a"), makeDetail("mirror"), makeDetail(""), makeDetail("")),
		h:   entry(makeDetail("b"), makeDetail("mirror"), makeDetail("")),
	}}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := NewRequestStatistics()
	s.SetKnownUsageAPIKeys([]string{key})
	if err := s.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	assert := func(s *RequestStatistics) {
		t.Helper()
		snap := s.Snapshot()
		if snap.TotalRequests != 5 || len(snap.APIs) != 1 {
			t.Errorf("total=%d buckets=%d", snap.TotalRequests, len(snap.APIs))
		}
		counts := map[string]int{}
		for _, d := range snap.APIs[h].Models["fixture"].Details {
			counts[d.RequestID]++
		}
		if counts["a"] != 1 || counts["b"] != 1 || counts["mirror"] != 1 || counts[""] != 2 {
			t.Errorf("identity counts %v", counts)
		}
	}
	assert(s)
	if got := s.MergeSnapshot(old); got.Added != 0 {
		t.Errorf("reimport added %d", got.Added)
	}
	if err := s.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	again := NewRequestStatistics()
	again.SetKnownUsageAPIKeys([]string{key})
	if err := again.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	assert(again)
}

func TestReviewLegacyOrdinarySpacingKeepsStoredMultiplicity(t *testing.T) {
	row := RequestDetail{Timestamp: time.Unix(1700, 0), Source: "same", Tokens: TokenStats{InputTokens: 2, TotalTokens: 2}}
	group := APISnapshot{Models: map[string]ModelSnapshot{"fixture": {Details: []RequestDetail{row}}}}
	fixture := StatisticsSnapshot{APIs: map[string]APISnapshot{"legacy": group, " legacy ": group}}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	stats := NewRequestStatistics()
	if err = stats.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if got := stats.Snapshot().TotalRequests; got != 2 {
		t.Fatalf("ordinary legacy total=%d, want 2", got)
	}
	result, err := stats.ImportSnapshot(fixture)
	if err != nil || result.Added != 0 {
		t.Fatalf("reimport=%+v err=%v", result, err)
	}
	if err = stats.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	cold := NewRequestStatistics()
	if err = cold.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if got := cold.Snapshot().TotalRequests; got != 2 {
		t.Fatalf("cold legacy total=%d, want 2", got)
	}

}

func TestReviewKnownUsageKeyMultipleLabelsAndFiles(t *testing.T) {
	const key = "synthetic_known_fixture"
	canonical := usageKeyBucket(key)
	legacyHash := "sha256:" + usageKeyHash(canonical)
	snapshot := func(label, source string, ids ...string) StatisticsSnapshot {
		var ds []RequestDetail
		for _, id := range ids {
			ds = append(ds, RequestDetail{RequestID: id, Timestamp: time.Unix(1700, 0), Source: source, Tokens: TokenStats{InputTokens: 2, TotalTokens: 2}})
		}
		return StatisticsSnapshot{APIs: map[string]APISnapshot{label: {Models: map[string]ModelSnapshot{"fixture": {Details: ds}}}}}
	}
	stats := NewRequestStatistics()
	stats.SetKnownUsageAPIKeys([]string{key})
	for _, tc := range []struct {
		fixture StatisticsSnapshot
		added   int64
	}{
		{snapshot(key, "source-a", "mirror", "independent-a"), 2},
		{snapshot(legacyHash, "source-b", "mirror", "independent-b"), 1},
		{snapshot(canonical, "source-c", "mirror", "independent-c"), 1},
	} {
		b, err := json.Marshal(tc.fixture)
		if err != nil {
			t.Fatal(err)
		}
		var decoded StatisticsSnapshot
		if err = json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := stats.ImportSnapshot(decoded)
		if err != nil || got.Added != tc.added {
			t.Fatalf("independent import added=%d want=%d err=%v", got.Added, tc.added, err)
		}
	}
	s := stats.Snapshot()
	if s.TotalRequests != 4 || len(s.APIs) != 1 {
		t.Fatalf("total=%d buckets=%d", s.TotalRequests, len(s.APIs))
	}
}
