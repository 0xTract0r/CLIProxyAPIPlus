package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCodexBootstrapCommitAndUpdates(t *testing.T) {
	for _, name := range []string{"before commit", "after commit", "complete once"} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Codex.StreamBootstrapBuffering = true
			calls := 0
			rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), `"type":"configuration_update"`) || !strings.Contains(string(body), `"name":"ordinary_fixture_tool"`) || ojson.GetBytesNoCopy(body, "instructions").String() != "ordinary original instructions" {
					t.Fatalf("request extension lost: %s", body)
				}
				text := `data: {"type":"response.created","response":{"id":"resp_fixture","object":"response","model":"gpt-5","status":"in_progress"}}` + "\n\n"
				if name != "before commit" {
					text += `data: {"type":"response.output_text.delta","delta":"content-once","item_id":"msg_1","output_index":0,"content_index":0}` + "\n\n"
				}
				if name == "complete once" {
					text += `data: {"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5","output":[],"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n"
				} else {
					text += `data: {"type":"response.failed","status":429,"headers":{"Retry-After":"19"},"error":{"message":"fixture refusal"}}` + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(text)), Request: r}, nil
			})
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://provider.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","instructions":"ordinary original instructions","input":[{"type":"configuration_update","tools":[{"type":"function","name":"ordinary_fixture_tool","parameters":{"type":"object"}}]},{"role":"user","content":"fixture"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			result, err := NewCodexExecutor(cfg).ExecuteStream(context.WithValue(t.Context(), "cliproxy.roundtripper", rt), a, req, opts)
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
			var wire strings.Builder
			if name == "before commit" && result != nil {
				t.Fatal("precommit refusal published a stream")
			}
			if result != nil {
				if err != nil {
					t.Fatal(err)
				}
				for ch := range result.Chunks {
					wire.Write(ch.Payload)
					if ch.Err != nil {
						err = ch.Err
					}
				}
			}
			if name == "complete once" {
				if err != nil || strings.Count(wire.String(), "content-once") != 1 || strings.Count(wire.String(), `"type":"response.completed"`) != 1 {
					t.Fatalf("wire=%s err=%v", wire.String(), err)
				}
				return
			}
			var status statusErr
			if !errors.As(err, &status) || status.StatusCode() != 429 || status.RetryAfter() == nil || *status.RetryAfter() != 19*time.Second {
				t.Fatalf("err=%#v", err)
			}
			if name == "after commit" && strings.Count(wire.String(), "content-once") != 1 {
				t.Fatalf("wire=%s", wire.String())
			}
		})
	}
}
