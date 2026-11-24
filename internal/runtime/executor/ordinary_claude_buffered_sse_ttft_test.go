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
			if _, err := io.WriteString(w, data); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		write("data: {\"type\":\"message_start\"}\n\n")
		if reporter.IsTTFTSet() {
			t.Fatal("metadata fixed TTFT")
		}
		time.Sleep(time.Second)
		write("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n")
		if !reporter.IsTTFTSet() {
			t.Fatal("visible content did not set TTFT before termination")
		}
		select {
		case <-done:
			t.Fatal("buffered read returned before termination")
		default:
		}
		time.Sleep(time.Second)
		write("data: {\"type\":\"message_stop\"}\n\n")
		w.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
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
