package management

import (
	"encoding/json"
	"testing"
)

func TestOrdinaryStoredQuotaRetainsProviderObservations(t *testing.T) {
	for _, tc := range []struct{ name, provider, usage, want string }{
		{"reported allow at 100 percent", "codex", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":100}}}`, `{"usage":{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":100}}}}`},
		{"reported refusal", "codex", `{"rate_limit":{"allowed":false,"limit_reached":true,"secondary_window":{"used_percent":40}}}`, `{"usage":{"rate_limit":{"allowed":false,"limit_reached":true,"secondary_window":{"used_percent":40}}}}`},
		{"missing flags stay absent", "codex", `{"rate_limit":{"primary_window":{"used_percent":100}}}`, `{"usage":{"rate_limit":{"primary_window":{"used_percent":100}}}}`},
		{"shared and model windows", "claude", `{"five_hour":{"utilization":100},"seven_day_opus":{"utilization":20},"private_key":"discard"}`, `{"usage":{"five_hour":{"utilization":100},"seven_day_opus":{"utilization":20}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in map[string]any
			if err := json.Unmarshal([]byte(tc.usage), &in); err != nil {
				t.Fatal(err)
			}
			got := storedQuotaDisplaySnapshot(tc.provider, map[string]any{"usage": in})
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			expect, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != string(expect) {
				t.Fatalf("unexpected stored quota projection: %s", b)
			}
		})
	}
}
