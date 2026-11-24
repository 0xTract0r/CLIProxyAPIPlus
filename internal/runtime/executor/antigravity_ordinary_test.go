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
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryAntigravityRT func(*http.Request) (*http.Response, error)

func (f ordinaryAntigravityRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryAntigravityProtocol(t *testing.T) {
	for _, name := range []string{"nonstream", "stream", "count", "429", "cancel", "refuse unsupported", "refuse unconfigured", "wrong SDK origin", "cut stream"} {
		t.Run(name, func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://configured.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
			cfg := &config.Config{}
			if name == "refuse unsupported" {
				cfg.Antigravity.SensitiveWords = []string{"fixture"}
			}
			if name == "refuse unconfigured" {
				delete(a.Attributes, "base_url")
			}
			calls := 0
			rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "configured.invalid" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("User-Agent") != "" {
					t.Fatalf("incorrect outbound headers or origin %s", r.URL)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), `"thoughtSignature":"existing-opaque-value"`) || !strings.Contains(string(body), `"name":"read"`) {
					t.Fatalf("tool or service signature changed %s", body)
				}
				if name == "cancel" {
					cancel()
					return nil, r.Context().Err()
				}
				status := 200
				headers := http.Header{"Content-Type": {"application/json"}}
				reply := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture result"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`
				switch name {
				case "stream", "cut stream":
					if r.URL.Path != antigravityStreamPath || r.URL.Query().Get("alt") != "sse" {
						t.Fatalf("SSE endpoint=%s", r.URL)
					}
					if name == "cut stream" {
						reply = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}`
					}
					reply = "data: " + reply + "\n\n"
					headers.Set("Content-Type", "text/event-stream")
				case "count":
					if r.URL.Path != antigravityCountTokensPath || ojson.GetBytesNoCopy(body, "project").Exists() || ojson.GetBytesNoCopy(body, "request.sessionId").Exists() {
						t.Fatalf("count request=%s body=%s", r.URL, body)
					}
					reply = `{"totalTokens":13}`
				default:
					if r.URL.Path != antigravityGeneratePath || ojson.GetBytesNoCopy(body, "project").String() != "fixture-project" {
						t.Fatalf("request=%s body=%s", r.URL, body)
					}
				}
				if name == "429" {
					status = 429
					headers.Set("Retry-After", "19")
					reply = `{"error":{"message":"fixture rejection"}}`
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
			})
			ctx := context.WithValue(base, "cliproxy.roundtripper", rt)
			e := NewAntigravityExecutor(cfg)
			if name == "wrong SDK origin" {
				req, _ := http.NewRequest(http.MethodGet, "https://other.invalid", nil)
				_, err := e.HttpRequest(ctx, a, req)
				var s statusErr
				if !errors.As(err, &s) || s.StatusCode() != 403 || calls != 0 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			req := cliproxyexecutor.Request{Model: "gemini-fixture", Payload: []byte(`{"request":{"sessionId":"fixture","contents":[{"role":"user","parts":[{"text":"read"}]},{"role":"model","parts":[{"functionCall":{"name":"read","id":"call-1","args":{}},"thoughtSignature":"existing-opaque-value"}]},{"role":"user","parts":[{"functionResponse":{"name":"read","id":"call-1","response":{"result":"ok"}}}]}]}}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("antigravity")}
			var payload []byte
			var err error
			switch name {
			case "stream", "cut stream":
				var result *cliproxyexecutor.StreamResult
				result, err = e.ExecuteStream(ctx, a, req, opts)
				if result != nil {
					for ch := range result.Chunks {
						payload = append(payload, ch.Payload...)
						if ch.Err != nil {
							err = ch.Err
						}
					}
				}
			case "count":
				var result cliproxyexecutor.Response
				result, err = e.CountTokens(ctx, a, req, opts)
				payload = result.Payload
			default:
				var result cliproxyexecutor.Response
				result, err = e.Execute(ctx, a, req, opts)
				payload = result.Payload
			}
			if name == "refuse unsupported" || name == "refuse unconfigured" {
				if err == nil || calls != 0 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			switch name {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v", err)
				}
			case "429":
				var s statusErr
				if !errors.As(err, &s) || s.StatusCode() != 429 || s.RetryAfter() == nil || *s.RetryAfter() != 19*time.Second {
					t.Fatalf("err=%#v", err)
				}
			case "cut stream":
				if err == nil || strings.Contains(string(payload), "[DONE]") {
					t.Fatalf("payload=%s err=%v", payload, err)
				}
			case "count":
				if err != nil || ojson.GetBytesNoCopy(payload, "totalTokens").Int() != 13 {
					t.Fatalf("payload=%s err=%v", payload, err)
				}
			default:
				if err != nil || !strings.Contains(string(payload), "fixture result") {
					t.Fatalf("payload=%s err=%v", payload, err)
				}
			}
		})
	}
}

func TestOrdinaryAntigravitySDKPreflightDoesNotSend(t *testing.T) {
	cfg := &config.Config{}
	e := NewAntigravityExecutor(cfg)
	a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://configured.invalid"}, Metadata: map[string]any{"access_token": "fixture", "refresh_token": "fixture"}}
	calls := 0
	rt := ordinaryAntigravityRT(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected upstream request")
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
	if !e.ShouldPrepareRequestAuth(a) {
		t.Fatal("preflight was not selected")
	}
	next, err := e.PrepareRequestAuth(ctx, a)
	if err != nil || next != a {
		t.Fatalf("preflight err=%v", err)
	}
	if _, err = e.Refresh(ctx, a); err == nil {
		t.Fatal("legacy refresh was not refused")
	}
	cfg.Antigravity.SensitiveWords = []string{"fixture"}
	if _, err = e.PrepareRequestAuth(ctx, a); err == nil {
		t.Fatal("legacy mode preflight was not refused")
	}
	if calls != 0 {
		t.Fatalf("unexpected upstream calls=%d", calls)
	}
}

func TestOrdinaryAntigravitySignatureChangeIsRejectedBeforeSend(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gemini-fixture", Protocol: "antigravity"}},
		Params: map[string]any{"contents.0.parts.0.thoughtSignature": "changed"},
	}}}}
	e := NewAntigravityExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://configured.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture-project"}}
	n := 0
	fake := ordinaryAntigravityRT(func(*http.Request) (*http.Response, error) {
		n++
		return nil, errors.New("unexpected upstream request")
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", fake)
	request := cliproxyexecutor.Request{Model: "gemini-fixture", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"input","thoughtSignature":"original-opaque"}]}]}}`)}
	_, err := e.Execute(ctx, auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("antigravity")})
	var s statusErr
	if n != 0 || !errors.As(err, &s) || s.StatusCode() != 400 || s.Error() != "Antigravity request changed an opaque service signature" {
		t.Fatalf("calls=%d err=%v", n, err)
	}
}
