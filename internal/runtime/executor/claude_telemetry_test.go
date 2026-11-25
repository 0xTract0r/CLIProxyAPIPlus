package executor

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type captureStreamTelemetryUsage struct {
	model   string
	records chan usage.Record
}

func (p *captureStreamTelemetryUsage) HandleUsage(_ context.Context, r usage.Record) {
	if r.Model == p.model {
		select {
		case p.records <- r:
		default:
		}
	}
}

func TestClaudeTelemetryTerminalBoundaryAndDecodedContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gzip    bool
		outcome string
	}{
		{"split", false, "stop"}, {"gzip_split", true, "stop"}, {"eof", false, "eof"}, {"cancel", false, "cancel"}, {"read_error", false, "read_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "claude-telemetry-" + tc.name
			plugin := &captureStreamTelemetryUsage{model: model, records: make(chan usage.Record, 4)}
			usage.RegisterPlugin(plugin)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			reader, writerPipe := io.Pipe()
			defer reader.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				headers := http.Header{"Content-Type": {"text/event-stream"}}
				if tc.gzip {
					headers.Set("Content-Encoding", "gzip")
				}
				return &http.Response{StatusCode: 200, Header: headers, Body: reader, Request: r}, nil
			})
			go func() {
				defer writerPipe.Close()
				go func() { <-ctx.Done(); _ = writerPipe.CloseWithError(ctx.Err()) }()
				var writer io.Writer = writerPipe
				var compressed *gzip.Writer
				if tc.gzip {
					compressed = gzip.NewWriter(writerPipe)
					writer = compressed
					defer compressed.Close()
				}

				_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n"+
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"b\"}}\n\n"+
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":6}}\n\n")
				if compressed != nil {
					_ = compressed.Flush()
				}
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
				if tc.outcome == "read_error" {
					_ = writerPipe.CloseWithError(io.ErrUnexpectedEOF)
				}
				if tc.outcome == "stop" {
					_, _ = io.WriteString(writer, "data: {\"type\":\"message_stop\"}\n\n")
				}
			}()
			streamCtx := context.WithValue(ctx, "cliproxy.roundtripper", rt)
			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "telemetry-test-key", "base_url": "https://fixture.invalid"}}
			result, err := executor.ExecuteStream(streamCtx, auth, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
			if err != nil {
				t.Fatal(err)
			}
			seenUsage := make(chan struct{})
			drained := make(chan struct{})
			var seenOnce sync.Once
			var streamErr error
			var visible int
			go func() {
				defer close(drained)
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
					if strings.Contains(string(chunk.Payload), "text_delta") {
						visible++
					}
					if strings.Contains(string(chunk.Payload), "message_delta") {
						seenOnce.Do(func() { close(seenUsage) })
					}
				}
			}()
			select {
			case <-seenUsage:
			case <-time.After(3 * time.Second):
				unblock()
				t.Fatal("message_delta not forwarded before terminal")
			}
			select {
			case r := <-plugin.records:
				unblock()
				t.Fatalf("usage published before terminal: %+v", r)
			case <-time.After(30 * time.Millisecond):
			}
			if tc.outcome == "cancel" {
				cancel()
			} else {
				unblock()
			}
			select {
			case <-drained:
			case <-time.After(3 * time.Second):
				unblock()
				t.Fatal("stream did not end")
			}
			if visible != 2 || (streamErr == nil) != (tc.outcome == "stop") || (tc.outcome == "cancel" && !errors.Is(streamErr, context.Canceled)) {
				t.Fatalf("content frames=%d, terminal error=%v", visible, streamErr)
			}
			var record usage.Record
			select {
			case record = <-plugin.records:
			case <-time.After(3 * time.Second):
				t.Fatal("missing terminal usage")
			}
			telemetry := record.Telemetry
			if record.TTFT <= 0 || record.Detail.OutputTokens != 6 || record.Detail.InputTokens != 4 {
				t.Fatalf("TTFT=%s, input=%d, output=%d", record.TTFT, record.Detail.InputTokens, record.Detail.OutputTokens)
			}
			if telemetry == nil || telemetry.ContentChunks == nil || *telemetry.ContentChunks != 2 || telemetry.FirstVisibleContentMS == nil || telemetry.VisibleContentEvents == nil || *telemetry.VisibleContentEvents != 2 {
				t.Fatalf("decoded content counted incorrectly: %+v", telemetry)
			}
			wantSuccess := tc.outcome == "stop"
			if telemetry.StreamCompleted == nil || *telemetry.StreamCompleted != wantSuccess || record.Failed == wantSuccess {
				t.Fatalf("wrong terminal outcome: failed=%v telemetry=%+v", record.Failed, telemetry)
			}
			if tc.outcome == "cancel" && telemetry.FailureKind != "cancelled" {
				t.Fatalf("cancel classification: %s", telemetry.FailureKind)
			}
			select {
			case extra := <-plugin.records:
				t.Fatalf("double accounting: %+v", extra)
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestCodexWebsocketTelemetryResponseDone(t *testing.T) {
	model := "codex-telemetry-response-done"
	plugin := &captureStreamTelemetryUsage{model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"x"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"telemetry-test","status":"completed","service_tier":"default","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	}))
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(t.Context())
	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"service_tier":"priority","input":[{"role":"user","content":"hi"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err != nil {
		t.Fatal(err)
	}
	var visible, finished bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		visible = visible || strings.Contains(string(chunk.Payload), "response.output_text.delta")
		finished = finished || strings.Contains(string(chunk.Payload), "response.done")
	}
	if !visible || !finished {
		t.Fatalf("visible=%t terminal=%t", visible, finished)
	}
	select {
	case record := <-plugin.records:
		if record.EffectiveServiceTier != "priority" || record.ResponseServiceTier != "default" {
			t.Fatalf("service tiers = %q / %q, want priority / default", record.EffectiveServiceTier, record.ResponseServiceTier)
		}
		v := record.Telemetry
		if v == nil || v.StreamCompleted == nil || !*v.StreamCompleted || v.Transport != "websocket" || record.TTFT <= 0 || v.FirstVisibleContentMS == nil || v.VisibleContentEvents == nil || *v.VisibleContentEvents != 1 || record.Detail.OutputTokens != 2 {
			t.Fatalf("wrong WS terminal usage: telemetry=%+v output=%d ttft=%s", v, record.Detail.OutputTokens, record.TTFT)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing WS usage")
	}
}
