package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// TestNewProxyAwareHTTPClientBlocksAccountWithoutProxy verifies the global egress
// guard: an account-scoped request with no resolved proxy_url must never reach the
// network. The returned client's transport must fail every request before dialing.
func TestNewProxyAwareHTTPClientBlocksAccountWithoutProxy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached when proxy_url is missing")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ID: "acc-no-proxy"},
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
		t.Fatal("expected RoundTrip to fail for account without proxy_url, got nil error")
	}
}

// TestNewProxyAwareHTTPClientAllowsAccountWithProxy verifies a healthy account with
// a configured proxy_url still receives a working proxied transport (negative control).
func TestNewProxyAwareHTTPClientAllowsAccountWithProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ID: "acc-proxy", ProxyURL: "http://proxy.example.com:8080"},
		0,
	)

	if _, ok := client.Transport.(blockingRoundTripper); ok {
		t.Fatal("account with proxy_url must not get a blocking transport")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected a proxy function to be configured for the account proxy_url")
	}
}

// TestNewProxyAwareHTTPClientAllowsInfraCallWithoutProxy verifies that infrastructure
// calls (auth == nil), such as model registry updates, are not blocked even when no
// proxy is configured.
func TestNewProxyAwareHTTPClientAllowsInfraCallWithoutProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)

	if _, ok := client.Transport.(blockingRoundTripper); ok {
		t.Fatal("infrastructure call (auth == nil) must not be blocked")
	}
}

// TestNewProxyAwareHTTPClientAllowsInjectedContextRoundTripper verifies that an
// explicitly injected context RoundTripper is treated as a deliberate egress path
// and is not blocked by the missing-proxy guard (the account uses that transport
// instead of an accidental direct dialer).
func TestNewProxyAwareHTTPClientAllowsInjectedContextRoundTripper(t *testing.T) {
	t.Parallel()

	var injected http.RoundTripper = roundTripperFuncForTest(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", injected)

	client := NewProxyAwareHTTPClient(ctx, &config.Config{}, &cliproxyauth.Auth{ID: "acc-ctx-rt"}, 0)

	if _, ok := client.Transport.(blockingRoundTripper); ok {
		t.Fatal("account with an injected context RoundTripper must not get a blocking transport")
	}
	if client.Transport == nil {
		t.Fatal("expected the injected context RoundTripper to be wired as transport")
	}
}

type roundTripperFuncForTest func(*http.Request) (*http.Response, error)

func (f roundTripperFuncForTest) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestNewProxyAwareHTTPClientAllowsExplicitDirect verifies the explicit "direct"
// sentinel remains allowed: choosing direct egress is an intentional operator choice
// and must not trip the missing-proxy guard.
func TestNewProxyAwareHTTPClientAllowsExplicitDirect(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ID: "acc-direct", ProxyURL: "direct"},
		0,
	)

	if _, ok := client.Transport.(blockingRoundTripper); ok {
		t.Fatal("explicit direct account must not get a blocking transport")
	}
}

func TestNewProxyAwareHTTPClientRequestProxyOverridesAuthAndGlobal(t *testing.T) {
	t.Parallel()

	ctx := coreexecutor.WithRequestProxyURL(context.Background(), "http://request-proxy.example:8081")
	client := NewProxyAwareHTTPClient(
		ctx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"},
		0,
	)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport = %#v, want request proxy", client.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://upstream.example/v1", nil)
	if errReq != nil {
		t.Fatalf("request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://request-proxy.example:8081" {
		t.Fatalf("proxy URL = %v, want request proxy", proxyURL)
	}

	refreshCtx := coreexecutor.WithoutRequestProxyURL(ctx)
	refreshClient := NewProxyAwareHTTPClient(
		refreshCtx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"},
		0,
	)
	refreshTransport, ok := refreshClient.Transport.(*http.Transport)
	if !ok || refreshTransport.Proxy == nil {
		t.Fatalf("refresh transport = %#v, want auth proxy", refreshClient.Transport)
	}
	refreshProxy, errRefresh := refreshTransport.Proxy(req)
	if errRefresh != nil {
		t.Fatalf("refresh proxy: %v", errRefresh)
	}
	if refreshProxy == nil || refreshProxy.String() != "http://auth-proxy.example:8080" {
		t.Fatalf("refresh proxy URL = %v, want auth proxy", refreshProxy)
	}
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewDevinHTTPClient_ReusesTransportFromContext(t *testing.T) {
	baseTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)

	c1 := NewDevinHTTPClient(ctx, nil, nil, 0)
	c2 := NewDevinHTTPClient(ctx, nil, nil, 0)

	if c1.Transport != c2.Transport {
		t.Errorf("expected c1.Transport == c2.Transport across requests, got different pointers %p vs %p", c1.Transport, c2.Transport)
	}

	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c1.Transport)
	}
	if !tr.DisableCompression {
		t.Error("expected DisableCompression = true")
	}
}

func TestNewDevinHTTPClient_NonStandardRoundTripperDisablesGzip(t *testing.T) {
	var seenEncoding string
	customRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seenEncoding = req.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: 200}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", customRT)

	c := NewDevinHTTPClient(ctx, nil, nil, 0)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	_, _ = c.Transport.RoundTrip(req)

	if seenEncoding != "identity" {
		t.Errorf("expected Accept-Encoding: identity, got %q", seenEncoding)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
