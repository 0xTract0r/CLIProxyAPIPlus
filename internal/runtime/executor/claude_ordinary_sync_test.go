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

type ordinarySyncTransport func(*http.Request) (*http.Response, error)

func (f ordinarySyncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryClaudeSyncEntrypoints(t *testing.T) {
	const maxCountResponseBytes = 64 << 10
	for _, name := range []string{"native tool", "bearer", "permission denied", "policy refusal", "request too large", "malformed response", "count remote", "count rejected", "count response too large", "count local", "count invalid role", "cancel"} {
		t.Run(name, func(t *testing.T) {
			count := strings.HasPrefix(name, "count")
			local := name == "count local"
			code := 200
			body := `{"model":"claude-haiku-4-5-20251001","max_tokens":24,"system":"fixture system","metadata":{"user_id":"fixture"},"prompt_cache_options":{"mode":"explicit"},"messages":[{"role":"user","content":"fixture"}],"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"weather"}}`
			if name == "request too large" {
				body = " "
				body += strings.Repeat(" ", ordinaryClaudeMaxRequestBytes)
				body += `{"messages":[]}`
			}
			if name == "count invalid role" {
				body = strings.Replace(body, `"role":"user"`, `"role":"system"`, 1)
			}
			apiBase := "https://api.anthropic.com"
			if !count || local {
				apiBase = "https://upstream.invalid"
			}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": apiBase}}
			if name == "bearer" {
				delete(auth.Attributes, "api_key")
				auth.Metadata = map[string]any{"access_token": "fixture"}
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			rt := ordinarySyncTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				urlPath := "/v1/messages"
				if count {
					urlPath += "/count_tokens"
				}
				if r.URL.Path != urlPath || r.Header.Get("Accept") != "application/json" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
					t.Fatalf("upstream %s %#v", r.URL, r.Header)
				}
				if name == "bearer" {
					if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Api-Key") != "" {
						t.Fatal("invalid bearer")
					}
				} else if r.Header.Get("X-Api-Key") != "fixture" || r.Header.Get("Authorization") != "" {
					t.Fatal("invalid api key")
				}
				bytes, err := io.ReadAll(r.Body)
				if err != nil || ojson.GetBytesNoCopy(bytes, "system").String() != "fixture system" || ojson.GetBytesNoCopy(bytes, "tools.0.name").String() != "weather" || ojson.GetBytesNoCopy(bytes, "stream").Bool() {
					t.Fatalf("upstream payload=%s err=%v", bytes, err)
				}
				if count && (ojson.GetBytesNoCopy(bytes, "metadata").Exists() || ojson.GetBytesNoCopy(bytes, "max_tokens").Exists() || ojson.GetBytesNoCopy(bytes, "stream").Exists()) {
					t.Fatalf("count request fields=%s", bytes)
				}
				if name == "cancel" {
					cancel()
					return nil, r.Context().Err()
				}
				response := `{"id":"msg_fixture","type":"message","model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"tool_use","id":"tool_fixture","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":26,"output_tokens":9}}`
				if count {
					response = `{"input_tokens":26}`
				}
				switch name {
				case "permission denied", "count rejected":
					code = 403
					response = `{"type":"error","error":{"type":"permission_error","message":"refused"}}`
				case "policy refusal":
					response = `{"id":"msg_fixture","type":"message","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","explanation":"fixture"}}`
				case "malformed response":
					response = `{"content":`
				case "count response too large":
					response = strings.Repeat(" ", maxCountResponseBytes) + response
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"13"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})
			ctx := context.WithValue(base, "cliproxy.roundtripper", rt)
			e := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "claude-haiku-4-5-20251001", Payload: []byte(body)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			var got cliproxyexecutor.Response
			var err error
			if count {
				got, err = e.CountTokens(ctx, auth, req, opts)
			} else {
				got, err = e.Execute(ctx, auth, req, opts)
			}
			wantCalls := 1
			switch name {
			case "request too large", "count local", "count invalid role":
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls=%d want=%d err=%v", calls, wantCalls, err)
			}
			switch name {
			case "permission denied", "count rejected":
				var status statusErr
				if !errors.As(err, &status) || status.StatusCode() != 403 || status.RetryAfter() == nil || *status.RetryAfter() != 13*time.Second || !strings.Contains(status.Error(), "refused") {
					t.Fatalf("err=%#v", err)
				}
			case "count invalid role", "policy refusal", "request too large", "malformed response", "count response too large":
				var status interface{ StatusCode() int }
				want := 400
				if name == "request too large" {
					want = 413
				}
				if name == "malformed response" || name == "count response too large" {
					want = 502
				}
				if !errors.As(err, &status) || status.StatusCode() != want {
					t.Fatalf("err=%#v want=%d", err, want)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%#v", err)
				}
			case "count remote", "count local":
				n := ojson.GetBytesNoCopy(got.Payload, "input_tokens").Int()
				if err != nil || n < 1 || (!local && n != 26) {
					t.Fatalf("payload=%s err=%v", got.Payload, err)
				}
			default:
				if err != nil || ojson.GetBytesNoCopy(got.Payload, "content.0.input.city").String() != "Paris" || ojson.GetBytesNoCopy(got.Payload, "stop_reason").String() != "tool_use" {
					t.Fatalf("payload=%s err=%v", got.Payload, err)
				}
			}
		})
	}
}
