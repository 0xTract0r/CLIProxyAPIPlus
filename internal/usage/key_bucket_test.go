package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestKnownUsageKeyColdLoadLiveAndReimport(t *testing.T) {
	const key = "synthetic-inbound-key-fixture"
	bucket := usageKeyBucket(key)
	t0 := time.Unix(1500, 0)
	detail := RequestDetail{Timestamp: t0, Tokens: TokenStats{InputTokens: 10, TotalTokens: 10}}
	entry := APISnapshot{Models: map[string]ModelSnapshot{"fixture": {Details: []RequestDetail{detail}}}}
	old := StatisticsSnapshot{APIs: map[string]APISnapshot{key: entry, bucket: entry, "anonymous": entry, "POST /v1/chat/completions": entry, "fixture-provider": entry}}
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
	if got := s.Snapshot().TotalRequests; got != 4 {
		t.Fatalf("old/new representations double counted: %d", got)
	}
	s.HandleUsage(context.Background(), coreusage.Record{Provider: "fixture-provider", Model: "fixture", APIKey: key, RequestedAt: t0.Add(time.Second), Detail: coreusage.Detail{InputTokens: 2, TotalTokens: 2}})
	now := s.Snapshot()
	if now.TotalRequests != 5 || len(now.APIs) != 4 {
		t.Fatalf("unexpected requests=%d buckets=%d", now.TotalRequests, len(now.APIs))
	}
	for _, label := range []string{"anonymous", "POST /v1/chat/completions", "fixture-provider"} {
		if got := now.APIs[label].Models["fixture"].Details; len(got) != 1 || got[0].APIKeyHash != "" {
			t.Fatalf("unrelated label changed: %q", label)
		}
	}
	for _, d := range now.APIs[bucket].Models["fixture"].Details {
		if d.Source != bucket || d.APIKeyHash != bucket[2:] {
			t.Fatal("expected CPAMP digest identity")
		}
	}
	if got := s.MergeSnapshot(old); got.Added != 0 {
		t.Fatalf("legacy import duplicated %d", got.Added)
	}
	if got := s.MergeSnapshot(now); got.Added != 0 {
		t.Fatalf("export import duplicated %d", got.Added)
	}
	if err := s.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || bytes.Contains(persisted, []byte(key)) {
		t.Fatal("known key appears in persisted history")
	}
	fresh := NewRequestStatistics()
	fresh.SetKnownUsageAPIKeys([]string{key})
	if err := fresh.SetPersistencePath(path); err != nil || fresh.Snapshot().TotalRequests != 5 {
		t.Fatal("unexpected reopened history")
	}
}
