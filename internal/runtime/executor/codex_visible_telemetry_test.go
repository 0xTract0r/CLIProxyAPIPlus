package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type captureCodexVisibleUsage struct {
	model   string
	records chan usage.Record
}

func (p *captureCodexVisibleUsage) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider != "codex" || record.Model != p.model {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func TestCodexExecuteObservesVisibleSSEWhenContentTypeIsNotSSE(t *testing.T) {
	model := "codex-visible-telemetry-non-sse-header"
	plugin := &captureCodexVisibleUsage{model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The production Codex HTTP path can return SSE framing under a generic body
		// Content-Type. The executor knows the protocol and must still observe events.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"a"}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"b"}`+"\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_visible","object":"response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":200,"total_tokens":208,"output_tokens_details":{"reasoning_tokens":20}}}}`+"\n\n")
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{
		ID:       "codex-visible-telemetry",
		Provider: "codex",
		ProxyURL: "direct",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test",
		},
	}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"` + model + `","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("codex"),
		ResponseFormat: sdktranslator.FromString("codex"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case record := <-plugin.records:
		telemetry := record.Telemetry
		if telemetry == nil || telemetry.VisibleContentEvents == nil || *telemetry.VisibleContentEvents != 2 {
			t.Fatalf("visible events missing: %+v", telemetry)
		}
		if telemetry.FirstVisibleContentMS == nil || telemetry.LastVisibleContentMS == nil || *telemetry.LastVisibleContentMS-*telemetry.FirstVisibleContentMS < 10 {
			t.Fatalf("visible span missing: %+v", telemetry)
		}
		if telemetry.ObservationKind != "protocol_content_events" || telemetry.StreamCompleted == nil || !*telemetry.StreamCompleted || telemetry.FastContext == nil {
			t.Fatalf("terminal telemetry missing: %+v", telemetry)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for codex usage record")
	}
}
