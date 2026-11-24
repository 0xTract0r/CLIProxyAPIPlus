package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryInventoryRT func(*http.Request) (*http.Response, error)

func (f ordinaryInventoryRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryInventorySSEFramingCompressionAndToolBinding(t *testing.T) {
	const data1 = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ordinary fixture"}}`
	const data2 = `{"type":"message_stop"}`
	expected := []string{"event: content_block_delta\ndata: " + data1 + "\n\n", "event: message_stop\ndata: " + data2 + "\n\n"}
	for _, name := range []string{"plain", "gzip header", "gzip magic", "gzip error nonstream", "gzip error stream", "invalid gzip nonstream", "invalid gzip stream"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			tr := ordinaryInventoryRT(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string]string{
					"tools.0.name": "read_file", "messages.1.content.0.id": "fixture-tool-id", "messages.2.content.0.tool_use_id": "fixture-tool-id",
				} {
					if got := ojson.GetBytesNoCopy(body, path).String(); got != want {
						t.Fatalf("%s=%q want=%q", path, got, want)
					}
				}
				header := http.Header{"Content-Type": {"text/event-stream"}}
				code := 200
				upstream := strings.Join(expected, "")
				if strings.Contains(name, "error") {
					code = 403
					upstream = `{"type":"error","error":{"message":"ordinary refusal"}}`
				}
				if strings.HasPrefix(name, "invalid") {
					code = 400
					upstream = "invalid gzip"
				}
				raw := []byte(upstream)
				if strings.HasPrefix(name, "gzip") {
					var compressed bytes.Buffer
					gz := gzip.NewWriter(&compressed)
					_, _ = gz.Write(raw)
					if e := gz.Close(); e != nil {
						t.Fatal(e)
					}
					raw = compressed.Bytes()
				}
				if name == "gzip header" || strings.HasPrefix(name, "invalid") {
					header.Set("Content-Encoding", "gzip")
				}
				return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", tr)
			e := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://configured.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","max_tokens":32,"prompt_cache_options":{"mode":"explicit"},"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"tool_use","id":"fixture-tool-id","name":"read_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"fixture-tool-id","content":"fixture"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			var result *cliproxyexecutor.StreamResult
			var err error
			if strings.HasSuffix(name, "nonstream") {
				_, err = e.Execute(ctx, auth, req, opts)
			} else {
				result, err = e.ExecuteStream(ctx, auth, req, opts)
			}
			if calls != 1 {
				t.Fatalf("upstream requests=%d", calls)
			}
			if name == "plain" || name == "gzip header" || name == "gzip magic" {
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					got = append(got, string(chunk.Payload))
				}
				if len(got) != len(expected) {
					t.Fatalf("events=%q want=%q", got, expected)
				}
				for i := range got {
					if got[i] != expected[i] {
						t.Fatalf("event %d=%q want=%q", i, got[i], expected[i])
					}
				}
			} else {
				var status statusErr
				want, message := 403, "ordinary refusal"
				if strings.HasPrefix(name, "invalid") {
					want, message = 400, "failed to decode error response body"
				}
				if !errors.As(err, &status) || status.StatusCode() != want || !strings.Contains(status.Error(), message) {
					t.Fatalf("error=%#v", err)
				}
			}
		})
	}
}
