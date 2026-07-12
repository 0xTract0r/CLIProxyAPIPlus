package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
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

func TestRequestStatisticsSnapshotWithOptionsTrimsDetailsOnly(t *testing.T) {
	stats := NewRequestStatistics()
	oldTime := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	recentTime := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	for _, requestedAt := range []time.Time{oldTime, recentTime} {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: requestedAt,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})
	}

	withoutDetails := stats.SnapshotWithOptions(SnapshotOptions{ExcludeDetails: true})
	if withoutDetails.TotalRequests != 2 || withoutDetails.TotalTokens != 60 {
		t.Fatalf("aggregate snapshot = requests %d tokens %d, want 2/60", withoutDetails.TotalRequests, withoutDetails.TotalTokens)
	}
	if details := withoutDetails.APIs["test-key"].Models["gpt-5.4"].Details; len(details) != 0 {
		t.Fatalf("excluded details len = %d, want 0", len(details))
	}

	recentOnly := stats.SnapshotWithOptions(SnapshotOptions{
		Since:       recentTime.Add(-time.Minute),
		DetailLimit: 1,
	})
	details := recentOnly.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("recent details len = %d, want 1", len(details))
	}
	if !details[0].Timestamp.Equal(recentTime) {
		t.Fatalf("recent detail timestamp = %s, want %s", details[0].Timestamp, recentTime)
	}
	if recentOnly.TotalRequests != 2 {
		t.Fatalf("recent aggregate total requests = %d, want full aggregate 2", recentOnly.TotalRequests)
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
	if snapshot.TotalBillableTokens != 165 {
		t.Fatalf("TotalBillableTokens = %d, want 165", snapshot.TotalBillableTokens)
	}
	details := snapshot.APIs["test-key"].Models["claude-sonnet-4.6"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].Tokens.TotalTokens != 15 {
		t.Fatalf("detail total tokens = %d, want 15", details[0].Tokens.TotalTokens)
	}
	if details[0].Tokens.BillableTokens != 165 {
		t.Fatalf("detail billable tokens = %d, want 165", details[0].Tokens.BillableTokens)
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

func TestRequestStatisticsRecordCapturesRequestID(t *testing.T) {
	stats := NewRequestStatistics()
	ctx := internallogging.WithRequestID(context.Background(), "req-abc123")
	stats.Record(ctx, coreusage.Record{
		APIKey: "test-key",
		Model:  "gpt-5.4",
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
	if details[0].RequestID != "req-abc123" {
		t.Fatalf("RequestID = %q, want %q", details[0].RequestID, "req-abc123")
	}
}

func TestRequestStatisticsRecordRequestIDEmptyWithoutContext(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey: "test-key",
		Model:  "gpt-5.4",
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
	if details[0].RequestID != "" {
		t.Fatalf("RequestID = %q, want empty", details[0].RequestID)
	}
}

func TestSnapshotPageWithOptionsOrdersFiltersAndPaginatesOutOfOrderDetails(t *testing.T) {
	stats := NewRequestStatistics()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Record out of chronological order to exercise the stable sort path.
	timestamps := []time.Time{
		base.Add(3 * time.Minute),
		base.Add(1 * time.Minute),
		base.Add(4 * time.Minute),
		base.Add(2 * time.Minute),
		base.Add(5 * time.Minute),
	}
	for _, ts := range timestamps {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: ts,
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	// First page: since=base (exclusive), limit=2 -> earliest two after base: +1m, +2m.
	page := stats.SnapshotPageWithOptions(SnapshotOptions{Since: base, DetailLimit: 2})
	details := page.Snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("page1 details len = %d, want 2", len(details))
	}
	if !details[0].Timestamp.Equal(base.Add(time.Minute)) || !details[1].Timestamp.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("page1 details out of order: %+v", details)
	}
	if !page.HasMore {
		t.Fatalf("page1 HasMore = false, want true")
	}
	wantNextSince := base.Add(2 * time.Minute)
	if !page.NextSince.Equal(wantNextSince) {
		t.Fatalf("page1 NextSince = %s, want %s", page.NextSince, wantNextSince)
	}

	// Second page resumes from NextSince (exclusive) with the same limit.
	page2 := stats.SnapshotPageWithOptions(SnapshotOptions{Since: page.NextSince, DetailLimit: 2})
	details2 := page2.Snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details2) != 2 {
		t.Fatalf("page2 details len = %d, want 2", len(details2))
	}
	if !details2[0].Timestamp.Equal(base.Add(3*time.Minute)) || !details2[1].Timestamp.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("page2 details out of order: %+v", details2)
	}
	if !page2.HasMore {
		t.Fatalf("page2 HasMore = false, want true")
	}

	// Third page drains the remainder and reports HasMore=false.
	page3 := stats.SnapshotPageWithOptions(SnapshotOptions{Since: page2.NextSince, DetailLimit: 2})
	details3 := page3.Snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details3) != 1 {
		t.Fatalf("page3 details len = %d, want 1", len(details3))
	}
	if !details3[0].Timestamp.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("page3 detail timestamp = %s, want %s", details3[0].Timestamp, base.Add(5*time.Minute))
	}
	if page3.HasMore {
		t.Fatalf("page3 HasMore = true, want false")
	}
	if !page3.NextSince.IsZero() {
		t.Fatalf("page3 NextSince = %s, want zero", page3.NextSince)
	}

	// No params: full snapshot in ascending order, no pagination.
	full := stats.SnapshotPageWithOptions(SnapshotOptions{})
	fullDetails := full.Snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(fullDetails) != len(timestamps) {
		t.Fatalf("full details len = %d, want %d", len(fullDetails), len(timestamps))
	}
	for i := 1; i < len(fullDetails); i++ {
		if fullDetails[i].Timestamp.Before(fullDetails[i-1].Timestamp) {
			t.Fatalf("full details not sorted ascending at index %d: %+v", i, fullDetails)
		}
	}
	if full.HasMore {
		t.Fatalf("full HasMore = true, want false")
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
