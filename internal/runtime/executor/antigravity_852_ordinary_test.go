package executor

import (
	"context"
	"errors"
	"github.com/MoeForge/moego/ojson"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravity852InBandErrors(t *testing.T) {
	for _, route := range []string{"generate", "count", "stream", "empty terminal"} {
		t.Run(route, func(t *testing.T) {
			calls := 0
			rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"error":{"code":8,"status":"RESOURCE_EXHAUSTED","message":"fixture capacity"},"totalTokens":1}`
				if route == "stream" || route == "empty terminal" {
					body = "data: " + body + "\n\n"
					if route == "empty terminal" {
						body = "data: {\"response\":{\"candidates\":[{\"finishReason\":null}]}}\n\n"
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": {"13"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
			req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
			exec := NewAntigravityExecutor(&config.Config{})
			var err error
			switch route {
			case "generate":
				_, err = exec.Execute(ctx, auth, req, opts)
			case "count":
				_, err = exec.CountTokens(ctx, auth, req, opts)
			default:
				var stream *cliproxyexecutor.StreamResult
				stream, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			var actual statusErr
			if calls != 1 || !errors.As(err, &actual) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if route == "empty terminal" {
				if actual.code != 502 {
					t.Fatalf("status=%d", actual.code)
				}
			} else if actual.code != 429 || actual.msg != "fixture capacity" || actual.retryAfter == nil || *actual.retryAfter != 13*time.Second {
				t.Fatalf("error=%+v", actual)
			}
		})
	}
}

func TestOrdinaryAntigravity852FilterSurvivesMetadata(t *testing.T) {
	const meta = `{"response":{"candidates":[{"finishReason":"BLOCKLIST","content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":""}}]}}],"usageMetadata":{"promptTokenCount":2}}}`
	for _, tail := range []string{meta, `{"response":{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"real reply"}]}}]}}`} {
		actualContent := tail != meta
		transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
			body := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"` + clienterror.GeminiFilterBlockMessagePrefix + " fixture " + clienterror.GeminiFilterBlockPolicyURL + `"}]}}]}}` + "\n\ndata: " + tail + "\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
		req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
		out, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FromString("antigravity")})
		if err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		for chunk := range out.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
			body.Write(chunk.Payload)
		}
		if actualContent {
			if err != nil || !strings.Contains(body.String(), "real reply") {
				t.Fatalf("output=%s err=%v", body.String(), err)
			}
			continue
		}
		var status statusErr
		if !errors.As(err, &status) || status.code != 400 || !status.requestScoped {
			t.Fatalf("want request-scoped filter error, got %v", err)
		}
	}
}

func TestOrdinaryAntigravity852CapacityRetryInfo(t *testing.T) {
	calls := 0
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"error":{"code":503,"status":"UNAVAILABLE","message":"fixture capacity","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17s"}]}}`
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	var status statusErr
	if calls != 1 || !errors.As(err, &status) || status.code != 503 || status.retryAfter == nil || *status.retryAfter != 17*time.Second {
		t.Fatalf("calls=%d status=%#v error=%v", calls, status, err)
	}
}

func TestOrdinaryAntigravity852OpenAIToolContinuationAndDone(t *testing.T) {
	calls := 0
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		frames := []string{
			`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_fixture","name":"read_value","willContinue":true}}]}}]}}`,
			`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_fixture","args":{"value":1}}}]}}]}}`,
			"[DONE]",
		}
		body := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"read_value","parameters":{"type":"object"}}}]}`)}
	out, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	var args strings.Builder
	var ids, finishes int
	for chunk := range out.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		event := ojson.ParseBytes(chunk.Payload)
		call := event.Get("choices.0.delta.tool_calls.0")
		args.WriteString(call.Get("function.arguments").String())
		if id := call.Get("id").String(); id != "" {
			if id != "call_fixture" || call.Get("function.name").String() != "read_value" {
				t.Fatalf("unexpected tool event: %s", chunk.Payload)
			}
			ids++
		}
		if event.Get("choices.0.finish_reason").String() == "tool_calls" {
			finishes++
		}
	}
	if calls != 1 || ids != 1 || finishes != 1 || args.String() != `{"value":1}` {
		t.Fatalf("calls=%d ids=%d finish=%d args=%s", calls, ids, finishes, args.String())
	}
}

func TestOrdinaryAntigravity852CountUsesSelectedModel(t *testing.T) {
	calls := 0
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := ojson.GetBytes(b, "request.model").String(); got != "models/gemini-3.7-flash" {
			t.Fatalf("request.model=%s", got)
		}
		for _, field := range []string{"project", "model", "request.stream"} {
			if ojson.GetBytes(b, field).Exists() {
				t.Fatalf("unexpected %s", field)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"totalTokens":12}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"model":"caller-metadata","stream":true}`)}
	resp, err := NewAntigravityExecutor(&config.Config{}).CountTokens(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil || calls != 1 || ojson.GetBytes(resp.Payload, "totalTokens").Int() != 12 {
		t.Fatalf("calls=%d body=%s err=%v", calls, resp.Payload, err)
	}
}

