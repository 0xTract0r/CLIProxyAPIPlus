package auth

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOrdinaryHistoricalQuotaDataRetainsUnknownAndUsage(t *testing.T) {
	var a Auth
	if _, ok := ParseAccountQuotaUtilization(&a); ok {
		t.Fatal("missing snapshot was interpreted as fresh usage")
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	window := map[string]any{"utilization": json.Number("25")}
	a.Metadata = map[string]any{
		"quota_snapshot": map[string]any{"usage": map[string]any{"five_hour": window}},
		"account_scheduling": map[string]any{"unknown_future_field": "keep"},
	}
	UpdateAccountBurnObservability(&a, &a, t0)
	if first := ReadAccountBurnState(&a); !first.HasPrev || first.HasBurnRate {
		t.Fatalf("unexpected single observation: %+v", first)
	}
	window["utilization"] = json.Number("35")
	UpdateAccountBurnObservability(&a, &a, t0.Add(time.Hour))
	second := ReadAccountBurnState(&a)
	if !second.HasBurnRate || second.BurnRatePerHour != 10 || accountSchedulingString(a.Metadata, "unknown_future_field") != "keep" {
		t.Fatalf("unexpected historical observation: %+v", second)
	}
}
