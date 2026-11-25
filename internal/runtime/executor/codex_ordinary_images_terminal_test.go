package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryImageTestTransport func(*http.Request) (*http.Response, error)

func (f ordinaryImageTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOrdinaryCodexImageEntrypoints(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload, upstream, want string
		code                                int
		stream                              bool
	}{
		{"generation", codexImagesGenerationsPath, `{"model":"gpt-image-2","prompt":"synthetic"}`, `{"data":[{"b64_json":"AA=="}]}`, "AA==", 200, false},
		{"edit", codexImagesEditsPath, `{"model":"gpt-image-2","prompt":"edit","images":[{"file_id":"sample"}]}`, `{"data":[{"b64_json":"BB=="}]}`, "BB==", 200, false},
		{"stream", codexImagesGenerationsPath, `{"model":"gpt-image-2","prompt":"synthetic"}`, "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"CC==\"}\n\n", "CC==", 200, true},
		{"busy", codexImagesGenerationsPath, `{"model":"gpt-image-2","prompt":"synthetic"}`, `{"error":{"code":"websocket_connection_limit_reached","message":"busy"}}`, "", 503, true},
		{"rate_limited", codexImagesGenerationsPath, `{"model":"gpt-image-2","prompt":"synthetic"}`, `{"error":{"code":"rate_limit_exceeded","message":"wait"}}`, "", 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": "https://upstream.invalid", "api_key": "fixture"}}
			calls := 0
			transport := ordinaryImageTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				accept := "application/json"
				if tc.stream {
					accept = "text/event-stream"
				}
				if req.Method != http.MethodPost || !strings.HasSuffix(tc.path, req.URL.Path) || req.Header.Get("Authorization") != "Bearer fixture" || req.Header.Get("Accept") != accept {
					t.Fatalf("upstream request method=%s path=%s Accept=%s", req.Method, req.URL.Path, req.Header.Get("Accept"))
				}
				payload, err := io.ReadAll(req.Body)
				if err != nil || !strings.Contains(string(payload), "prompt") || (tc.name == "edit" && !strings.Contains(string(payload), "sample")) {
					t.Fatalf("upstream body=%s; err=%v", payload, err)
				}
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Content-Type": {accept}, "Retry-After": {"19"}}, Body: io.NopCloser(strings.NewReader(tc.upstream)), Request: req}, nil
			})
			ctx := context.WithValue(context.Background(), ordinaryImageRoundTripperKey{}, transport)
			req := cliproxyexecutor.Request{Model: "gpt-image-2", Payload: []byte(tc.payload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(codexOpenAIImageSourceFormat), Stream: tc.stream, Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: tc.path}}
			var result string
			var err error
			if tc.stream {
				resultStream, execErr := executor.ExecuteStream(ctx, auth, req, opts)
				err = execErr
				if err == nil {
					for chunk := range resultStream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						result += string(chunk.Payload)
					}
				}
			} else {
				response, execErr := executor.Execute(ctx, auth, req, opts)
				err = execErr
				result = string(response.Payload)
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, err = %v", calls, err)
			}
			if tc.code == 200 {
				if err != nil || !strings.Contains(result, tc.want) {
					t.Fatalf("response=%q err=%v", result, err)
				}
			} else {
				status, ok := err.(statusErr)
				if !ok || status.StatusCode() != tc.code || status.RetryAfter() == nil || *status.RetryAfter() != 19*time.Second {
					t.Fatalf("error=%#v", err)
				}
			}
		})
	}
}

func TestOrdinaryCodexTerminalConnectionPressure(t *testing.T) {
	for _, tc := range []struct {
		fields string
		want   int
		delay  bool
	}{
		{"", 503, true}, {`,"status":400`, 400, true}, {`,"status":429`, 429, true}, {`,"status":503`, 503, true},
	} {
		payload := []byte(fmt.Sprintf(`{"type":"response.failed","response":{"error":{"code":"websocket_connection_limit_reached","message":"busy"}},"headers":{"Retry-After":"19"}%s}`, tc.fields))
		err, _, ok := codexTerminalFailureErr(payload)
		if !ok || err.StatusCode() != tc.want || err.RetryAfter() == nil || *err.RetryAfter() != 19*time.Second {
			t.Errorf("status data=%q: %#v, ok=%v", tc.fields, err, ok)
		}
	}
	e := newCodexStatusErrWithHeaders(http.StatusServiceUnavailable, http.Header{"Retry-After": {"invalid"}}, []byte(`{"error":{"code":"websocket_connection_limit_reached"}}`))
	if e.RetryAfter() != nil {
		t.Fatalf("invalid Retry-After accepted: %#v", e)
	}
}
