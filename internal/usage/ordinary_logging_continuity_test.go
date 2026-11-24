package usage

import (
	"context"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageLoggerRespectsStatisticsToggle(t *testing.T) {
	before := StatisticsEnabled()
	t.Cleanup(func() { SetStatisticsEnabled(before) })
	stats := NewRequestStatistics()
	logger := &LoggerPlugin{stats: stats}
	record := coreusage.Record{Provider: "fixture", Model: "example"}

	SetStatisticsEnabled(false)
	logger.HandleUsage(context.Background(), record)
	if stats.Snapshot().TotalRequests != 0 {
		t.Fatal("disabled statistics recorded a request")
	}

	SetStatisticsEnabled(true)
	logger.HandleUsage(context.Background(), record)
	if stats.Snapshot().TotalRequests != 1 {
		t.Fatal("enabled statistics did not record the request exactly once")
	}
}

func TestPricingBillableTokenCountIncludesBothCacheKinds(t *testing.T) {
	tokens := TokenStats{TotalTokens: 1000, CacheReadTokens: 500, CacheWriteTokens: 100}
	if got := billableTokenCount(tokens); got != 1600 {
		t.Fatalf("billableTokenCount = %d, want 1600", got)
	}
}
