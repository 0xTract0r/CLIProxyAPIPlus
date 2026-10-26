package claude

import (
	"context"
	"errors"
	"net"
	"net/http"
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

type cancelledDialer struct{}

func (cancelledDialer) Dial(network, addr string) (net.Conn, error) {
	return nil, errors.New("dial should not be called after context cancellation")
}

func TestUTLSRoundTripperDoesNotDialAfterContextCancelled(t *testing.T) {
	transport := newUtlsRoundTripper(nil)
	transport.dialer = cancelledDialer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	_, err = transport.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context canceled", err)
	}
}
