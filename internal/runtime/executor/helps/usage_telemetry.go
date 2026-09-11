package helps

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func intPtr(v int64) *int64 { return &v }
func (r *UsageReporter) initTelemetry(ctx context.Context) {
	r.telemetry = usage.Telemetry{Version: 1, AttemptID: uuid.NewString(), StartedAtMS: r.requestedAt.UnixMilli(), Transport: "unknown", ObservationKind: "attempt_only", ObservedStages: []string{"executor"}}
	if ctx != nil {
		r.telemetry.RequestID = logging.GetRequestID(ctx)
	}
}
func (r *UsageReporter) offset() int64 { return max(0, time.Since(r.requestedAt).Milliseconds()) }
func (r *UsageReporter) observeFirstBody() {
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	if r.telemetry.FirstBodyMS == nil {
		r.telemetry.FirstBodyMS = intPtr(r.offset())
		r.telemetry.ObservedStages = append(r.telemetry.ObservedStages, "first_body")
	}
}
func (r *UsageReporter) observeHeaders(resp *http.Response) {
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	r.telemetry.Transport = "http"
	if r.telemetry.ObservationKind == "attempt_only" {
		r.telemetry.ObservationKind = "http_body"
	}
	if r.telemetry.ResponseHeadersMS == nil {
		r.telemetry.ResponseHeadersMS = intPtr(r.offset())
		r.telemetry.ObservedStages = append(r.telemetry.ObservedStages, "response_headers")
	}
}
func (r *UsageReporter) traceRequest(req *http.Request) *http.Request {
	// Traces observe existing transport behavior without changing deadlines or retries.
	connectStarts := make(map[string]time.Time)
	var tlsStart time.Time
	r.telemetryMu.Lock()
	r.telemetry.Transport = "http"
	r.telemetryMu.Unlock()
	trace := &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			r.telemetryMu.Lock()
			connectStarts[network+"|"+addr] = time.Now()
			r.telemetryMu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			r.telemetryMu.Lock()
			defer r.telemetryMu.Unlock()
			connectStart := connectStarts[network+"|"+addr]
			delete(connectStarts, network+"|"+addr)
			if err == nil && !connectStart.IsZero() && r.telemetry.ConnectMS == nil {
				r.telemetry.ConnectMS = intPtr(time.Since(connectStart).Milliseconds())
				r.telemetry.ObservedStages = append(r.telemetry.ObservedStages, "connect")
			}
		},
		TLSHandshakeStart: func() { r.telemetryMu.Lock(); tlsStart = time.Now(); r.telemetryMu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			r.telemetryMu.Lock()
			defer r.telemetryMu.Unlock()
			if err == nil && !tlsStart.IsZero() && r.telemetry.TLSMS == nil {
				r.telemetry.TLSMS = intPtr(time.Since(tlsStart).Milliseconds())
				r.telemetry.ObservedStages = append(r.telemetry.ObservedStages, "tls")
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}
func (r *UsageReporter) SetWebsocketTelemetry() {
	if r == nil {
		return
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	r.telemetry.Transport = "websocket"
	r.telemetry.ObservationKind = "protocol_content_events"
}

// UseDecodedContentTelemetry selects executor protocol-line observation before any
// HTTP reads. Raw transport reads still measure first-body latency, but must not
// parse encoded bytes or pre-read terminal events before the executor handles them.
func (r *UsageReporter) UseDecodedContentTelemetry() {
	if r == nil {
		return
	}
	r.decodedContentTelemetry = true
}

func (r *UsageReporter) bodyObserver(resp *http.Response) func([]byte) {
	if r.decodedContentTelemetry {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return nil
	}
	r.telemetryMu.Lock()
	r.telemetry.ObservationKind = "http_sse_unclassified"
	r.telemetryMu.Unlock()
	// Never retain more than 64 KiB of a line. Oversized events disable content
	// observations so partial samples cannot masquerade as complete coverage.
	const limit = 64 * 1024
	var line []byte
	disabled := false
	return func(p []byte) {
		if disabled {
			return
		}
		for len(p) > 0 {
			i := bytes.IndexByte(p, '\n')
			part := p
			if i >= 0 {
				part = p[:i]
			}
			if len(line)+len(part) > limit {
				disabled = true
				line = nil
				r.telemetryMu.Lock()
				r.telemetry.ObservationKind = "content_observation_truncated"
				r.telemetryMu.Unlock()
				return
			}
			line = append(line, part...)
			if i < 0 {
				return
			}
			event := bytes.TrimSpace(line)
			if bytes.HasPrefix(event, []byte("data:")) {
				r.ObserveContentEvent(bytes.TrimSpace(event[5:]))
			}
			line = line[:0]
			p = p[i+1:]
		}
	}
}

// ObserveContentEvent inspects a protocol event without retaining its payload.
// Text, reasoning and tool argument deltas count; heartbeat/role/usage do not.
func (r *UsageReporter) ObserveContentEvent(payload []byte) {
	if r == nil {
		return
	}
	r.observeFirstBody()
	done := bytes.Equal(payload, []byte("[DONE]"))
	content := false
	recognized := done
	finish := ""
	if !done {
		if !gjson.ValidBytes(payload) {
			return
		}
		root := gjson.ParseBytes(payload)
		kind := root.Get("type").String()
		switch kind {
		case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
			recognized = true
			content = root.Get("delta").String() != ""
		case "content_block_delta":
			recognized = true
			for _, p := range []string{"delta.text", "delta.thinking", "delta.partial_json"} {
				content = content || root.Get(p).String() != ""
			}
		case "response.completed", "response.done", "message_stop":
			recognized = true
			done = true
			finish = "completed"
		case "response.failed":
			finish = "failed"
		case "response.incomplete":
			finish = "incomplete"
		}
		if root.Get("choices").IsArray() {
			recognized = true
		}
		for _, choice := range root.Get("choices").Array() {
			for _, p := range []string{"delta.content", "delta.reasoning_content", "delta.reasoning"} {
				content = content || choice.Get(p).String() != ""
			}
			for _, tool := range choice.Get("delta.tool_calls").Array() {
				content = content || tool.Get("function.arguments").String() != ""
			}
			if reason := choice.Get("finish_reason").String(); reason != "" {
				done = true
				finish = "completed"
			}
		}
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	t := &r.telemetry
	if recognized && t.ObservationKind != "content_observation_truncated" {
		t.ObservationKind = "protocol_content_events"
	}
	if done || finish != "" {
		completed := done
		t.StreamCompleted = &completed
		t.FinishReason = finish
		if t.FinishReason == "" {
			t.FinishReason = "completed"
		}
	}
	if !content {
		return
	}
	now := r.offset()
	if t.ContentChunks == nil {
		t.ContentChunks = intPtr(0)
		t.StallCount = intPtr(0)
		t.StallDurationMS = intPtr(0)
		t.StallThresholdMS = intPtr(1000)
		t.MaxContentGapMS = intPtr(0)
		t.FirstContentMS = intPtr(now)
		t.ObservedStages = append(t.ObservedStages, "content_events")
	}
	if t.LastContentMS != nil {
		gap := max(0, now-*t.LastContentMS)
		t.MaxContentGapMS = intPtr(max(*t.MaxContentGapMS, gap))
		if gap >= 1000 {
			*t.StallCount++
			*t.StallDurationMS += gap
		}
	}
	*t.ContentChunks++
	t.LastContentMS = intPtr(now)
}
func (r *UsageReporter) observeFailure(errs ...error) {
	if r == nil {
		return
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	for _, err := range errs {
		if err == nil {
			continue
		}
		var ne net.Error
		switch {
		case errors.Is(err, context.Canceled):
			r.telemetry.FailureKind = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			r.telemetry.FailureKind = "timeout"
		case errors.As(err, &ne) && ne.Timeout():
			r.telemetry.FailureKind = "timeout"
		}
		break
	}
}
func (r *UsageReporter) telemetrySnapshot(failed bool, fail usage.Failure) *usage.Telemetry {
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	t := r.telemetry
	t.EndedAtMS = time.Now().UnixMilli()
	t.ObservedStages = append([]string(nil), t.ObservedStages...)
	// JSON round trips are unnecessary: numeric pointers are copied below so later
	// stream observations cannot mutate an already-published accounting record.
	for _, p := range []**int64{&t.ConnectMS, &t.TLSMS, &t.ResponseHeadersMS, &t.FirstBodyMS, &t.FirstContentMS, &t.LastContentMS, &t.ContentChunks, &t.MaxContentGapMS, &t.StallCount, &t.StallDurationMS, &t.StallThresholdMS} {
		if *p != nil {
			*p = intPtr(**p)
		}
	}
	if t.StreamCompleted != nil {
		v := *t.StreamCompleted
		t.StreamCompleted = &v
	}
	if failed {
		completed := false
		t.StreamCompleted = &completed
		t.FinishReason = "failed"
	}
	if failed && t.FailureKind == "" {
		switch {
		case fail.StatusCode == 429:
			t.FailureKind = "rate_limited"
		case fail.StatusCode >= 500:
			t.FailureKind = "upstream_error"
		case fail.StatusCode >= 400:
			t.FailureKind = "http_error"
		default:
			t.FailureKind = "unknown"
		}
	}
	return &t
}
