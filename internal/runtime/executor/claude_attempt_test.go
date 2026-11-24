package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryClaudeTrackedBody struct {
	io.Reader
	closings atomic.Int32
}

func (b *ordinaryClaudeTrackedBody) Close() error {
	b.closings.Add(1)
	return nil
}

func TestOrdinaryClaudeResponseBodyOwnedByEachAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, payload     string
		status            int
		stream, wantError bool
	}{
		{"success", `{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`, 200, false, false},
		{"invalid successful JSON", `{"partial":`, 200, false, true},
		{"normal refusal", `{"error":{"message":"fixture refused"}}`, 403, false, true},
		{"stream success", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", 200, true, false},
		{"stream incomplete", "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", 200, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &ordinaryClaudeTrackedBody{Reader: strings.NewReader(tc.payload)}
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {contentType}}, Body: body, Request: r}, nil
			}))
			a := &coreauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
			req := executorapi.Request{Model: "claude-fixture", Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)}
			opts := executorapi.Options{SourceFormat: translation.FormatClaude}
			e := NewClaudeExecutor(&config.Config{})
			var err error
			if tc.stream {
				var got *executorapi.StreamResult
				got, err = e.ExecuteStream(ctx, a, req, opts)
				if err == nil {
					for chunk := range got.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = e.Execute(ctx, a, req, opts)
			}
			if (err != nil) != tc.wantError || calls != 1 || body.closings.Load() != 1 {
				t.Fatalf("err=%v attempts=%d body closes=%d", err, calls, body.closings.Load())
			}
		})
	}
}
