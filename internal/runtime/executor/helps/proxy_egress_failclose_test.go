package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// TestNewUtlsRoundTripperBlocksInvalidProxyDialer verifies that a present-but-invalid
// proxy_url installs a fail-closed blocking dialer: the very first dial fails with
// ErrProxyEgressBlocked and returns no connection (proving zero I/O / zero direct
// dial). This is the anti-correlation guarantee: an invalid proxy must never fall
// through to a direct connection that would expose the real server IP.
func TestNewUtlsRoundTripperBlocksInvalidProxyDialer(t *testing.T) {
	t.Parallel()

	invalidProxies := []string{
		"ftp://1.2.3.4:1080",  // unsupported scheme
		"socks5://h:notaport", // malformed port
		"garbage",             // no scheme/host
		"http://ho%zzst:1080", // invalid percent-encoding
	}
	for _, proxyURL := range invalidProxies {
		proxyURL := proxyURL
		t.Run(proxyURL, func(t *testing.T) {
			t.Parallel()
			rt := newUtlsRoundTripper(proxyURL, tls.HelloChrome_133)
			conn, err := rt.dialer.Dial("tcp", "api.anthropic.com:443")
			if conn != nil {
				_ = conn.Close()
				t.Fatalf("expected nil conn (no dial) for invalid proxy %q", proxyURL)
			}
			if !errors.Is(err, proxyutil.ErrProxyEgressBlocked) {
				t.Fatalf("err = %v, want ErrProxyEgressBlocked for invalid proxy %q", err, proxyURL)
			}
		})
	}
}

// TestNewUtlsRoundTripperValidProxyDialerNotBlocked is the negative control: a valid
// socks5 proxy URL must NOT be treated as blocked. Dialing an unbound local proxy
// still fails, but with a real network error, never the fail-closed sentinel.
func TestNewUtlsRoundTripperValidProxyDialerNotBlocked(t *testing.T) {
	t.Parallel()

	rt := newUtlsRoundTripper("socks5://127.0.0.1:1080", tls.HelloChrome_133)
	conn, err := rt.dialer.Dial("tcp", "api.anthropic.com:443")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected a network error dialing an unbound local socks5 proxy")
	}
	if errors.Is(err, proxyutil.ErrProxyEgressBlocked) {
		t.Fatal("valid socks5 proxy must not be treated as fail-closed blocked")
	}
}

// TestNewProxyAwareHTTPClientBlocksInvalidProxy verifies the account-level guard now
// covers the invalid case too (previously only empty was blocked): a request with an
// invalid proxy_url must never reach the network.
func TestNewProxyAwareHTTPClientBlocksInvalidProxy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached when proxy_url is invalid")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ID: "acc-invalid-proxy", ProxyURL: "ftp://1.2.3.4:1080"},
		0,
	)

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, errDo := client.Transport.RoundTrip(req)
	if errDo == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected RoundTrip to fail for account with invalid proxy_url, got nil error")
	}
}

// TestNewUtlsHTTPClientForProfileBlocksAccountWithoutUsableProxy verifies the codex
// serving constructor now fails closed for BOTH empty and invalid account proxies
// (it previously had no empty guard at all): the transport blocks and RoundTrip
// returns ErrProxyEgressBlocked, and it is not a live fallbackRoundTripper.
func TestNewUtlsHTTPClientForProfileBlocksAccountWithoutUsableProxy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		proxy string
	}{
		{"empty", ""},
		{"invalid", "ftp://1.2.3.4:1080"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := NewUtlsHTTPClientForProfile(
				context.Background(),
				&config.Config{},
				&cliproxyauth.Auth{ID: "acc-" + tc.name, ProxyURL: tc.proxy},
				0,
				ClaudeCLIClientHelloProfileID,
			)
			if _, ok := client.Transport.(*fallbackRoundTripper); ok {
				t.Fatalf("blocked account must not get a live fallbackRoundTripper transport")
			}
			req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages", nil)
			resp, err := client.Transport.RoundTrip(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, proxyutil.ErrProxyEgressBlocked) {
				t.Fatalf("err = %v, want ErrProxyEgressBlocked", err)
			}
		})
	}
}

// TestNewUtlsHTTPClientForProfileAllowsInfraAndCtxRT verifies the guard's two
// exemptions: an infrastructure call (auth == nil) and an explicitly injected
// context RoundTripper are deliberate egress paths and must not be blocked.
func TestNewUtlsHTTPClientForProfileAllowsInfraAndCtxRT(t *testing.T) {
	t.Parallel()

	infra := NewUtlsHTTPClientForProfile(context.Background(), &config.Config{}, nil, 0, ClaudeCLIClientHelloProfileID)
	if _, ok := infra.Transport.(*fallbackRoundTripper); !ok {
		t.Fatalf("infra call (auth == nil) transport = %T, want *fallbackRoundTripper", infra.Transport)
	}

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))
	client := NewUtlsHTTPClientForProfile(ctx, &config.Config{}, &cliproxyauth.Auth{ID: "acc-ctx"}, 0, ClaudeCLIClientHelloProfileID)
	resp, err := client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("ctx RT client.Get error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("close body: %v", errClose)
	}
	if !called {
		t.Fatal("expected injected context RoundTripper to handle the request (account not blocked)")
	}
}
