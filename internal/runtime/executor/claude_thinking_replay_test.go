package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeRefusedRequestDoesNotChangeNextRequest(t *testing.T) {
	for _, mode := range []string{"nonstream", "stream"} {
		t.Run(mode, func(t *testing.T) {
			const opaque = "c3ludGhldGljLW9wYXF1ZS1maXh0dXJl"
			upstream := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				select {
				case upstream <- body:
				default:
					http.Error(w, "extra request", http.StatusInternalServerError)
					return
				}
				if len(ojson.GetBytes(body, "messages").Array()) > 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"fixture refusal"}}`)
					return
				}
				if mode == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "claude", ProxyURL: "direct",
				Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL},
			}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: mode == "stream"}
			call := func(ctx context.Context, payload string) error {
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}
				if mode == "stream" {
					response, err := executor.ExecuteStream(ctx, auth, req, options)
					if err != nil {
						return err
					}
					var output strings.Builder
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
						output.Write(chunk.Payload)
					}
					if strings.Count(output.String(), `"type":"message_stop"`) != 1 {
						t.Fatal("stream did not complete exactly once")
					}
					return nil
				}
				_, err := executor.Execute(ctx, auth, req, options)
				return err
			}
			wire := func() ojson.Result {
				t.Helper()
				select {
				case body := <-upstream:
					return ojson.ParseBytes(body)
				default:
					t.Fatal("expected a loopback request")
					return ojson.Result{}
				}
			}

			first := `{"max_tokens":64,"messages":[{"role":"user","content":"previous request"},{"role":"assistant","content":[{"type":"thinking","thinking":"caller supplied","signature":"` + opaque + `"},{"type":"text","text":"previous answer"}]},{"role":"user","content":"refused turn"}]}`
			var status interface{ StatusCode() int }
			if err := call(t.Context(), first); !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "fixture refusal") {
				t.Fatalf("expected upstream 400, got %v", err)
			}
			if got := wire().Get("messages.1.content.0.signature").String(); got != opaque || len(upstream) != 0 {
				t.Fatal("caller opaque value changed or refused request ran again")
			}
			if err := call(t.Context(), `{"max_tokens":64,"messages":[{"role":"user","content":"independent request"}]}`); err != nil {
				t.Fatal(err)
			}
			fresh := wire()
			if len(fresh.Get("messages").Array()) != 1 || !strings.Contains(fresh.Get("messages.0.content").Raw, "independent request") ||
				strings.Contains(fresh.Raw, opaque) || strings.Contains(fresh.Raw, "previous answer") || len(upstream) != 0 {
				t.Fatal("independent request carried earlier state")
			}
		})
	}
}
