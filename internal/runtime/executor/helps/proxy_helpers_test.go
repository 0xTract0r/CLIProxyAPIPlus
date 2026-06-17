package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
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
