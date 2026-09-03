package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

// TestSnapshotWithOptionsDetailLimitKeepsNewestTail is a direction-locking
// regression test for the live /usage path. When DetailLimit truncates an
// over-limit window, the live snapshot must keep the *newest* DetailLimit
// records (tail), not the oldest. A prior refactor delegated
// SnapshotWithOptions to the export cursor path and silently returned the
// oldest records instead, hiding the most recent events from the UI.
func TestSnapshotWithOptionsDetailLimitKeepsNewestTail(t *testing.T) {
	stats := NewRequestStatistics()
	base := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	const total = 12
	// Record out of chronological order to prove the live path sorts before
	// taking the tail rather than relying on insertion order.
	order := []int{5, 0, 11, 3, 8, 1, 10, 2, 9, 4, 7, 6}
	for _, i := range order {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: base.Add(time.Duration(i) * time.Minute),
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
	}

	const limit = 4
	snap := stats.SnapshotWithOptions(SnapshotOptions{DetailLimit: limit})
	details := snap.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != limit {
		t.Fatalf("live details len = %d, want %d", len(details), limit)
	}
	// Tail = the newest `limit` records: minutes 8,9,10,11 in ascending order.
	for i, detail := range details {
		want := base.Add(time.Duration(total-limit+i) * time.Minute)
		if !detail.Timestamp.Equal(want) {
			t.Fatalf("live detail[%d] timestamp = %s, want %s (newest tail)", i, detail.Timestamp, want)
		}
	}
	// The single newest record must be present (regression: it was dropped).
	if last := details[len(details)-1].Timestamp; !last.Equal(base.Add(time.Duration(total-1) * time.Minute)) {
		t.Fatalf("live newest detail = %s, want %s", last, base.Add(time.Duration(total-1)*time.Minute))
	}
}

