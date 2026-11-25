package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCopilotSDKRequestPreservesExplicitHeaders(t *testing.T) {
	auth := copilotOrdinaryFixtureAuth(t, t.Name(), "gpt-5-codex", copilot.Chat)
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-Custom-Foo") != "fixture" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("explicit ordinary headers or selected credential were lost")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.individual.githubcopilot.com"+copilot.Chat, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Custom-Foo", "fixture")
	req.Header.Set("Accept", "application/json")
	e := NewCopilotExecutor(nil)
	resp, err := e.HttpRequest(ctx, auth, req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := e.HttpRequest(ctx, nil, req); err == nil {
		t.Error("nil authentication was accepted")
	}
	if req.Header.Get("Authorization") != "" || calls != 1 {
		t.Errorf("original request mutated or unexpected outbound requests: %d", calls)
	}
}

func TestOrdinaryCopilotInferenceConfiguredHeaders(t *testing.T) {
	const model = "claude-opus-5"
	auth := copilotOrdinaryFixtureAuth(t, t.Name(), model, copilot.Chat)
	auth.Attributes = map[string]string{
		"header:X-Custom-Foo":         "fixture",
		"header:Accept-Language":      "ja",
		"header:Authorization":        "ignored",
		"header:Host":                 "elsewhere.invalid",
		"header:X-Forwarded-Identity": "ignored",
		"header:X-Route-Override":     "ignored",
		"header:X-GitHub-Api-Version": "ignored",
		"header:X-Custom-Bad":         "bad\r\nheader",
	}
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.individual.githubcopilot.com" || r.URL.Path != copilot.Chat || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("configured header changed the selected authority, endpoint or credential")
		}
		if r.Header.Get("X-Custom-Foo") != "fixture" || r.Header.Get("Accept-Language") != "ja" {
			t.Error("ordinary operator-configured headers were dropped")
		}
		for _, h := range []string{"Host", "X-Forwarded-Identity", "X-Route-Override", "X-Custom-Bad"} {
			if r.Header.Get(h) != "" {
				t.Errorf("invalid/protected header %s forwarded", h)
			}
		}
		if r.Header.Get("X-GitHub-Api-Version") == "ignored" {
			t.Error("configured header changed the protocol contract")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion","id":"fixture","model":"claude-opus-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)), Request: r}, nil
	}))
	req := executorapi.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
	_, err := NewCopilotExecutor(nil).Execute(ctx, auth, req, executorapi.Options{SourceFormat: tr.FormatOpenAI})
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
