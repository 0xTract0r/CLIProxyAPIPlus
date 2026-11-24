package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityNonStreamRetainsMetadataAndToolOutputs(t *testing.T) {
	wire := "data: " + `{"requestId":"fixture-request","response":{"modelVersion":"fixture-v1","usageMetadata":{"promptTokenCount":7},"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"first"},{"functionCall":{"name":"lookup","id":"call-a","args":{"limit":2}}}]},"safetyRatings":[{"category":"fixture-category"}]}]}}` + "\n\n" +
		"data: " + `{"response":{"usageMetadata":{"candidatesTokenCount":3,"totalTokenCount":10},"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"describe","id":"call-b","args":{"verbose":true}}}]},"groundingMetadata":{"webSearchQueries":["fixture topic"]},"finishReason":"STOP"}]}}` + "\n\n"
	testReq := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
	auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != antigravityGeneratePath {
			t.Fatal("non-streaming response selected a different upstream")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(wire)), Request: req}, nil
	}))
	exec := NewAntigravityExecutor(&config.Config{})
	resp, err := exec.Execute(ctx, auth, testReq, executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"requestId": "fixture-request", "response.modelVersion": "fixture-v1",
		"response.usageMetadata.promptTokenCount": "7", "response.usageMetadata.candidatesTokenCount": "3", "response.usageMetadata.totalTokenCount": "10",
		"response.candidates.0.safetyRatings.0.category":             "fixture-category",
		"response.candidates.0.groundingMetadata.webSearchQueries.0": "fixture topic",
		"response.candidates.0.finishReason":                         "STOP", "response.candidates.0.content.role": "model",
		"response.candidates.0.content.parts.0.text":                      "first",
		"response.candidates.0.content.parts.1.functionCall.name":         "lookup",
		"response.candidates.0.content.parts.1.functionCall.id":           "call-a",
		"response.candidates.0.content.parts.1.functionCall.args.limit":   "2",
		"response.candidates.0.content.parts.2.functionCall.name":         "describe",
		"response.candidates.0.content.parts.2.functionCall.id":           "call-b",
		"response.candidates.0.content.parts.2.functionCall.args.verbose": "true",
	} {
		if got := ojson.GetBytesNoCopy(resp.Payload, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	for _, format := range []translation.Format{translation.FormatOpenAI, translation.FormatClaude} {
		t.Run(format.String(), func(t *testing.T) {
			out, err := exec.Execute(ctx, auth, testReq, executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: format})
			if err != nil {
				t.Fatal(err)
			}
			var calls []ojson.Result
			switch format {
			case translation.FormatOpenAI:
				calls = ojson.GetBytesNoCopy(out.Payload, "choices.0.message.tool_calls").Array()
				if ojson.GetBytesNoCopy(out.Payload, "choices.0.finish_reason").String() != "tool_calls" {
					t.Errorf("OpenAI completion lost its ordinary tool termination")
				}
			case translation.FormatClaude:
				for _, item := range ojson.GetBytesNoCopy(out.Payload, "content").Array() {
					if item.Get("type").String() == "tool_use" {
						calls = append(calls, item)
					}
				}
			}
			if len(calls) != 2 {
				t.Fatalf("expected two regular %s tool calls, got %d", format, len(calls))
			}
			for i, want := range []string{"lookup", "describe"} {
				path := "name"
				if format == translation.FormatOpenAI {
					path = "function.name"
				}
				if got := calls[i].Get(path).String(); got != want {
					t.Errorf("tool %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestOrdinaryAntigravityTerminalErrorsAfterMetadata(t *testing.T) {
	const metadata = "data: {\"error\":null,\"response\":{\"modelVersion\":\"fixture-version\",\"usageMetadata\":{\"promptTokenCount\":3}}}\n\n"
	const partial = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ordinary partial\"}]}}]}}\n\n"
	const stop = "data: {\"error\":null,\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"ordinary success\"}]}}]}}\n\n"
	for _, tc := range []struct {
		name, tail string
		status     int
	}{
		{"null is harmless", stop, http.StatusOK},
		{"invalid", "data: {\"error\":{\"status\":\"INVALID_ARGUMENT\",\"message\":\"invalid input\"}}\n\n", http.StatusBadRequest},
		{"denied", "data: {\"error\":{\"status\":\"PERMISSION_DENIED\",\"message\":\"ordinary denied\"}}\n\n", http.StatusForbidden},
		{"server error", "data: {\"error\":{\"code\":500,\"message\":\"provider failure\"}}\n\n", http.StatusInternalServerError},
		{"wait", "data: {\"error\":{\"status\":\"RESOURCE_EXHAUSTED\",\"message\":\"ordinary wait\"}}\n\n", http.StatusTooManyRequests},
		{"overloaded", "data: {\"error\":{\"status\":\"UNAVAILABLE\",\"message\":\"ordinary overload\"}}\n\n", http.StatusServiceUnavailable},
	} {
		for _, stream := range []bool{false, true} {
			name := "nonstream"
			if stream {
				name = "stream"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				calls := 0
				prefix := metadata
				if tc.status == http.StatusInternalServerError {
					prefix += partial
				}
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"9"}},
						Body: io.NopCloser(strings.NewReader(metadata + tc.tail)), Request: req}, nil
				}))
				exec := NewAntigravityExecutor(&config.Config{})
				auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
				req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
				opts := executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity}
				var wire strings.Builder
				var err error
				if stream {
					var out *executorapi.StreamResult
					out, err = exec.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for c := range out.Chunks {
							if c.Err != nil {
								err = c.Err
							}
							wire.Write(c.Payload)
						}
					}
				} else {
					var out executorapi.Response
					out, err = exec.Execute(ctx, auth, req, opts)
					wire.Write(out.Payload)
				}
				if calls != 1 {
					t.Fatalf("one request expected, got %d", calls)
				}
				if tc.status == http.StatusOK {
					if err != nil || !strings.Contains(wire.String(), "ordinary success") {
						t.Fatalf("error:null suppressed a successful response: %v", err)
					}
					return
				}
				var status statusErr
				if !errors.As(err, &status) || status.StatusCode() != tc.status || strings.Contains(wire.String(), "finishReason") {
					t.Fatalf("provider error lost or turned into a completed response: %v", err)
				}
				if tc.status == http.StatusTooManyRequests || tc.status == http.StatusServiceUnavailable {
					if status.RetryAfter() == nil || *status.RetryAfter() != 9*time.Second {
						t.Fatalf("provider Retry-After lost: %v", status.RetryAfter())
					}
				}
			})
		}
	}
}