// TestSnapshotWithOptionsSinceIsInclusive locks the live path's Since
// semantics to baseline: a record exactly at the Since boundary is retained
// (>= Since), unlike the export cursor path which is strictly-after.
func TestSnapshotWithOptionsSinceIsInclusive(t *testing.T) {
	stats := NewRequestStatistics()
	boundary := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{boundary.Add(-time.Minute), boundary, boundary.Add(time.Minute)} {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: ts,
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
	}

	snap := stats.SnapshotWithOptions(SnapshotOptions{Since: boundary})
	details := snap.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("inclusive-Since details len = %d, want 2 (boundary + after)", len(details))
	}
	if !details[0].Timestamp.Equal(boundary) {
		t.Fatalf("first detail = %s, want boundary %s (inclusive)", details[0].Timestamp, boundary)
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

// TestSnapshotPageWithOptionsAppliesGlobalLimitAcrossBuckets is a regression
// test for a production bug where DetailLimit was applied independently to
// each (api, model) bucket instead of globally: /usage/export?limit=5000
// returned 124,083 details in one page because each bucket had fewer than
// 5000 records and was therefore never truncated. This asserts the combined
// page across all buckets is capped at DetailLimit.
func TestSnapshotPageWithOptionsAppliesGlobalLimitAcrossBuckets(t *testing.T) {
	stats := NewRequestStatistics()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// 3 api keys x 4 models = 12 buckets, 10 records each = 120 total, all
	// with distinct nanosecond timestamps so there is no boundary tie. A
	// naive per-bucket limit of 8 would never truncate any of these buckets
	// (10 > 8 in every bucket, so it *would* truncate per-bucket too -
	// instead use a limit that is well below any single bucket's count but
	// also below the combined total, and assert the total, not per-bucket).
	apiKeys := []string{"key-a", "key-b", "key-c"}
	models := []string{"model-1", "model-2", "model-3", "model-4"}
	perBucket := 10
	total := len(apiKeys) * len(models) * perBucket
	for bi, apiKey := range apiKeys {
		for bj, model := range models {
			for k := 0; k < perBucket; k++ {
				// Globally unique offsets so every record has a distinct
				// timestamp and global ordering is unambiguous.
				offset := time.Duration(bi*len(models)*perBucket+bj*perBucket+k) * time.Second
				stats.Record(context.Background(), coreusage.Record{
					APIKey:      apiKey,
					Model:       model,
					RequestedAt: base.Add(offset),
					Detail: coreusage.Detail{
						InputTokens:  1,
						OutputTokens: 1,
						TotalTokens:  2,
					},
				})
			}
		}
	}

	// Use a limit bigger than any single bucket (10) but much smaller than
	// the combined total (120), which is exactly the case the per-bucket bug
	// missed: no individual bucket exceeds the limit, so a per-bucket
	// implementation would never truncate and would return all 120 records.
	const globalLimit = 25
	page := stats.SnapshotPageWithOptions(SnapshotOptions{DetailLimit: globalLimit})

	gotTotal := 0
	for _, api := range page.Snapshot.APIs {
		for _, model := range api.Models {
			gotTotal += len(model.Details)
		}
	}
	if gotTotal != globalLimit {
		t.Fatalf("combined details across all buckets = %d, want exactly %d (global limit)", gotTotal, globalLimit)
	}
	if !page.HasMore {
		t.Fatalf("HasMore = false, want true (only %d of %d records returned)", globalLimit, total)
	}
	if page.NextSince.IsZero() {
		t.Fatalf("NextSince = zero, want a cursor for the next page")
	}

	// The global cutoff must be the timestamp of the 25th earliest record
	// overall (0-indexed 24): base + 24s.
	wantCutoff := base.Add(24 * time.Second)
	if !page.NextSince.Equal(wantCutoff) {
		t.Fatalf("NextSince = %s, want %s", page.NextSince, wantCutoff)
	}
}

// TestSnapshotPageWithOptionsPaginatesAcrossBucketsWithoutGapsOrDuplicates
// drives full pagination (repeated calls with Since=previous NextSince)
// across multiple api/model buckets and asserts the union of all pages is
// exactly the full record set with no duplicates and no gaps, i.e. the
// global cursor is gap-free and duplicate-free even though records are
// interleaved across buckets and inserted out of chronological order.
func TestSnapshotPageWithOptionsPaginatesAcrossBucketsWithoutGapsOrDuplicates(t *testing.T) {
	stats := NewRequestStatistics()
	base := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	apiKeys := []string{"key-a", "key-b", "key-c"}
	models := []string{"model-1", "model-2"}
	perBucket := 17 // deliberately not a multiple of the page size below
	total := len(apiKeys) * len(models) * perBucket

	// Insert in a shuffled (non-chronological) order per bucket to exercise
	// the sort path, while keeping every timestamp globally unique.
	type recordSpec struct {
		apiKey string
		model  string
		offset time.Duration
	}
	var specs []recordSpec
	seq := 0
	for _, apiKey := range apiKeys {
		for _, model := range models {
			for k := 0; k < perBucket; k++ {
				specs = append(specs, recordSpec{apiKey: apiKey, model: model, offset: time.Duration(seq) * time.Second})
				seq++
			}
		}
	}
	// Reverse insertion order within the spec list to make writes
	// out-of-order relative to timestamp, without changing the assigned
	// timestamps themselves.
	for i := len(specs) - 1; i >= 0; i-- {
		spec := specs[i]
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      spec.apiKey,
			Model:       spec.model,
			RequestedAt: base.Add(spec.offset),
			Detail: coreusage.Detail{
				InputTokens:  1,
				OutputTokens: 1,
				TotalTokens:  2,
			},
		})
	}

	const pageSize = 9
	seen := make(map[string]bool, total)
	var since time.Time
	pages := 0
	for {
		pages++
		if pages > total { // guard against an infinite loop on a bug
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page := stats.SnapshotPageWithOptions(SnapshotOptions{Since: since, DetailLimit: pageSize})
		pageCount := 0
		for apiKey, api := range page.Snapshot.APIs {
			for model, modelSnapshot := range api.Models {
				for _, detail := range modelSnapshot.Details {
					key := apiKey + "|" + model + "|" + detail.Timestamp.Format(time.RFC3339Nano)
					if seen[key] {
						t.Fatalf("duplicate record across pages: %s", key)
					}
					seen[key] = true
					pageCount++
				}
			}
		}
		if !page.HasMore {
			if pageCount == 0 && pages > 1 {
				t.Fatalf("page %d returned 0 records with HasMore=false unexpectedly", pages)
			}
			break
		}
		if pageCount == 0 {
			t.Fatalf("page %d returned 0 records but HasMore=true", pages)
		}
		since = page.NextSince
	}

	if len(seen) != total {
		t.Fatalf("total records observed across all pages = %d, want %d (gap or drop detected)", len(seen), total)
	}
}

