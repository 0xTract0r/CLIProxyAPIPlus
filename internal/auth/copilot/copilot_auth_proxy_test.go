package copilot

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNewCopilotAuthWithProxyURL_OverrideTakesPrecedence verifies the
// per-account proxy override wins over the globally configured
// cfg.ProxyURL. Regression coverage for the bug where
// GitHubCopilotExecutor.Refresh and ensureAPIToken silently used the global
// proxy when validating / exchanging Copilot tokens on every business
// request.
func TestNewCopilotAuthWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewCopilotAuthWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://api.github.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://override.example.com:8081" {
		t.Fatalf("proxy URL = %v, want http://override.example.com:8081", proxyURL)
	}
}

// TestNewCopilotAuthWithProxyURL_EmptyOverrideFallsBackToCfg verifies the
// empty override falls back to cfg.ProxyURL, preserving the historical
// default behavior.
func TestNewCopilotAuthWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewCopilotAuthWithProxyURL(cfg, "")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://api.github.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}

// TestNewCopilotAuth_DefaultStillUsesCfgProxyURL guards backward
// compatibility for the legacy NewCopilotAuth ctor used by first-time login
// paths without auth context.
func TestNewCopilotAuth_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewCopilotAuth(cfg)

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://api.github.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}
