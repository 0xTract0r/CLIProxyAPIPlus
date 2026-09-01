package claude

import (
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// TestNewAnthropicHttpClientFailsClosedOnInvalidProxy asserts the OAuth control-plane
// client fails closed on a present-but-invalid proxy_url: it must NOT fall back to
// the Go-default (direct) *http.Transport (which would expose the real server IP
// under the account's OAuth identity). The transport must block every request with
// ErrProxyEgressBlocked before any dial.
func TestNewAnthropicHttpClientFailsClosedOnInvalidProxy(t *testing.T) {
	t.Parallel()

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyURL: "ftp://1.2.3.4:1080"})
	if _, ok := client.Transport.(*http.Transport); ok {
		t.Fatal("invalid proxy must not fall back to a direct *http.Transport")
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/oauth/token", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, errDo := client.Transport.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(errDo, proxyutil.ErrProxyEgressBlocked) {
		t.Fatalf("err = %v, want ErrProxyEgressBlocked", errDo)
	}
}

// TestNewAnthropicHttpClientAllowsValidDirectAndEmpty is the negative control: a
// valid proxy, the explicit "direct" sentinel, and an empty proxy all keep the
// normal *http.Transport path (never fail-closed blocked).
func TestNewAnthropicHttpClientAllowsValidDirectAndEmpty(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"valid":  "http://proxy.example.com:8080",
		"direct": "direct",
		"empty":  "",
	}
	for name, proxyURL := range cases {
		name, proxyURL := name, proxyURL
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := NewAnthropicHttpClient(&config.SDKConfig{ProxyURL: proxyURL})
			if _, ok := client.Transport.(*http.Transport); !ok {
				t.Fatalf("%s proxy transport = %T, want *http.Transport (not blocked)", name, client.Transport)
			}
		})
	}
}