// TestSnapshotPageWithOptionsKeepsTiedTimestampsInSamePage asserts the
// documented same-timestamp boundary policy: when the DetailLimit cutoff
// falls in the middle of a run of records sharing the exact same
// (nanosecond-precision) timestamp, all tied records are kept in the current
// page rather than being split across pages. This avoids ever dropping or
// duplicating a record at the cost of the page occasionally being slightly
// larger than DetailLimit when such a collision occurs.
func TestSnapshotPageWithOptionsKeepsTiedTimestampsInSamePage(t *testing.T) {
	stats := NewRequestStatistics()
	base := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	tied := base.Add(time.Minute) // shared boundary timestamp

	// 3 records strictly before the tie, then 4 records all sharing the same
	// timestamp, then 2 records strictly after. limit=4 lands exactly in the
	// middle of the tied run (rank 3 of 0..8, i.e. the 4th earliest record is
	// one of the tied ones).
	stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: base, Detail: coreusage.Detail{TotalTokens: 1}})
	stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: base.Add(10 * time.Second), Detail: coreusage.Detail{TotalTokens: 1}})
	stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: base.Add(20 * time.Second), Detail: coreusage.Detail{TotalTokens: 1}})
	for i := 0; i < 4; i++ {
		stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: tied, Detail: coreusage.Detail{TotalTokens: 1}})
	}
	stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: base.Add(90 * time.Second), Detail: coreusage.Detail{TotalTokens: 1}})
	stats.Record(context.Background(), coreusage.Record{APIKey: "k", Model: "m", RequestedAt: base.Add(100 * time.Second), Detail: coreusage.Detail{TotalTokens: 1}})

	page := stats.SnapshotPageWithOptions(SnapshotOptions{DetailLimit: 4})
	details := page.Snapshot.APIs["k"].Models["m"].Details
	// 3 strictly-before records + all 4 tied records = 7, even though
	// DetailLimit=4: the tied run is not split.
	if len(details) != 7 {
		t.Fatalf("details len = %d, want 7 (3 before + 4 tied, tie not split)", len(details))
	}
	tiedCount := 0
	for _, d := range details {
		if d.Timestamp.Equal(tied) {
			tiedCount++
		}
	}
	if tiedCount != 4 {
		t.Fatalf("tied-timestamp records included = %d, want 4 (all)", tiedCount)
	}
	if !page.NextSince.Equal(tied) {
		t.Fatalf("NextSince = %s, want %s (shared boundary timestamp)", page.NextSince, tied)
	}
	if !page.HasMore {
		t.Fatalf("HasMore = false, want true (2 records remain after the tie)")
	}

	// Next page (Since=tied, strictly-after) must return exactly the 2
	// remaining records with no re-inclusion of the tied run.
	page2 := stats.SnapshotPageWithOptions(SnapshotOptions{Since: page.NextSince, DetailLimit: 4})
	details2 := page2.Snapshot.APIs["k"].Models["m"].Details
	if len(details2) != 2 {
		t.Fatalf("page2 details len = %d, want 2", len(details2))
	}
	for _, d := range details2 {
		if !d.Timestamp.After(tied) {
			t.Fatalf("page2 unexpectedly re-included a record at or before the boundary: %+v", d)
		}
	}
	if page2.HasMore {
		t.Fatalf("page2 HasMore = true, want false")
	}
}

