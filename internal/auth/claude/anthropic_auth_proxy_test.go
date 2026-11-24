package claude

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNewClaudeAuthWithProxyURL_OverrideDirectTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example:8080"}}
	auth := NewClaudeAuthWithProxyURL(cfg, "direct")
	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected standard HTTP transport, got %T", auth.httpClient.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("direct override should bypass configured and environment proxies")
	}
}

func TestNewClaudeAuthWithProxyURL_OverrideProxyAppliedWithoutConfig(t *testing.T) {
	const configured = "http://proxy.example:8080"
	auth := NewClaudeAuthWithProxyURL(nil, configured)
	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected standard HTTP transport, got %T", auth.httpClient.Transport)
	}
	proxyURL, err := transport.Proxy(&http.Request{})
	if err != nil || proxyURL == nil || proxyURL.String() != configured {
		t.Fatalf("expected configured proxy, got %v, %v", proxyURL, err)
	}
}
