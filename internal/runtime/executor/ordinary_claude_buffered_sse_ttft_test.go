package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func TestOrdinaryClaudeBufferedSSETTFTPrecedesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, start   string
		startSetsTTFT bool
	}{
		{name: "delta only"},
		{name: "text string", start: `{"type":"text","text":"hel"}`, startSetsTTFT: true},
		{name: "empty text", start: `{"type":"text","text":""}`},
		{name: "numeric text", start: `{"type":"text","text":7}`},
		{name: "object text", start: `{"type":"text","text":{"value":"not text"}}`},
		{name: "text block ignores thinking field", start: `{"type":"text","thinking":"wrong field"}`},
		{name: "thinking string", start: `{"type":"thinking","thinking":"initial"}`, startSetsTTFT: true},
		{name: "boolean thinking", start: `{"type":"thinking","thinking":true}`},
		{name: "thinking block ignores text field", start: `{"type":"thinking","text":"wrong field"}`},
		{name: "nonempty tool input", start: `{"type":"tool_use","input":{"query":"fixture"}}`, startSetsTTFT: true},
		{name: "empty tool input", start: `{"type":"tool_use","input":{}}`},
		{name: "array tool input", start: `{"type":"tool_use","input":[{"query":"fixture"}]}`},
		{name: "tool block ignores text field", start: `{"type":"tool_use","text":"wrong field"}`},
		{name: "unknown block ignores text fields", start: `{"type":"other","text":"wrong field","thinking":"wrong field"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reporter := helps.NewUsageReporter(t.Context(), "claude", "fixture", nil)
				reporter.UseDecodedContentTelemetry()
				r, w := io.Pipe()
				defer r.Close()
				defer w.Close()
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: r}
				reporter.ObserveResponse(response)
				done := make(chan error, 1)
				go func() {
					_, err := readOrdinaryClaudeResponse(response, 64<<10, reporter.ContentEventObserver())
					done <- err
				}()
				write := func(data string) {
					t.Helper()
					if _, err := io.WriteString(w, "data: "+data+"\n\n"); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
				}
				write(`{"type":"message_start"}`)
				if reporter.IsTTFTSet() {
					t.Fatal("metadata fixed TTFT")
				}
				time.Sleep(time.Second)
				if tc.start != "" {
					write(`{"type":"content_block_start","index":0,"content_block":` + tc.start + `}`)
					if got := reporter.IsTTFTSet(); got != tc.startSetsTTFT {
						t.Fatalf("content block start set TTFT=%t, want %t", got, tc.startSetsTTFT)
					}
					write(`{"type":"content_block_stop","index":0}`)
				}
				time.Sleep(time.Second)
				if tc.start != "" {
					write(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
				}
				write(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`)
				if !reporter.IsTTFTSet() {
					t.Fatal("visible content did not set TTFT before termination")
				}
				select {
				case <-done:
					t.Fatal("buffered read returned before termination")
				default:
				}
				time.Sleep(time.Second)
				write(`{"type":"message_stop"}`)
				w.Close()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestOrdinaryClaudeNearSSEMIMEUsesFirstByte(t *testing.T) {
	reporter := helps.NewUsageReporter(t.Context(), "claude", "fixture", nil)
	reporter.UseDecodedContentTelemetry()
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-streamx"}},
		Body:       io.NopCloser(strings.NewReader(`{"content":[]}`)),
	}
	reporter.ObserveResponse(response)
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if !reporter.IsTTFTSet() {
		t.Fatal("non-SSE body did not retain first-byte tracking")
	}
}