// TestRequestStatisticsRecordCapturesSessionIDFromRecordField covers the
// P6 session-aggregation slice: a Record that already carries a SessionID
// (e.g. a future usage-reporter change that bakes it in at construction time,
// mirroring how ServiceTier/ReasoningEffort are captured today) must have it
// persisted onto the stored RequestDetail unchanged.
func TestRequestStatisticsRecordCapturesSessionIDFromRecordField(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:    "test-key",
		Model:     "gpt-5.4",
		SessionID: "claude:from-record-field",
		Detail: coreusage.Detail{
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if got := details[0].SessionID; got != "claude:from-record-field" {
		t.Fatalf("SessionID = %q, want %q", got, "claude:from-record-field")
	}
}

// TestRequestStatisticsRecordCapturesSessionIDFromContextFallback covers the
// path a request-entry wiring slice will actually use: the record itself
// carries no SessionID, but ctx was populated via coreauth.WithSessionID
// (e.g. by the request entry point after calling ExtractSessionID). Record
// must fall back to reading it off ctx, exactly like RequestID already does
// via internallogging.GetRequestID.
func TestRequestStatisticsRecordCapturesSessionIDFromContextFallback(t *testing.T) {
	stats := NewRequestStatistics()
	ctx := coreauth.WithSessionID(context.Background(), "claude:from-ctx")
	stats.Record(ctx, coreusage.Record{
		APIKey: "test-key",
		Model:  "gpt-5.4",
		Detail: coreusage.Detail{
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if got := details[0].SessionID; got != "claude:from-ctx" {
		t.Fatalf("SessionID = %q, want %q", got, "claude:from-ctx")
	}
}

// TestRequestStatisticsRecordSessionIDEmptyWithoutSource covers the "unknown
// is not a number" contract: when neither the record field nor ctx carries a
// session id, the stored detail must have an explicitly empty SessionID, not
// a substituted placeholder.
func TestRequestStatisticsRecordSessionIDEmptyWithoutSource(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey: "test-key",
		Model:  "gpt-5.4",
		Detail: coreusage.Detail{
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if got := details[0].SessionID; got != "" {
		t.Fatalf("SessionID = %q, want empty", got)
	}
}

// TestSessionAggregateForAuthIndex_ActiveAndClosedBuckets covers the core
// bucketing contract: a session's most recent recorded request determines
// whether it counts as active (idle < activeWithin), closed (idle >=
// closedAfter), or neither (counted in Total only). It also exercises the
// "most recent wins" rule for a session with multiple recorded requests.
func TestSessionAggregateForAuthIndex_ActiveAndClosedBuckets(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const authIndex = "authidx-buckets-1"

	record := func(sessionID string, at time.Time) {
		ctx := coreauth.WithSessionID(context.Background(), sessionID)
		stats.Record(ctx, coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			AuthIndex:   authIndex,
			RequestedAt: at,
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
	}

	// s-active: two requests, most recent 2min ago -- must win over the older
	// 30min-ago request when determining idle time (last-seen, not first-seen).
	record("s-active", now.Add(-30*time.Minute))
	record("s-active", now.Add(-2*time.Minute))
	// s-mid: idle 20min, strictly between the two thresholds -- neither bucket.
	record("s-mid", now.Add(-20*time.Minute))
	// s-closed: idle 40min, past closedAfter.
	record("s-closed", now.Add(-40*time.Minute))

	aggregate := stats.SessionAggregateForAuthIndex(authIndex, now, 10*time.Minute, 30*time.Minute)
	if aggregate.Total != 3 {
		t.Fatalf("Total = %d, want 3", aggregate.Total)
	}
	if aggregate.Active != 1 {
		t.Fatalf("Active = %d, want 1 (s-active only, using its most recent request)", aggregate.Active)
	}
	if aggregate.Closed != 1 {
		t.Fatalf("Closed = %d, want 1 (s-closed only)", aggregate.Closed)
	}
}

// TestSessionAggregateForAuthIndex_EmptySessionIDsExcluded guards the "误并
// 成一个空 key 大桶" failure mode: requests that carry no session id at all
// (ExtractSessionID could not classify them) must never be aggregated into a
// shared "" bucket, no matter how many such requests are recorded for the
// account.
func TestSessionAggregateForAuthIndex_EmptySessionIDsExcluded(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const authIndex = "authidx-empty-session-1"

	for i := 0; i < 5; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			AuthIndex:   authIndex,
			RequestedAt: now.Add(-time.Duration(i) * time.Minute),
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
	}

	aggregate := stats.SessionAggregateForAuthIndex(authIndex, now, 10*time.Minute, 30*time.Minute)
	if aggregate.Total != 0 {
		t.Fatalf("Total = %d, want 0 (empty-session-id requests must never form a bucket)", aggregate.Total)
	}
	if aggregate.Active != 0 || aggregate.Closed != 0 {
		t.Fatalf("Active/Closed = %d/%d, want 0/0", aggregate.Active, aggregate.Closed)
	}
}

// TestSessionAggregateForAuthIndex_MessageHashPrimaryIDsNotDoubleCounted
// covers the level-8 legacy message-hash fallback (extractMessageHashIDs):
// repeated requests that all resolve to the SAME primary session id (the
// common case once a conversation has an assistant reply pinned in its
// history) must count as exactly ONE session, never once per request. It
// also documents the known, deliberate limitation that a conversation's
// first-turn (short-hash) id and its later-turn (full-hash) id are not
// merged into a single session by this aggregation (see
// SessionAggregateForAuthIndex's doc comment).
func TestSessionAggregateForAuthIndex_MessageHashPrimaryIDsNotDoubleCounted(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const authIndex = "authidx-msg-hash-1"

	turn1Payload := []byte(`{"messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"hello there"}` +
		`]}`)
	turn2Payload := []byte(`{"messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"hello there"},` +
		`{"role":"assistant","content":"hi, how can I help?"},` +
		`{"role":"user","content":"what is 2+2?"}` +
		`]}`)

	idTurn1 := coreauth.ExtractSessionID(nil, turn1Payload, nil)
	idTurn2 := coreauth.ExtractSessionID(nil, turn2Payload, nil)
	if idTurn1 == "" || idTurn2 == "" {
		t.Fatalf("expected non-empty message-hash session ids, got turn1=%q turn2=%q", idTurn1, idTurn2)
	}
	if idTurn1 == idTurn2 {
		t.Fatalf("turn1 (short hash, no assistant) and turn2 (full hash, with assistant) unexpectedly matched: %q", idTurn1)
	}

	record := func(sessionID string, at time.Time) {
		ctx := coreauth.WithSessionID(context.Background(), sessionID)
		stats.Record(ctx, coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			AuthIndex:   authIndex,
			RequestedAt: at,
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
	}

	// Turn 1 request, classified under the short-hash primary id.
	record(idTurn1, now.Add(-20*time.Minute))
	// Turn 2 and a follow-up turn 3 both resolve to the SAME full-hash primary
	// id (firstUserMsg/firstAssistantMsg are pinned to the conversation's
	// first occurrences and do not change on later turns) -- publishing two
	// separate records under it must not inflate the session count.
	record(idTurn2, now.Add(-5*time.Minute))
	record(idTurn2, now.Add(-1*time.Minute))

	aggregate := stats.SessionAggregateForAuthIndex(authIndex, now, 10*time.Minute, 30*time.Minute)
	if aggregate.Total != 2 {
		t.Fatalf("Total = %d, want 2 (turn1's short-hash session + turn2/3's shared full-hash session, no per-request double count)", aggregate.Total)
	}
	if aggregate.Active != 1 {
		t.Fatalf("Active = %d, want 1 (only the turn2/3 session's latest request -1min falls inside the 10min active window)", aggregate.Active)
	}
	if aggregate.Closed != 0 {
		t.Fatalf("Closed = %d, want 0 (turn1's session is idle 20min, between the active/closed thresholds, not yet closed)", aggregate.Closed)
	}
}

// TestSessionAggregateForAuthIndex_UnknownOrEmptyAuthIndex covers defensive
// handling: an empty authIndex, and an authIndex with no matching recorded
// details, must both return the zero aggregate rather than matching
// unrelated accounts' sessions.
func TestSessionAggregateForAuthIndex_UnknownOrEmptyAuthIndex(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	ctx := coreauth.WithSessionID(context.Background(), "claude:some-session")
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		AuthIndex:   "authidx-owns-the-session",
		RequestedAt: now.Add(-time.Minute),
		Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})

	if got := stats.SessionAggregateForAuthIndex("", now, 10*time.Minute, 30*time.Minute); got != (SessionAggregate{}) {
		t.Fatalf("SessionAggregateForAuthIndex(\"\") = %+v, want zero value", got)
	}
	if got := stats.SessionAggregateForAuthIndex("authidx-does-not-exist", now, 10*time.Minute, 30*time.Minute); got != (SessionAggregate{}) {
		t.Fatalf("SessionAggregateForAuthIndex(unknown) = %+v, want zero value", got)
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