func TestOrdinaryAntigravityPayloadRulesFilterPreparedBodyOnce(t *testing.T) {
	const input = `{"request":{"systemInstruction":{"parts":[{"text":"remove this"}]},"contents":[{"role":"user","parts":[{"text":"drop first"}]},{"role":"model","parts":[{"text":"keep second"}]},{"role":"user","parts":[{"text":"keep third"}]}],"generationConfig":{"temperature":0.2}}}`
	for _, operation := range []string{"generate", "stream", "count"} {
		t.Run(operation, func(t *testing.T) {
			models := []config.PayloadModelRule{{Name: "fixture-model", Protocol: "antigravity"}}
			cfg := &config.Config{Payload: config.PayloadConfig{
				Override: []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.temperature": 0.75}}},
				Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"contents.0", "systemInstruction"}}},
			}}
			var captured []byte
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
				var err error
				captured, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var out string
				mediaType := "application/json"
				switch operation {
				case "count":
					out = `{"totalTokens":3}`
				case "stream":
					mediaType = "text/event-stream"
					out = "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n"
				default:
					out = `{"response":{"candidates":[{"finishReason":"STOP"}]}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {mediaType}}, Body: io.NopCloser(strings.NewReader(out)), Request: req}, nil
			}))
			exec := NewAntigravityExecutor(cfg)
			auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
			req := executorapi.Request{Model: "fixture-model", Payload: []byte(input)}
			opts := executorapi.Options{SourceFormat: translation.FormatAntigravity, ResponseFormat: translation.FormatAntigravity}
			switch operation {
			case "count":
				if _, err := exec.CountTokens(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			case "stream":
				result, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			default:
				if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			root := ojson.GetBytesNoCopy(captured, "request")
			if root.Get("systemInstruction").Exists() || root.Get("contents.#").Int() != 2 ||
				root.Get("contents.0.parts.0.text").String() != "keep second" || root.Get("contents.1.parts.0.text").String() != "keep third" ||
				root.Get("generationConfig.temperature").Float() != 0.75 {
				t.Fatalf("configured rules did not run exactly once on the prepared body: %s", captured)
			}
		})
	}
}

func TestOrdinaryAntigravityRetryHintsOnAllWireOperations(t *testing.T) {
	for _, tc := range []struct {
		name, header, reply string
		want                time.Duration
	}{
		{"HTTP fallback", "23", `{"error":{"message":"upstream wait"}}`, 23 * time.Second},
		{"provider body first", "23", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"11s"}]}}`, 11 * time.Second},
		{"provider human-readable duration", "", `{"error":{"message":"Your quota will reset after 1h43m56s."}}`, time.Hour + 43*time.Minute + 56*time.Second},
	} {
		for _, operation := range []string{"generate", "stream", "count"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				sends := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
					sends++
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {tc.header}},
						Body: io.NopCloser(strings.NewReader(tc.reply)), Request: req}, nil
				}))
				exec := NewAntigravityExecutor(&config.Config{})
				auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
				req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
				opts := executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity}
				var err error
				switch operation {
				case "count":
					_, err = exec.CountTokens(ctx, auth, req, opts)
				case "stream":
					_, err = exec.ExecuteStream(ctx, auth, req, opts)
				default:
					_, err = exec.Execute(ctx, auth, req, opts)
				}
				assertRetryAfterDuration(t, err, tc.want)
				if sends != 1 {
					t.Fatalf("one upstream attempt expected, got %d", sends)
				}
			})
		}
	}
}

