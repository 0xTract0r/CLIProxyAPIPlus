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
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryCodexTestRoundTripper func(*http.Request) (*http.Response, error)

func (f ordinaryCodexTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOrdinaryCodexHTTPEntpoints(t *testing.T) {
	for _, name := range []string{"non-stream", "stream", "429", "403", "terminal 503", "cancel", "truncated", "sdk other host"} {
		t.Run(name, func(t *testing.T) {
			a := &cliproxyauth.Auth{ID: "fixture-credential", Attributes: map[string]string{"base_url": "https://upstream.invalid/backend", "api_key": "fixture"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/backend/responses" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Accept") != "text/event-stream" {
					t.Fatalf("wrong upstream request %s", r.URL)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil || !ojson.GetBytesNoCopy(raw, "stream").Bool() || !strings.Contains(string(raw), "synthetic") || !ojson.GetBytesNoCopy(raw, "instructions").Exists() || ojson.GetBytesNoCopy(raw, "prompt_cache_key").Exists() {
					t.Fatalf("request body=%s err=%v", raw, err)
				}
				if name == "cancel" {
					cancel()
					return nil, r.Context().Err()
				}
				status := 200
				headers := http.Header{"Content-Type": {"text/event-stream"}}
				payload := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n"
				switch name {
				case "429", "403":
					status = 429
					if name == "403" {
						status = 403
					}
					headers.Set("Retry-After", "19")
					payload = `{"error":{"message":"refused"}}`
				case "terminal 503":
					payload += `data: {"type":"response.failed","status":503,"headers":{"Retry-After":"19"},"response":{"error":{"code":"websocket_connection_limit_reached","message":"busy"}}}` + "\n\n"
				case "truncated":
				default:
					payload += `data: {"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_fixture","role":"assistant","content":[{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
			})
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
			executor := NewCodexExecutor(&config.Config{})
			if name == "sdk other host" {
				r, _ := http.NewRequest(http.MethodGet, "https://other.invalid/", nil)
				_, err := executor.HttpRequest(ctx, a, r)
				var s statusErr
				if calls != 0 || !errors.As(err, &s) || s.StatusCode() != 403 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"synthetic","prompt_cache_key":"untrusted-shared"}`)}
			var wire strings.Builder
			var err error
			if name == "stream" || name == "terminal 503" || name == "truncated" {
				var out *cliproxyexecutor.StreamResult
				out, err = executor.ExecuteStream(ctx, a, req, opts)
				if err == nil {
					for ch := range out.Chunks {
						wire.Write(ch.Payload)
						if ch.Err != nil {
							err = ch.Err
						}
					}
				}
			} else {
				var out cliproxyexecutor.Response
				out, err = executor.Execute(ctx, a, req, opts)
				wire.Write(out.Payload)
			}
			if calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			switch name {
			case "429", "403", "terminal 503":
				want := 503
				if name == "429" {
					want = 429
				} else if name == "403" {
					want = 403
				}
				var s statusErr
				if !errors.As(err, &s) || s.StatusCode() != want {
					t.Fatalf("err=%#v", err)
				}
				if s.RetryAfter() == nil || *s.RetryAfter() != 19*time.Second {
					t.Fatalf("RetryAfter=%v", s.RetryAfter())
				}
			case "truncated":
				if err == nil {
					t.Fatal("truncated stream accepted")
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v", err)
				}
			default:
				if err != nil || !strings.Contains(wire.String(), "world") {
					t.Fatalf("wire=%q err=%v", wire.String(), err)
				}
			}
		})
	}
}

func TestOrdinaryCodexPromptCacheTenantIsolation(t *testing.T) {
	cache := func(tenant, token string) string {
		g, _ := gin.CreateTestContext(nil)
		if tenant != "" {
			g.Set("userApiKey", tenant)
		}
		ctx := context.WithValue(context.Background(), "gin", g)
		d := ojson.NewDocString(`{"prompt_cache_key":"shared"}`)
		a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": token, "base_url": "https://upstream.invalid"}}
		ordinaryCodexPromptCache(ctx, d, a, "gpt-5", cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		return d.Get("prompt_cache_key").String()
	}
	a := cache("tenant-a", "fixture")
	if len(a) != 64 || cache("tenant-a", "fixture") != a || cache("tenant-b", "fixture") == a || cache("tenant-a", "other") == a || cache("", "fixture") != "" {
		t.Fatal("cache partition is not stable or tenant scoped")
	}
}
