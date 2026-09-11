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
