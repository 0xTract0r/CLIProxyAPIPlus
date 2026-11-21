package usage

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageValidityRejectsMalformedCLISinkRecords(t *testing.T) {
	stats := NewRequestStatistics()
	sink := &LoggerPlugin{stats: stats}
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := stats.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	publish := func(detail coreusage.Detail, at time.Time) {
		sink.HandleUsage(t.Context(), coreusage.Record{
			APIKey: "fixture", Model: "ordinary-usage-fixture", Detail: detail, RequestedAt: at,
		})
	}
	stamp := time.Unix(1700000000, 0).UTC()
	publish(coreusage.Detail{TotalTokens: 4}, stamp)
	for _, malformed := range []coreusage.Detail{
		{TotalTokens: -1},
		{TotalTokens: 3, OutputTokens: -1},
		{InputTokens: math.MaxInt64, OutputTokens: 1},
		{TotalTokens: math.MaxInt64, CacheReadTokens: 1},
		{TotalTokens: math.MaxInt64},
	} {
		publish(malformed, stamp.Add(time.Second))
	}
	publish(coreusage.Detail{TotalTokens: 3}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	publish(coreusage.Detail{TotalTokens: 5}, stamp.Add(2*time.Second))
	if err := stats.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	reopened := NewRequestStatistics()
	if err := reopened.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot(); got.TotalRequests != 2 || got.TotalTokens != 9 {
		t.Fatalf("reopened CLI sink history = %d requests, %d tokens", got.TotalRequests, got.TotalTokens)
	}
}
