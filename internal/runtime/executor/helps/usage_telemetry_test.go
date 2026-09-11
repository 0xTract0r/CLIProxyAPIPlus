package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestTelemetryHTTPStreamingAndTLS(t *testing.T) {
	body := ": ping\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"private answer\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	r := NewUsageReporter(logging.WithRequestID(context.Background(), "request-test"), "openai", "test", nil)
	resp, err := r.TrackHTTPClient(srv.Client()).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(got) != body {
		t.Fatalf("transport changed: %q %v", got, err)
	}
	v := r.buildRecord(usage.Detail{}, false).Telemetry
	if v.Transport != "http" || v.ConnectMS == nil || v.TLSMS == nil || v.ResponseHeadersMS == nil || v.FirstBodyMS == nil {
		t.Fatalf("missing stages: %+v", v)
	}
	if v.ContentChunks == nil || *v.ContentChunks != 1 || v.StreamCompleted == nil || !*v.StreamCompleted {
		t.Fatalf("content observations: %+v", v)
	}
	if v.RequestID != "request-test" || v.EndedAtMS < v.StartedAtMS || *v.FirstContentMS < *v.FirstBodyMS {
		t.Fatalf("bad boundaries %+v", v)
	}
	encoded, _ := json.Marshal(v)
	if strings.Contains(string(encoded), "private answer") {
		t.Fatal("body leaked")
	}
}

func TestTelemetryContentStallSnapshotsAndSplitBilling(t *testing.T) {
	r := NewUsageReporter(context.Background(), "codex", "test", nil)
	r.SetWebsocketTelemetry()
	r.requestedAt = time.Now().Add(-2 * time.Second)
	r.ObserveContentEvent([]byte(`{"type":"response.output_text.delta","delta":"a"}`))
	// Set the previous event offset to exercise the 1000 ms rule without sleeping.
	r.telemetry.LastContentMS = intPtr(0)
	first := r.buildRecord(usage.Detail{}, false)
	r.ObserveContentEvent([]byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`))
	r.ObserveContentEvent([]byte(`{"type":"response.completed"}`))
	second, ok := r.buildAdditionalModelRecord("image", usage.Detail{OutputTokens: 1})
	if !ok || first.Telemetry.AttemptID != second.Telemetry.AttemptID || *first.Telemetry.ContentChunks != 1 || *second.Telemetry.ContentChunks != 2 {
		t.Fatal("split billing identity or immutable snapshot broken")
	}
	if *second.Telemetry.StallCount != 1 || *second.Telemetry.StallDurationMS < 1000 || *second.Telemetry.StallThresholdMS != 1000 {
		t.Fatalf("bad stall accounting: %+v", second.Telemetry)
	}
	other := NewUsageReporter(context.Background(), "codex", "test", nil)
	if other.telemetry.AttemptID == r.telemetry.AttemptID {
		t.Fatal("attempt identity reused")
	}
}

func TestTelemetrySSEBoundedAndUnsupported(t *testing.T) {
	r := NewUsageReporter(context.Background(), "test", "test", nil)
	observe := r.bodyObserver(&http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}})
	observe([]byte(": heartbeat\n\ndata: {\"usage\":{\"output_tokens\":9}}\n\n"))
	if r.telemetry.ContentChunks != nil || r.telemetry.ObservationKind != "http_sse_unclassified" {
		t.Fatal("metadata counted")
	}
	observe([]byte("data: " + strings.Repeat("a", 65536) + "\n"))
	observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"))
	if r.telemetry.ContentChunks != nil || r.telemetry.ObservationKind != "content_observation_truncated" {
		t.Fatal("truncated stream silently resumed")
	}
}

func TestTelemetryFailureAndOptionalFields(t *testing.T) {
	r := NewUsageReporter(context.Background(), "test", "test", nil)
	r.observeFailure(context.DeadlineExceeded)
	v := r.telemetrySnapshot(true, usage.Failure{})
	if v.FailureKind != "timeout" || v.FirstBodyMS != nil || v.StreamCompleted == nil || *v.StreamCompleted {
		t.Fatalf("incorrect failure %+v", v)
	}
	r2 := NewUsageReporter(context.Background(), "test", "test", nil)
	if r2.telemetrySnapshot(true, usage.Failure{StatusCode: 429}).FailureKind != "rate_limited" {
		t.Fatal("status classification")
	}
	raw, _ := json.Marshal(usage.Record{})
	if strings.Contains(string(raw), "telemetry") {
		t.Fatal("legacy record gains telemetry")
	}
}
