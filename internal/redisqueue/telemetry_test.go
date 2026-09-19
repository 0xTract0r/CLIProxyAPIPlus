package redisqueue

import (
	"context"
	"encoding/json"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"testing"
)

func TestTelemetryUsageEventOptionalContract(t *testing.T) {
	withEnabledQueue(t, func() {
		p := &usageQueuePlugin{}
		p.HandleUsage(context.Background(), coreusage.Record{Model: "test"})
		requireMissingField(t, popSinglePayload(t), "telemetry")
		p.HandleUsage(context.Background(), coreusage.Record{Model: "test", Telemetry: &coreusage.Telemetry{Version: 1, AttemptID: "attempt-test", Transport: "unknown", ObservationKind: "attempt_only"}})
		v := popSinglePayload(t)
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(v["telemetry"], &nested); err != nil {
			t.Fatalf("missing telemetry: %+v", v)
		}
		requireStringField(t, nested, "attempt_id", "attempt-test")
		requireMissingField(t, nested, "first_content_ms")
	})
}

func TestFastImpactRealtimeTelemetryContract(t *testing.T) {
	withEnabledQueue(t, func() {
		enabled := true
		p := &usageQueuePlugin{}
		p.HandleUsage(context.Background(), coreusage.Record{Model: "gpt-test", ServiceTier: "default", Telemetry: &coreusage.Telemetry{Version: 2, AttemptID: "attempt-fast", FastContext: &coreusage.FastContext{SchemaVersion: 1, UpstreamRequestServiceTier: "priority", ServerFastEnabled: &enabled, TierSource: "account", RequestKind: "serving"}}})
		v := popSinglePayload(t)
		var telemetry coreusage.Telemetry
		if err := json.Unmarshal(v["telemetry"], &telemetry); err != nil {
			t.Fatal(err)
		}
		requireStringField(t, v, "service_tier", "default")
		if telemetry.Version != 2 || telemetry.FastContext == nil || telemetry.FastContext.UpstreamRequestServiceTier != "priority" || !*telemetry.FastContext.ServerFastEnabled {
			t.Fatalf("realtime=%+v", telemetry)
		}
	})
}
