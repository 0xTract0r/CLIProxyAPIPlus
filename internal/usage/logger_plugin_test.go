package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
	if got := snapshot.TotalCostUSD; got <= 0 {
		t.Fatalf("total cost = %f, want > 0", got)
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestRequestStatisticsTracksUnfinalizedPricing(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey: "test-key",
		Model:  "gpt-5.3-codex-spark",
		Detail: coreusage.Detail{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	})

	snapshot := stats.Snapshot()
	if snapshot.TotalCostUSD != 0 {
		t.Fatalf("TotalCostUSD = %f, want 0", snapshot.TotalCostUSD)
	}
	if snapshot.PricingStatus != string(pricingStateUnfinalized) {
		t.Fatalf("PricingStatus = %q, want %q", snapshot.PricingStatus, pricingStateUnfinalized)
	}
	if snapshot.UnfinalizedRequestCount != 1 {
		t.Fatalf("UnfinalizedRequestCount = %d, want 1", snapshot.UnfinalizedRequestCount)
	}
}

func TestRequestStatisticsDoesNotFoldCacheTokensIntoTotal(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey: "test-key",
		Model:  "claude-sonnet-4.6",
		Detail: coreusage.Detail{
			InputTokens:      10,
			OutputTokens:     5,
			CacheReadTokens:  100,
			CacheWriteTokens: 50,
		},
	})

	snapshot := stats.Snapshot()
	if snapshot.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want 15", snapshot.TotalTokens)
	}
	details := snapshot.APIs["test-key"].Models["claude-sonnet-4.6"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].Tokens.TotalTokens != 15 {
		t.Fatalf("detail total tokens = %d, want 15", details[0].Tokens.TotalTokens)
	}
	if details[0].Tokens.CacheReadTokens != 100 || details[0].Tokens.CacheWriteTokens != 50 {
		t.Fatalf("cache tokens = read %d write %d, want read 100 write 50", details[0].Tokens.CacheReadTokens, details[0].Tokens.CacheWriteTokens)
	}
	if snapshot.TotalCostUSD <= 0 {
		t.Fatalf("TotalCostUSD = %f, want > 0", snapshot.TotalCostUSD)
	}
}

func TestRequestStatisticsPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-statistics.json")
	stats := NewRequestStatistics()
	if err := stats.SetPersistencePath(path); err != nil {
		t.Fatalf("SetPersistencePath() error = %v", err)
	}

	timestamp := time.Date(2026, 4, 17, 8, 30, 0, 0, time.UTC)
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: timestamp,
		Latency:     750 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  11,
			OutputTokens: 19,
			TotalTokens:  30,
		},
	})

	waitForUsageFile(t, path)

	restored := NewRequestStatistics()
	if err := restored.SetPersistencePath(path); err != nil {
		t.Fatalf("restored.SetPersistencePath() error = %v", err)
	}

	snapshot := restored.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	if snapshot.TotalTokens != 30 {
		t.Fatalf("TotalTokens = %d, want 30", snapshot.TotalTokens)
	}
	if snapshot.TotalCostUSD <= 0 {
		t.Fatalf("TotalCostUSD = %f, want > 0", snapshot.TotalCostUSD)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 750 {
		t.Fatalf("latency_ms = %d, want 750", details[0].LatencyMs)
	}
}

func waitForUsageFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for persisted usage snapshot at %s", path)
}
