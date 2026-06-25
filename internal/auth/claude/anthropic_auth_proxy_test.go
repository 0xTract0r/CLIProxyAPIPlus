package claude

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNewClaudeAuthWithProxyURL_UsesUtlsRefreshTransport asserts the bare claude
// OAuth refresh client no longer uses a Go-default *http.Transport (the
// anti-correlation leak 03117a8e signature) but the serving uTLS refresh round
// tripper. The deep profile/proxy/strict assertions live in the helps package
// test (oauth_refresh_utls_client_test.go); from the auth package we can only
// observe that the transport is no longer the leaky stdlib transport.
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
