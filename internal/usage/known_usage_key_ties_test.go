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
