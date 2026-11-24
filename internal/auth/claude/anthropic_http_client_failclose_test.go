package claude

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNewAnthropicHttpClientFailsClosedOnInvalidProxy(t *testing.T) {
	for _, raw := range []string{"file://fixture.invalid", "http://:8080", "http://fixture.invalid:99999", "http://user:synthetic-secret@fixture.invalid:0", "http://[invalid"} {
		for _, client := range []*http.Client{
			NewAnthropicHttpClient(&config.SDKConfig{ProxyURL: raw}),
			NewClaudeAuthWithProxyURL(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: "direct"}}, raw).httpClient,
			NewClaudeAuth(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: raw}}).httpClient,
		} {
			if _, ok := client.Transport.(rejectedClaudeOAuthProxy); !ok {
				t.Fatalf("invalid proxy installed active transport %T", client.Transport)
			}
			req, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/oauth/token", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(req)
			if response != nil || !errors.Is(err, errInvalidClaudeOAuthProxy) || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("invalid proxy must fail locally with a sanitized configuration error")
			}
		}
	}
}

func TestNewAnthropicHttpClientAllowsValidDirectAndEmpty(t *testing.T) {
	for _, raw := range []string{"", "direct", "none", "http://proxy.fixture.invalid:8080", "https://proxy.fixture.invalid", "socks5://proxy.fixture.invalid:1080", "socks5h://proxy.fixture.invalid:1080"} {
		tr, ok := NewAnthropicHttpClient(&config.SDKConfig{ProxyURL: raw}).Transport.(*http.Transport)
		if !ok {
			t.Fatalf("valid mode %q should install standard HTTP transport", raw)
		}
		if (raw == "direct" || raw == "none") && tr.Proxy != nil {
			t.Fatal("explicit direct mode should omit proxy callback")
		}
	}
}

func TestClaudeOAuthProxyConfigErrorAtExchange(t *testing.T) {
	auth := NewClaudeAuthWithProxyURL(nil, "http://:8080")
	_, err := auth.ExchangeCodeForTokens(context.Background(), "fixture-code", "fixture-state", &PKCECodes{CodeVerifier: "fixture-verifier"})
	if !errors.Is(err, errInvalidClaudeOAuthProxy) {
		t.Fatalf("exchange result: %v", err)
	}
	_, err = auth.FetchOAuthProfile(context.Background(), "fixture-token")
	if !errors.Is(err, errInvalidClaudeOAuthProxy) {
		t.Fatalf("profile result: %v", err)
	}
}
