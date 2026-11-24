package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeContinuesExplicitThreadAfterContentRefusal(t *testing.T) {
	const beta = "message-threads-2026-08-12"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			e := NewClaudeExecutor(&config.Config{})
			a := &coreauth.Auth{ID: t.Name(), Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture-key"}}
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Anthropic-Beta") != beta || r.Header.Get("X-Api-Key") != "fixture-key" {
					t.Fatal("thread request lost the explicit beta or selected credential")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				doc := ojson.NewDoc(raw)
				headers := http.Header{"Content-Type": {"application/json"}}
				body := `{"id":"msg_first","model":"claude-opus-5","type":"message","role":"assistant","content":[{"type":"text","text":"fixture refusal"}],"stop_reason":"refusal"}`
				if calls == 1 {
					if doc.Get("thread.type").String() != "create" {
						t.Fatal("explicit thread creation lost")
					}
					if stream {
						headers.Set("Content-Type", "text/event-stream")
						body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_first\",\"model\":\"claude-opus-5\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				} else {
					if doc.Get("thread.type").String() != "continue" || doc.Get("thread.previous_message_id").String() != "msg_first" {
						t.Fatal("explicit thread continuation lost")
					}
					body = `{"id":"msg_second","model":"claude-opus-5","type":"message","role":"assistant","content":[{"type":"text","text":"continued"}],"stop_reason":"end_turn"}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			opts := executorapi.Options{SourceFormat: translation.FormatClaude, Headers: http.Header{"Anthropic-Beta": {beta}}}
			req := executorapi.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":24,"thread":{"type":"create"},"messages":[{"role":"user","content":"first"}]}`)}
			if stream {
				got, err := e.ExecuteStream(ctx, a, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range got.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(ctx, a, req, opts); err != nil {
				t.Fatal(err)
			}
			req.Payload = []byte(`{"max_tokens":24,"thread":{"type":"continue","previous_message_id":"msg_first"},"messages":[{"role":"user","content":"second"}]}`)
			if got, err := e.Execute(ctx, a, req, opts); err != nil || calls != 2 || ojson.GetBytesNoCopy(got.Payload, "id").String() != "msg_second" {
				t.Fatalf("continuation failed: attempts=%d err=%v", calls, err)
			}
		})
	}
}
