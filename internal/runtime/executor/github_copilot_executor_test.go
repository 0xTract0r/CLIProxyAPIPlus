package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCopilotCatalogRoutesAndPayload(t *testing.T) {
	const model = "claude-opus-5"
	for _, tc := range []struct {
		name, endpoint string
		format         tr.Format
		request        string
		fields         map[string]string
		contains       []string
	}{
		{"chat function and image", copilot.Chat, tr.FormatOpenAI,
			`{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}],"tools":[{"type":"function","function":{"name":"describe","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"describe"}}}`,
			map[string]string{"tools.0.function.name": "describe", "tools.0.function.parameters.type": "object", "tool_choice.function.name": "describe"}, []string{"describe", "data:image/png;base64,YQ=="}},
		{"responses from Claude tools and image", copilot.Responses, tr.FormatClaude,
			`{"max_tokens":32,"system":"system text","messages":[{"role":"user","content":[{"type":"text","text":"user text"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}]},{"role":"assistant","content":[{"type":"text","text":"assistant text"}]}],"tools":[{"name":"describe","description":"Describe image","input_schema":{"type":"object","properties":{"size":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"describe"}}`,
			map[string]string{"tools.0.name": "describe", "tools.0.parameters.properties.size.type": "string", "tool_choice.name": "describe"}, []string{"system text", "user text", "assistant text", "data:image/png;base64,YQ=="}},
		{"responses input and typed choice", copilot.Responses, tr.FormatOpenAIResponse,
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"just text"}]}],"tools":[{"type":"function","name":"describe","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"describe"}}`,
			map[string]string{"input.0.content.0.text": "just text", "tools.0.name": "describe", "tool_choice.name": "describe"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := copilotOrdinaryFixtureAuth(t, t.Name(), model, tc.endpoint)
			e := NewCopilotExecutor(nil)
			req := executorapi.Request{Model: copilot.ModelPrefix + model, Payload: []byte(tc.request)}
			opts := e.PlanRequestRoute(auth, req, executorapi.Options{SourceFormat: tc.format})
			prepared, err := e.prepare(t.Context(), auth, req, opts, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.route.Endpoint != tc.endpoint || ojson.GetBytes(prepared.body, "model").String() != model {
				t.Fatalf("unexpected route/model: %q, %s", prepared.route.Endpoint, prepared.body)
			}
			for field, want := range tc.fields {
				if got := ojson.GetBytes(prepared.body, field).String(); got != want {
					t.Errorf("%s=%q want %q", field, got, want)
				}
			}
			for _, fragment := range tc.contains {
				if !strings.Contains(string(prepared.body), fragment) {
					t.Errorf("missing %q in %s", fragment, prepared.body)
				}
			}
		})
	}
}

func TestOrdinaryCopilotResponsesToMessages(t *testing.T) {
	const model = "claude-opus-5"
	opts := executorapi.Options{SourceFormat: tr.FormatClaude}
	req := executorapi.Request{Model: model, Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)}
	for _, tc := range []struct {
		name, wire string
		stream     bool
		fields     map[string]string
		fragments  []string
	}{
		{"text", `{"id":"resp_1","object":"response","model":"claude-opus-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":5}}`, false,
			map[string]string{"type": "message", "content.0.type": "text", "content.0.text": "hello", "usage.input_tokens": "3", "usage.output_tokens": "5"}, nil},
		{"tool", `{"id":"resp_2","object":"response","model":"claude-opus-5","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"sum","arguments":"{\"a\":1}"}],"usage":{"input_tokens":1,"output_tokens":2}}`, false,
			map[string]string{"content.0.type": "tool_use", "content.0.name": "sum", "content.0.input.a": "1", "stop_reason": "tool_use"}, nil},
		{"text stream", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"claude-opus-5\",\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n", true, nil,
			[]string{"message_start", "content_block_start", "text_delta", "hello", "message_delta", "message_stop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := copilotOrdinaryFixtureAuth(t, t.Name(), model, copilot.Responses)
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != copilot.Responses || r.Header.Get("Authorization") != "Bearer test-credential" || r.Method != http.MethodPost {
					t.Fatalf("unexpected request: %s", r.URL)
				}
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(tc.wire)), Request: r}, nil
			}))
			e := NewCopilotExecutor(nil)
			if tc.stream {
				result, err := e.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var out strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					out.Write(chunk.Payload)
				}
				remaining := out.String()
				for _, fragment := range tc.fragments {
					i := strings.Index(remaining, fragment)
					if i < 0 {
						t.Fatalf("missing SSE %q in %s", fragment, out.String())
					}
					remaining = remaining[i+len(fragment):]
				}
			} else {
				result, err := e.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for field, want := range tc.fields {
					if got := ojson.GetBytes(result.Payload, field).String(); got != want {
						t.Errorf("%s=%q want %q: %s", field, got, want, result.Payload)
					}
				}
			}
			if calls != 1 {
				t.Fatalf("expected exactly one request, got %d", calls)
			}
		})
	}
}
