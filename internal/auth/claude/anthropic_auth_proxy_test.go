package claude

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/net/proxy"
)

// TestNewClaudeAuthWithProxyURL_UsesUtlsRefreshTransport asserts the bare claude
// OAuth refresh client no longer uses a Go-default *http.Transport (the
// anti-correlation leak 03117a8e signature) but the native OAuth uTLS round tripper. Proxy and wire-profile assertions
// additionally cover the control-plane transport in this package.
func TestNewClaudeAuthWithProxyURL_UsesUtlsRefreshTransport(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://proxy.example.com:1080"}}
	auth := NewClaudeAuthWithProxyURL(cfg, "")

	if auth.httpClient == nil || auth.httpClient.Transport == nil {
		t.Fatal("expected non-nil refresh http client and transport")
	}
	if _, isStdlib := auth.httpClient.Transport.(*http.Transport); isStdlib {
		t.Fatal("claude refresh transport is *http.Transport (Go-default TLS); want serving uTLS refresh transport to avoid anti-correlation leak")
	}
}

// TestNewClaudeAuthWithProxyURL_DirectOverrideStillConstructs asserts the
// "direct" proxy override is accepted and yields a usable uTLS refresh client
// (the override is resolved to ModeDirect by proxyutil inside the round tripper's
// dialer). Proxy-vs-direct dialer correctness is asserted in the helps test.
func TestNewClaudeAuthWithProxyURL_DirectOverrideStillConstructs(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://proxy.example.com:1080"}}
	auth := NewClaudeAuthWithProxyURL(cfg, "direct")

	if auth.httpClient == nil || auth.httpClient.Transport == nil {
		t.Fatal("expected non-nil refresh http client and transport for direct override")
	}
	if _, isStdlib := auth.httpClient.Transport.(*http.Transport); isStdlib {
		t.Fatal("claude refresh (direct override) transport is *http.Transport; want serving uTLS refresh transport")
	}
}

func TestNewClaudeAuthWithProxyURL_OverrideDirectTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://proxy.example.com:1080"}}
	auth := NewClaudeAuthWithProxyURL(cfg, "direct")

	transport, ok := auth.httpClient.Transport.(*utlsRoundTripper)
	if !ok || transport == nil {
		t.Fatalf("expected utlsRoundTripper, got %T", auth.httpClient.Transport)
	}
	if transport.dialer != proxy.Direct {
		t.Fatalf("expected proxy.Direct, got %T", transport.dialer)
	}
}

func TestNewClaudeAuthWithProxyURL_OverrideProxyAppliedWithoutConfig(t *testing.T) {
	auth := NewClaudeAuthWithProxyURL(nil, "socks5://proxy.example.com:1080")

	transport, ok := auth.httpClient.Transport.(*utlsRoundTripper)
	if !ok || transport == nil {
		t.Fatalf("expected utlsRoundTripper, got %T", auth.httpClient.Transport)
	}
	if transport.dialer == proxy.Direct {
		t.Fatalf("expected proxy dialer, got %T", transport.dialer)
	}
}
