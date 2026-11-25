package usage

import (
	"context"
	"testing"
	"time"
)

type ordinaryCacheRecordSink chan Record

func (sink ordinaryCacheRecordSink) HandleUsage(_ context.Context, record Record) { sink <- record }

func TestOrdinaryUsageSDKRetainsLegacyRecordFields(t *testing.T) {
	sink := make(ordinaryCacheRecordSink, 1)
	manager := NewManager(1)
	manager.Register(sink)
	defer manager.Stop()
	first := int64(0)
	manager.Publish(context.Background(), Record{
		SessionID: "synthetic-session",
		Telemetry: &Telemetry{FirstVisibleContentMS: &first},
		Detail:    Detail{CacheWriteTokens: 19, CacheCreationTokens: 11},
	})
	select {
	case record := <-sink:
		if record.SessionID != "synthetic-session" || record.Telemetry == nil ||
			record.Telemetry.FirstVisibleContentMS == nil || *record.Telemetry.FirstVisibleContentMS != 0 ||
			record.Detail.CacheWriteTokens != 19 || record.Detail.CacheCreationTokens != 11 {
			t.Fatalf("SDK changed reported fields: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("SDK did not publish the local record")
	}
}

func TestOrdinaryUsageCacheWriteAccountingDoesNotDoubleCount(t *testing.T) {
	for _, tc := range []struct {
		name            string
		legacy, current int64
	}{
		{"legacy-only", 10, 0}, {"official-only", 0, 10},
		{"duplicate", 10, 10}, {"official-wins", 90, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := EnsureTokenBreakdownForProvider(Detail{InputTokens: 6, OutputTokens: 2,
				CacheReadTokens: 4, CacheWriteTokens: tc.legacy, CacheCreationTokens: tc.current}, "claude", "")
			if d.CacheCreationTokens != 10 || d.TotalTokens != 22 || d.TokenBreakdown.Input.CacheWriteTokens != 10 {
				t.Fatalf("cache creation normalized or counted incorrectly: %+v", d)
			}
		})
	}
}
