package gitlab

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// TestNewAuthClientWithProxyURL_OverrideTakesPrecedence verifies the
// per-account proxy override applied at Refresh time wins over a globally
// configured cfg.ProxyURL for the GitLab OAuth refresh path. Regression
// coverage for the bug where GitLabExecutor.Refresh dropped to
// cfg.ProxyURL.
func TestNewAuthClientWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewAuthClientWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://gitlab.com", nil)
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

// TestNewAuthClientWithProxyURL_EmptyOverrideFallsBackToCfg verifies the
// empty override falls back to cfg.ProxyURL, preserving the historical
// default behavior.
func TestNewAuthClientWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewAuthClientWithProxyURL(cfg, "")

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://gitlab.com", nil)
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

// TestNewAuthClient_DefaultStillUsesCfgProxyURL guards backward
// compatibility for the legacy NewAuthClient ctor used by login paths
// without auth context.
func TestNewAuthClient_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewAuthClient(cfg)

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://gitlab.com", nil)
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
