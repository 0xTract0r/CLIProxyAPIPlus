package qwen

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNewQwenAuthWithProxyURL_OverrideTakesPrecedence verifies that the
// per-account proxy override applied at Refresh time wins over a globally
// configured cfg.ProxyURL. This is the regression test for the bug where
// QwenExecutor.Refresh ignored auth.ProxyURL and dropped to cfg.ProxyURL.
func TestNewQwenAuthWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewQwenAuthWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://chat.qwen.ai", nil)
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

// TestNewQwenAuthWithProxyURL_EmptyOverrideFallsBackToCfg verifies that when
// the account does not specify a proxy override the constructor falls back to
// the global cfg.ProxyURL, preserving the historical default behavior.
func TestNewQwenAuthWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewQwenAuthWithProxyURL(cfg, "")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://chat.qwen.ai", nil)
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

// TestNewQwenAuth_DefaultStillUsesCfgProxyURL guards backward compatibility for
// the legacy NewQwenAuth ctor used by OAuth start paths without auth context.
func TestNewQwenAuth_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewQwenAuth(cfg)

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://chat.qwen.ai", nil)
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
