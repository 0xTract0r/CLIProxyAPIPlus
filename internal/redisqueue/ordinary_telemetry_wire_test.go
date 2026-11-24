package redisqueue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestOrdinaryUsageQueuePreservesVisibleContentTiming(t *testing.T) {
	withEnabledQueue(t, func() {
		first, last, events := int64(0), int64(31), int64(2)
		p := &usageQueuePlugin{}
		p.HandleUsage(context.Background(), coreusage.Record{
			Model: "fixture", Stream: true, TTFT: 4 * time.Millisecond, Latency: 37 * time.Millisecond,
			Telemetry: &coreusage.Telemetry{
				Version: 1, FirstVisibleContentMS: &first, LastVisibleContentMS: &last,
				VisibleContentEvents: &events, VisibleContentObserved: true,
			},
		})
		payload := popSinglePayload(t)
		for key, want := range map[string]string{"ttft_ms": "4", "latency_ms": "37", "stream": "true", "failed": "false"} {
			if string(payload[key]) != want {
				t.Errorf("%s = %s, want %s", key, payload[key], want)
			}
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(payload["telemetry"], &got); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"first_visible_content_ms": "0", "last_visible_content_ms": "31", "visible_content_events": "2", "visible_content_observed": "true",
		} {
			if string(got[key]) != want {
				t.Errorf("telemetry.%s = %s, want %s", key, got[key], want)
			}
		}
		requireMissingField(t, got, "first_content_ms")
		requireMissingField(t, got, "fast_context")
	})
}

func TestOrdinaryUsageQueueNormalizesLegacyCacheWriteOnce(t *testing.T) {
	withEnabledQueue(t, func() {
		for _, tc := range []struct {
			name            string
			legacy, current int64
		}{
			{"legacy-only", 10, 0}, {"official-only", 0, 10},
			{"duplicate", 10, 10}, {"official-wins", 90, 10},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := &usageQueuePlugin{}
				p.HandleUsage(context.Background(), coreusage.Record{Model: "fixture", Provider: "claude",
					Detail: coreusage.Detail{InputTokens: 6, CacheReadTokens: 4, OutputTokens: 2,
						CacheWriteTokens: tc.legacy, CacheCreationTokens: tc.current}})
				payload := popSinglePayload(t)
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(payload["tokens"], &raw); err != nil {
					t.Fatal(err)
				}
				if string(raw["cache_creation_tokens"]) != "10" || string(raw["total_tokens"]) != "22" {
					t.Fatalf("wrong cache write wire: %s", payload["tokens"])
				}
				requireMissingField(t, raw, "cache_write_tokens")
				var breakdown coreusage.TokenBreakdown
				if err := json.Unmarshal(payload["token_breakdown"], &breakdown); err != nil {
					t.Fatal(err)
				}
				if breakdown.TotalTokens != 22 || breakdown.Input.CacheWriteTokens != 10 {
					t.Fatalf("wrong accounting breakdown: %+v", breakdown)
				}
			})
		}
	})
}