func TestOrdinaryAntigravity852ClaudeToolArgumentsStayOneObject(t *testing.T) {
	calls := 0
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		frames := []string{
			`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_fixture","name":"read_value","args":{"first":1}}}]}}]}}`,
			`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_fixture","args":{"next":2}}}]}}]}}`,
			`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}`,
			"[DONE]",
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + strings.Join(frames, "\n\ndata: ") + "\n\n")), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
	out, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatal(err)
	}
	var args strings.Builder
	var starts, terminals int
	for chunk := range out.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		for _, line := range strings.Split(string(chunk.Payload), "\n") {
			raw, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			item := ojson.Parse(strings.TrimSpace(raw))
			args.WriteString(item.Get("delta.partial_json").String())
			if item.Get("content_block.type").String() == "tool_use" {
				starts++
			}
			if item.Get("type").String() == "message_stop" {
				terminals++
			}
		}
	}
	if calls != 1 || starts != 1 || terminals != 1 || args.String() != `{"first":1,"next":2}` {
		t.Fatalf("calls=%d starts=%d stops=%d args=%s", calls, starts, terminals, args.String())
	}
}

func TestOrdinaryAntigravity852NonStreamKeepsGrounding(t *testing.T) {
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		body := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["fixture query"],"groundingChunks":[{"web":{"uri":"https://source.invalid","title":"fixture source"}}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini}
	out, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	body := ojson.ParseBytes(out.Payload)
	if body.Get("candidates.0.content.parts.0.text").String() != "answer" || body.Get("candidates.0.groundingMetadata.groundingChunks.0.web.title").String() != "fixture source" || body.Get("candidates.0.groundingMetadata.webSearchQueries.0").String() != "fixture query" {
		t.Fatalf("response=%s", out.Payload)
	}
}

func TestOrdinaryAntigravity852NormalizesExplicitThinkingOnSelectedModel(t *testing.T) {
	transport := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		doc := ojson.ParseBytes(b)
		if doc.Get("model").String() != "gemini-3.7-flash" || doc.Get("request.generationConfig.thinkingConfig.thinkingLevel").String() != "HIGH" || doc.Get("request.generationConfig.thinking_config").Exists() {
			t.Fatalf("unexpected thinking request: %s", b)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"thinking_config":{"thinking_level":"high","include_thoughts":true}}}`)}
	if _, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryAntigravity852PreservesNoToolChoiceOnClaude(t *testing.T) {
	calls := 0
	rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := ojson.ParseBytes(raw)
		if got := body.Get("request.toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
			t.Fatalf("mode=%q", got)
		}
		if got := body.Get("project").String(); got != "configured-project" {
			t.Fatalf("project=%q", got)
		}
		if got := body.Get("model").String(); got != "claude-sonnet-4-6" {
			t.Fatalf("model=%q", got)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "configured-project"}}
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"max_tokens":32,"tools":[{"name":"fixture_tool","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"},"messages":[{"role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	exec := NewAntigravityExecutor(&config.Config{})
	if _, err := exec.Execute(ctx, auth, req, opts); err != nil || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	delete(auth.Metadata, "project_id")
	_, err := exec.Execute(ctx, auth, req, opts)
	var status statusErr
	if !errors.As(err, &status) || status.code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("missing project calls=%d error=%v", calls, err)
	}
}
