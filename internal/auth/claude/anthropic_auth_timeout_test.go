package claude

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type contextBlockingRoundTripper struct{}

func (contextBlockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestExchangeCodeForTokensRespectsContextDeadline(t *testing.T) {
	pkceCodes, err := GeneratePKCECodes()
	if err != nil {
		t.Fatalf("generate pkce: %v", err)
	}

	auth := &ClaudeAuth{
		httpClient: &http.Client{Transport: contextBlockingRoundTripper{}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err = auth.ExchangeCodeForTokens(ctx, "test-code", "test-state", pkceCodes)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ExchangeCodeForTokens error = %v, want context deadline exceeded", err)
	}
}

func TestNewAnthropicHttpClientHasBoundedTimeout(t *testing.T) {
	client := NewAnthropicHttpClient(nil)
	if client.Timeout != anthropicHTTPClientTimeout {
		t.Fatalf("client timeout = %s, want %s", client.Timeout, anthropicHTTPClientTimeout)
	}
}

func TestClaudeOAuthConstructorsShareBoundedDefault(t *testing.T) {
	for _, client := range []*http.Client{NewAnthropicHttpClient(nil), NewClaudeAuth(nil).httpClient, NewClaudeAuthWithHTTPClient(nil).httpClient, NewClaudeAuthWithProxyURL(nil, "direct").httpClient} {
		if client.Timeout != 60*time.Second {
			t.Fatalf("default HTTP request timeout = %s, want 60s", client.Timeout)
		}
	}
	injected := &http.Client{Timeout: time.Second, Transport: contextBlockingRoundTripper{}}
	if auth := NewClaudeAuthWithHTTPClient(injected); auth.httpClient != injected {
		t.Fatal("caller-provided HTTP client was replaced")
	}
}

func TestClaudeOAuthClientTimeoutAndProfileCancellation(t *testing.T) {
	client := NewAnthropicHttpClient(nil)
	client.Timeout = 10 * time.Millisecond
	client.Transport = contextBlockingRoundTripper{}
	auth := NewClaudeAuthWithHTTPClient(client)
	if _, err := auth.FetchOAuthProfile(context.Background(), "fixture-token"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("profile total deadline error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := auth.FetchOAuthRoles(ctx, "fixture-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("roles canceled error = %v", err)
	}
}

func TestClaudeOAuthStandardTransportRespectsCanceledRequest(t *testing.T) {
	client := NewClaudeAuthWithProxyURL(nil, "direct").httpClient
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("unexpected dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("standard transport cancellation error = %v", err)
	}
}

func TestClaudeOAuthTransportBoundsWithZeroGlobalDefaults(t *testing.T) {
	original := http.DefaultTransport
	zeroDefaults := &http.Transport{}
	http.DefaultTransport = zeroDefaults
	defer func() { http.DefaultTransport = original }()
	for _, raw := range []string{"", "direct", "http://proxy.fixture.invalid:8080", "https://proxy.fixture.invalid", "socks5://proxy.fixture.invalid:1080"} {
		tr, ok := NewClaudeAuthWithProxyURL(nil, raw).httpClient.Transport.(*http.Transport)
		if !ok || tr.TLSHandshakeTimeout != 10*time.Second || tr.IdleConnTimeout != 90*time.Second {
			t.Fatalf("mode %q did not provide bounded TLS and idle timeouts", raw)
		}
	}
	if zeroDefaults.TLSHandshakeTimeout != 0 || zeroDefaults.IdleConnTimeout != 0 {
		t.Fatal("constructor mutated the global transport")
	}
}

func TestClaudeOAuthStandardTLSHandshakeTimeout(t *testing.T) {
	client := NewClaudeAuthWithProxyURL(nil, "direct").httpClient
	client.Timeout = time.Second
	transport := client.Transport.(*http.Transport)
	transport.TLSHandshakeTimeout = 10 * time.Millisecond
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		local, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return local, nil
	}
	resp, err := client.Get("https://fixture.invalid")
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("stalled in-memory TLS handshake unexpectedly succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("expected Go TLS handshake timeout, got %v", err)
	}
}
