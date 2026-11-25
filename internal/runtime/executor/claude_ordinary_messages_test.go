package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryClaudeTestRoundTripper func(*http.Request) (*http.Response, error)

func (f ordinaryClaudeTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOrdinaryClaudeMessagesStream(t *testing.T) {
	for _, name := range []string{"API key", "Bearer", "upstream 429", "SSE overload", "truncated", "cancel", "redirect", "text invoke"} {
		t.Run(name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://upstream.invalid", "api_key": "fixture"}}
			if name == "Bearer" {
				delete(auth.Attributes, "api_key")
				auth.Metadata = map[string]any{"access_token": "fixture"}
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/messages" || r.Method != "POST" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Accept") != "text/event-stream" {
					t.Fatalf("request=%s %s", r.Method, r.URL)
				}
				if name == "Bearer" {
					if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Api-Key") != "" {
						t.Fatal("unexpected Bearer headers")
					}
				} else if r.Header.Get("X-Api-Key") != "fixture" || r.Header.Get("Authorization") != "" {
					t.Fatal("unexpected API key headers")
				}
				b, err := io.ReadAll(r.Body)
				if err != nil || !ojson.GetBytesNoCopy(b, "stream").Bool() || ojson.GetBytesNoCopy(b, "system").String() != "synthetic system" || ojson.GetBytesNoCopy(b, "max_tokens").Int() != 24 || ojson.GetBytesNoCopy(b, "messages.0.content").String() != "hello" {
					t.Fatalf("body=%s err=%v", b, err)
				}
				if name == "cancel" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if name == "redirect" {
					return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://other.invalid/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				code := 200
				reply := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"model\":\"claude-test\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n"
				switch name {
				case "text invoke":
					reply += "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"<invoke name='test'>value</invoke>\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				case "upstream 429":
					code = 429
					reply = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
				case "SSE overload":
					reply += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
				case "truncated":
				default:
					reply += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": {"11"}, "Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
			})
			ctx := context.WithValue(base, "cliproxy.roundtripper", transport)
			if name == "cancel" {
				timer := time.AfterFunc(20*time.Millisecond, cancel)
				defer timer.Stop()
			}
			e := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "claude-test", Payload: []byte(`{"model":"claude-test","system":"synthetic system","prompt_cache_options":{"mode":"explicit"},"max_tokens":24,"messages":[{"role":"user","content":"hello"}]}`)}
			result, err := e.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Stream: true})
			switch name {
			case "cancel":
				if err == nil || !strings.Contains(err.Error(), "context canceled") {
					t.Fatalf("err=%v", err)
				}
				return
			case "redirect":
				s, ok := err.(statusErr)
				if !ok || s.StatusCode() != 307 {
					t.Fatalf("err=%#v", err)
				}
				return
			case "upstream 429":
				s, ok := err.(statusErr)
				if !ok || s.StatusCode() != 429 || s.RetryAfter() == nil || *s.RetryAfter() != 11*time.Second || !strings.Contains(s.Error(), "slow down") {
					t.Fatalf("err=%#v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var wire strings.Builder
			var terminal error
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					terminal = chunk.Err
				} else {
					wire.Write(chunk.Payload)
				}
			}
			if !strings.Contains(wire.String(), "world") {
				t.Fatalf("wire=%q", wire.String())
			}
			if name == "text invoke" && (!strings.Contains(wire.String(), "<invoke name='test'>value</invoke>") || strings.Contains(wire.String(), `"type":"tool_use"`)) {
				t.Fatalf("invoke was rewritten: %s", wire.String())
			}
			switch name {
			case "truncated", "SSE overload":
				s, ok := terminal.(statusErr)
				want := 502
				if name == "SSE overload" {
					want = 529
				}
				if !ok || s.StatusCode() != want {
					t.Fatalf("terminal=%#v", terminal)
				}
			default:
				if terminal != nil || !strings.Contains(wire.String(), "event: message_stop\ndata:") {
					t.Fatalf("wire=%q error=%v", wire.String(), terminal)
				}
			}
		})
	}
}

func TestOrdinaryClaudeBeforeSendRetry(t *testing.T) {
	for _, name := range []string{"dial", "connected", "read", "refusal"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			rt := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					switch name {
					case "dial":
						return nil, &net.OpError{Op: "dial", Err: errors.New("offline fixture")}
					case "connected":
						httptrace.ContextClientTrace(r.Context()).GotConn(httptrace.GotConnInfo{})
						return nil, &net.OpError{Op: "dial", Err: errors.New("offline fixture")}
					case "read":
						return nil, io.EOF
					case "refusal":
						return &http.Response{StatusCode: 429, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
					}
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "fixture-body" {
					t.Fatalf("replayed body=%q", body)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
			})
			req, _ := http.NewRequest(http.MethodPost, "https://upstream.invalid", strings.NewReader("fixture-body"))
			resp, err := doOrdinaryClaudeMessagesRequest(&http.Client{Transport: rt}, req)
			if resp != nil {
				defer resp.Body.Close()
			}
			want := 1
			if name == "dial" {
				want = 2
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("response=%v err=%v", resp, err)
				}
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}

func TestOrdinaryClaudeMessagesStreamCompaction(t *testing.T) {
	calls := 0
	rt := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/messages" || r.Header.Get("Accept") != "application/json" || ojson.GetBytesNoCopy(body, "stream").Bool() || !strings.Contains(string(body), "Please provide a concise and comprehensive summary") {
			t.Fatalf("invalid summary request %s %s err=%v", r.URL, body, err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_summary","type":"message","role":"assistant","content":[{"type":"text","text":"The work is ready."}],"usage":{"input_tokens":10,"output_tokens":5}}`)), Request: r}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
	model := "claude-haiku-4-5-20251001"
	body := []byte(`{"model":"claude-haiku-4-5-20251001","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Summarize work"}]},{"type":"compaction_trigger"}]}`)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://upstream.invalid", "api_key": "fixture"}}
	e := NewClaudeExecutor(&config.Config{})
	got, err := e.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	for c := range got.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		stream.Write(c.Payload)
	}
	if calls != 1 || !strings.Contains(stream.String(), "response.completed") || !strings.Contains(stream.String(), "encrypted_content") {
		t.Fatalf("calls=%d stream=%q", calls, stream.String())
	}
}
