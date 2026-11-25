package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeRetryBoundaryAcrossEntrypoints(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "count"} {
		for _, path := range []string{"before connection", "after send", "provider refusal"} {
			t.Run(mode+"/"+path, func(t *testing.T) {
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil || !strings.Contains(string(body), "fixture input") {
						t.Fatalf("upstream request lost the user's payload: %v", err)
					}
					response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r}
					if calls == 1 {
						switch path {
						case "before connection":
							return nil, &net.OpError{Op: "dial", Err: errors.New("synthetic connection failure")}
						case "after send":
							return nil, io.ErrUnexpectedEOF
						case "provider refusal":
							response.StatusCode = http.StatusTooManyRequests
							response.Body = io.NopCloser(strings.NewReader(`{"error":{"message":"ordinary rate limit"}}`))
							return response, nil
						}
					}
					payload := `{"id":"fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"ok"}]}`
					switch mode {
					case "count":
						if r.URL.Path != "/v1/messages/count_tokens" {
							t.Errorf("count reached %q", r.URL.Path)
						}
						payload = `{"input_tokens":15}`
					case "stream":
						response.Header.Set("Content-Type", "text/event-stream")
						payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-fixture\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					response.Body = io.NopCloser(strings.NewReader(payload))
					return response, nil
				}))
				base := "https://fixture.invalid"
				if mode == "count" {
					base = "https://api.anthropic.com"
				}
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": base}}
				e := NewClaudeExecutor(&config.Config{})
				req := cliproxyexecutor.Request{Model: "claude-fixture", Payload: []byte(`{"max_tokens":10,"messages":[{"role":"user","content":"fixture input"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				var err error
				switch mode {
				case "sync":
					_, err = e.Execute(ctx, auth, req, opts)
				case "count":
					_, err = e.CountTokens(ctx, auth, req, opts)
				case "stream":
					var r *cliproxyexecutor.StreamResult
					r, err = e.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for ch := range r.Chunks {
							if ch.Err != nil {
								err = ch.Err
							}
						}
					}
				}
				if path == "before connection" {
					if err != nil || calls != 2 {
						t.Fatalf("pre-send fallback produced %v after %d attempts", err, calls)
					}
				} else {
					status := http.StatusBadGateway
					if path == "provider refusal" {
						status = http.StatusTooManyRequests
					}
					var failure statusErr
					if !errors.As(err, &failure) || failure.StatusCode() != status || calls != 1 {
						t.Fatalf("error=%v calls=%d", err, calls)
					}
				}
			})
		}
	}
}
