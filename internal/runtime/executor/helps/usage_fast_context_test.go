package helps

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestCodexFastContextAttributionAndVisibleEvents(t *testing.T) {
	for _, tc := range []struct {
		client, out        string
		enabled            bool
		tier, source, kind string
	}{
		{`{}`, `{}`, false, "auto", "default", "serving"},
		{`{"service_tier":"priority"}`, `{"service_tier":"priority"}`, false, "priority", "client", "serving"},
		{`{}`, `{"service_tier":"priority"}`, true, "priority", "account", "serving"},
		{`{"service_tier":"priority"}`, `{"service_tier":"priority"}`, true, "priority", "both", "serving"},
		{`{"service_tier":"flex"}`, `{"service_tier":"flex"}`, false, "flex", "client", "serving"},
		{`{}`, `{"service_tier":"unexpected"}`, false, "unexpected", "unknown", "serving"},
		{`{"generate":false}`, `{}`, false, "auto", "default", "prewarm"},
	} {
		r := NewUsageReporter(context.Background(), "codex", "gpt-test", nil)
		r.SetCodexFastContext([]byte(tc.client), []byte(tc.out), tc.enabled)
		f := r.telemetry.FastContext
		if f.UpstreamRequestServiceTier != tc.tier || f.TierSource != tc.source || f.RequestKind != tc.kind || *f.ServerFastEnabled != tc.enabled {
			t.Fatalf("provenance=%+v", f)
		}
	}
	r := NewUsageReporter(context.Background(), "codex", "gpt-test", nil)
	r.SetCodexFastContext([]byte(`{}`), []byte(`{}`), true)
	r.ObserveContentEvent([]byte(`{"type":"response.reasoning_text.delta","delta":"thinking"}`))
	if r.telemetry.FirstVisibleContentMS != nil {
		t.Fatal("reasoning counted as visible")
	}
	r.requestedAt = time.Now().Add(-time.Second)
	r.ObserveContentEvent([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	r.ObserveContentEvent([]byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`))
	r.ObserveContentEvent([]byte(`{"type":"response.completed","response":{"usage":{"output_tokens":200,"output_tokens_details":{"reasoning_tokens":20}}}}`))
	result := r.buildRecord(usage.Detail{OutputTokens: 200}, false, usage.Failure{})
	if result.Telemetry.Version != 2 || *result.Telemetry.VisibleContentEvents != 2 || *result.Telemetry.FirstVisibleContentMS < 900 || !result.Telemetry.OutputReasoningSubset || !*result.Telemetry.StreamCompleted {
		t.Fatalf("telemetry=%+v", result.Telemetry)
	}
}
