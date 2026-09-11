package usage

import (
	"context"
	"encoding/json"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"testing"
)

func TestTelemetrySnapshotExportRoundTrip(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{APIKey: "test", Model: "model", Alias: "requested", Telemetry: &coreusage.Telemetry{Version: 1, AttemptID: "attempt-test"}})
	raw, err := json.Marshal(stats.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var decoded StatisticsSnapshot
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	details := decoded.APIs["test"].Models["model"].Details
	if len(details) != 1 || details[0].RequestedModel != "requested" || details[0].ResolvedModel != "model" || details[0].Telemetry == nil || details[0].Telemetry.AttemptID != "attempt-test" {
		t.Fatal("export lost telemetry")
	}
}
