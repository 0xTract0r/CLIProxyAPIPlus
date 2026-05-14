package iflow

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// TestNewIFlowAuthWithProxyURL_OverrideTakesPrecedence verifies that the
// per-account proxy override applied at Refresh time wins over a globally
// configured cfg.ProxyURL. This is the regression test for the bug where
// IFlowExecutor.Refresh ignored auth.ProxyURL and dropped to cfg.ProxyURL.
func TestNewIFlowAuthWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewIFlowAuthWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://iflow.cn", nil)
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

// TestNewIFlowAuthWithProxyURL_EmptyOverrideFallsBackToCfg verifies that when
// the account does not specify a proxy override the constructor falls back to
// the global cfg.ProxyURL, preserving the historical default behavior.
func TestNewIFlowAuthWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewIFlowAuthWithProxyURL(cfg, "")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://iflow.cn", nil)
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

// TestNewIFlowAuth_DefaultStillUsesCfgProxyURL guards backward compatibility
// for the legacy NewIFlowAuth ctor used by OAuth start paths without auth
// context.
func TestNewIFlowAuth_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewIFlowAuth(cfg)

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://iflow.cn", nil)
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