func TestOrdinaryOpenAICompatDecoratorObservesFinalizedPayload(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "fixture-model"}},
		Params: map[string]any{"temperature": 0.25},
	}}}}
	exec := NewOpenAICompatExecutor("openai-compat", cfg)
	auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture-api-key"}}
	var observed []byte
	var status, calls int
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		response := `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	}))
	ctx = httptransport.WithDecorator(ctx, func(next http.RoundTripper) http.RoundTripper {
		return ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
			calls++
			var err error
			observed, err = io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(observed))
			resp, err := next.RoundTrip(req)
			if resp != nil {
				status = resp.StatusCode
			}
			return resp, err
		})
	})
	req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}]}`)}
	out, err := exec.Execute(ctx, auth, req, executorapi.Options{SourceFormat: translation.FormatOpenAI})
	if err != nil || !strings.Contains(string(out.Payload), `"content":"ok"`) || cfg.RequestLog || calls != 1 ||
		status != http.StatusOK || ojson.GetBytesNoCopy(observed, "model").String() != "fixture-model" ||
		ojson.GetBytesNoCopy(observed, "temperature").Float() != 0.25 {
		t.Fatalf("ordinary observation did not see the actual finalized request and response: calls=%d status=%d err=%v", calls, status, err)
	}
}

func TestOrdinaryAntigravityProviderRefusalKeepsItsExplicitScope(t *testing.T) {
	for _, tc := range []struct {
		name, upstreamStatus, reason string
		code                         int
		credential, terminal         bool
	}{
		{"expired credential", "UNAUTHENTICATED", "", http.StatusUnauthorized, true, false},
		{"explicit credential validation", "PERMISSION_DENIED", "VALIDATION_REQUIRED", http.StatusForbidden, true, false},
		{"explicit permanent service policy refusal", "PERMISSION_DENIED", "TOS_VIOLATION", http.StatusForbidden, true, true},
		{"ordinary permission denied", "PERMISSION_DENIED", "MODEL_ACCESS_DENIED", http.StatusForbidden, false, false},
		{"ordinary wait", "RESOURCE_EXHAUSTED", "", http.StatusTooManyRequests, false, false},
	} {
		for _, insideSSE := range []bool{false, true} {
			name := "HTTP"
			if insideSSE {
				name = "SSE"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				body := ojson.NewDoc([]byte(`{"error":{"message":"ordinary provider refusal"}}`))
				_ = body.Set("error.code", tc.code)
				_ = body.Set("error.status", tc.upstreamStatus)
				if tc.reason != "" {
					_ = body.SetRaw("error.details", `[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"`+tc.reason+`"}]`)
				}
				raw := string(body.Bytes())
				sends := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
					sends++
					code, mediaType := tc.code, "application/json"
					wire := raw
					if insideSSE {
						code, mediaType = http.StatusOK, "text/event-stream"
						wire = "data: " + wire + "\n\n"
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {mediaType}, "Retry-After": {"14"}},
						Body: io.NopCloser(strings.NewReader(wire)), Request: req}, nil
				}))
				exec := NewAntigravityExecutor(&config.Config{})
				auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
				req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
				opts := executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity}
				_, err := exec.Execute(ctx, auth, req, opts)
				var status statusErr
				if sends != 1 || !errors.As(err, &status) || status.StatusCode() != tc.code ||
					status.IsCredentialScoped() != tc.credential || status.IsTerminalTOSViolation() != tc.terminal {
					t.Fatalf("provider refusal scope changed: sends=%d code=%d credential=%v terminal=%v err=%v",
						sends, status.StatusCode(), status.IsCredentialScoped(), status.IsTerminalTOSViolation(), err)
				}
				if tc.code == http.StatusTooManyRequests && (status.RetryAfter() == nil || *status.RetryAfter() != 14*time.Second) {
					t.Fatalf("ordinary wait lost its retry hint: %v", status.RetryAfter())
				}
			})
		}
	}
}
