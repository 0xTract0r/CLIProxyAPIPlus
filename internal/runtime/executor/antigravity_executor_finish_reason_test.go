package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityStreamDoesNotFinalizeEmptyUpstreamStream(t *testing.T) {
	for _, tc := range []struct {
		format translation.Format
		input  string
	}{
		{translation.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		{translation.FormatOpenAI, `{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`},
	} {
		for _, wire := range []string{"", "data: [DONE]\n\n"} {
			t.Run(tc.format.String()+"/"+wire, func(t *testing.T) {
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(wire)), Request: req}, nil
				}))
				exec := NewAntigravityExecutor(&config.Config{})
				req := executorapi.Request{Model: "gemini-3.7-flash", Payload: []byte(tc.input)}
				stream, err := exec.ExecuteStream(ctx, ordinaryPortAuth(), req, executorapi.Options{SourceFormat: tc.format, ResponseFormat: tc.format})
				if err != nil {
					t.Fatal(err)
				}
				var terminal error
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						terminal = chunk.Err
					}
					if len(chunk.Payload) > 0 && (ojson.GetBytesNoCopy(chunk.Payload, "candidates.0.finishReason").String() != "" ||
						ojson.GetBytesNoCopy(chunk.Payload, "choices.0.finish_reason").String() != "" ||
						strings.Contains(string(chunk.Payload), "[DONE]")) {
						t.Fatalf("empty response synthesized a completion: %s", chunk.Payload)
					}
				}
				var status statusErr
				if !errors.As(terminal, &status) || status.StatusCode() != http.StatusBadGateway {
					t.Fatalf("empty response did not fail as incomplete: %v", terminal)
				}
			})
		}
	}
}

func TestAntigravityStreamWithoutAnyProviderTerminalDoesNotFinalizeTools(t *testing.T) {
	const wire = "data: " + `{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"fixture"},"id":"call_fixture"}}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}}` + "\n\n"
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(wire)), Request: req}, nil
	}))
	exec := NewAntigravityExecutor(&config.Config{})
	req := executorapi.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`)}
	stream, err := exec.ExecuteStream(ctx, ordinaryPortAuth(), req, executorapi.Options{SourceFormat: translation.FormatOpenAI, ResponseFormat: translation.FormatOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	var terminal error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
		if ojson.GetBytesNoCopy(chunk.Payload, "choices.0.finish_reason").String() == "tool_calls" {
			t.Fatal("incomplete upstream tool output was marked ready")
		}
	}
	var status statusErr
	if !errors.As(terminal, &status) || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("missing explicit terminal marker did not fail closed: %v", terminal)
	}
}
