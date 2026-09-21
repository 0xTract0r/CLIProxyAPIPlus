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
		{`{"service_tier":"fast"}`, `{"service_tier":"default"}`, false, "default", "default", "serving"},
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

func TestCodexVisibleCoverageCustomRefusalAndUnknownOutput(t *testing.T) {
	for _, kind := range []string{"response.custom_tool_call_input.delta", "response.refusal.delta"} {
		r := NewUsageReporter(context.Background(), "codex", "main", nil)
		r.SetCodexFastContext([]byte(`{}`), []byte(`{}`), false)
		r.ObserveContentEvent([]byte(`{"type":"response.output_text.delta","delta":"text"}`))
		r.ObserveContentEvent([]byte(`{"type":"` + kind + `","delta":"tool-or-refusal"}`))
		if *r.telemetry.VisibleContentEvents != 2 || !r.telemetry.VisibleContentObserved {
			t.Fatalf("coverage=%+v", r.telemetry)
		}
		auxiliary := r.buildRecordForModel("image-tool", usage.Detail{}, false, usage.Failure{}).Telemetry
		if auxiliary.FastContext != nil || auxiliary.VisibleContentObserved || auxiliary.FirstVisibleContentMS != nil {
			t.Fatal("auxiliary model borrowed main timing")
		}
	}
	for _, payload := range []string{`{"type":"response.unknown.delta","delta":"unclassified"}`, `{"type":"response.completed","response":{"output":[{"type":"image_generation_call"}]}}`, `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"audio"}]}]}}`} {
		r := NewUsageReporter(context.Background(), "codex", "main", nil)
		r.SetCodexFastContext([]byte(`{}`), []byte(`{}`), false)
		r.ObserveContentEvent([]byte(payload))
		if r.telemetry.VisibleContentObserved {
			t.Fatal("unknown output reported complete visibility")
		}
	}
}
