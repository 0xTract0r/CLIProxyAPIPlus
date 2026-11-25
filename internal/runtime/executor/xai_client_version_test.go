package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestStandardXAIRequestOriginAndStatus(t *testing.T) {
	e := NewXAIExecutor(&config.Config{})
	auth := &coreauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "fixture-key", "base_url": "https://sdk.fixture.invalid/v1"}}
	var calls int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "sdk.fixture.invalid" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Fatal("unexpected SDK request")
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://other.fixture.invalid/next"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://sdk.fixture.invalid/v1/models", nil)
	resp, err := e.HttpRequest(ctx, auth, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 || resp.StatusCode != http.StatusTemporaryRedirect || req.Header.Get("Authorization") != "" {
		t.Fatal("request followed redirect or mutated caller")
	}
	for _, raw := range []string{"https://other.fixture.invalid/v1/models", "https://cli-chat-proxy.grok.com/v1/responses"} {
		r, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := e.HttpRequest(ctx, auth, r); err == nil {
			t.Fatal("unavailable destination was accepted")
		}
	}
	req.Host = "other.fixture.invalid"
	if _, err := e.HttpRequest(ctx, auth, req); err == nil || calls != 1 {
		t.Fatal("unexpected request authority")
	}
}

func TestStandardXAIWebsocketRejectsLegacyDestination(t *testing.T) {
	e := &XAIWebsocketsExecutor{}
	if _, _, _, err := e.dialXAIWebsocket(context.Background(), nil, "wss://cli-chat-proxy.grok.com/v1/responses", nil); err == nil {
		t.Fatal("unsupported destination passed")
	}
}
